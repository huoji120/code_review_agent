package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"code-review-agent/internal/llm"
)

type noticeClient struct {
	call  func(context.Context, []llm.Message) (string, error)
	tools func(context.Context, []llm.Message) (llm.ToolResponse, error)
}

func (c *noticeClient) Chat(ctx context.Context, messages []llm.Message) (string, error) {
	return c.call(ctx, messages)
}
func (c *noticeClient) ChatStream(ctx context.Context, messages []llm.Message, emit func(llm.Delta) error) error {
	text, err := c.call(ctx, messages)
	if err != nil {
		return err
	}
	return emit(llm.Delta{Content: text})
}

func (c *noticeClient) ChatTools(ctx context.Context, messages []llm.Message, _ []llm.ToolDefinition, emit func(llm.Delta) error) (llm.ToolResponse, error) {
	if c.tools == nil {
		return llm.ToolResponse{}, fmt.Errorf("native fixture callback missing")
	}
	response, err := c.tools(ctx, messages)
	if err != nil {
		return llm.ToolResponse{}, err
	}
	if emit != nil {
		if err := emit(llm.Delta{Content: response.Content, Thinking: response.Thinking}); err != nil {
			return llm.ToolResponse{}, err
		}
	}
	return response, nil
}

func noticeMessages(messages []llm.Message) []string {
	var notices []string
	for _, m := range messages {
		if strings.HasPrefix(m.Content, "Forum notification:\n") {
			notices = append(notices, m.Content)
		}
	}
	return notices
}

func TestAutomaticNoticeDuringNormalFileToolsPreservesBuffer(t *testing.T) {
	client := &noticeClient{}
	team := newTestTeam(t, client)
	team.createStageLocked(phaseRecon)
	a := team.workers[0].agent
	a.cfg.Agent.MaxTurns = 3
	step := 0
	var bufferID string
	client.tools = func(ctx context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		step++
		switch step {
		case 1:
			if _, err := a.board.Post("user", "user", "*", 0, "new evidence", strings.Repeat("literal evidence ", 500)); err != nil {
				t.Fatal(err)
			}
			return toolReply("read_file", map[string]any{"path": "entry1.go", "offset": 1, "limit": 10}), nil
		case 2:
			notices := noticeMessages(messages)
			if len(notices) != 1 || len(notices[0]) > 1024 || !strings.Contains(notices[0], "new evidence") || strings.Contains(notices[0], strings.Repeat("literal evidence ", 500)) {
				t.Fatal("next read-file turn did not receive bounded notice")
			}
			for _, m := range messages {
				if m.Type == "function_call_output" {
					var result struct {
						BufferID string `json:"buffer_id"`
					}
					if err := json.Unmarshal([]byte(m.Content), &result); err != nil {
						t.Fatal(err)
					}
					bufferID = result.BufferID
				}
			}
			if bufferID == "" {
				t.Fatal("missing oversized file buffer")
			}
			return toolReply("read_tool_buffer", map[string]any{"buffer_id": bufferID, "offset": 0, "limit": 128}), nil
		case 3:
			if len(noticeMessages(messages)) != 1 {
				t.Fatal("quiet turn duplicated notice")
			}
			last := messages[len(messages)-1].Content
			if messages[len(messages)-1].Type != "function_call_output" || !strings.Contains(last, `"ok":true`) {
				t.Fatalf("notice invalidated buffer: %s", last)
			}
			return toolReply("read_tool_buffer", map[string]any{"buffer_id": bufferID, "offset": 0, "limit": 64}), nil
		}
		return llm.ToolResponse{}, fmt.Errorf("unexpected model request")
	}
	a.Run(context.Background(), "inspect", func(Event) {})
	if step != 3 || bufferID == "" {
		t.Fatalf("normal worker path not exercised: %d", step)
	}
}

func TestNotificationCancellationCheckpointRestoreAndCompactionRetry(t *testing.T) {
	client := &noticeClient{}
	team := newTestTeam(t, client)
	team.createStageLocked(phaseRecon)
	w := team.workers[0]
	a := w.agent
	if _, err := a.board.Post("user", "user", "*", 0, "pending evidence", "precise excerpt"); err != nil {
		t.Fatal(err)
	}
	a.sanitizeMessages()
	ctx, cancel := context.WithCancel(context.Background())
	client.tools = func(ctx context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		if len(noticeMessages(messages)) != 1 {
			t.Fatal("request lacked notice")
		}
		cancel()
		return llm.ToolResponse{}, ctx.Err()
	}
	if _, err := a.chatStream(ctx, func(Event) {}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
	if a.forumPending == "" || w.saved.ForumPending != a.forumPending || w.saved.ForumCursor != a.forumCursor {
		t.Fatal("pending/cursor checkpoint split")
	}
	// Checkpoints own their slices; subsequent history mutation cannot erase the
	// stored notice before a concurrent SaveSession consumes it.
	original := a.forumPending
	a.messages[len(a.messages)-1] = llm.Message{Role: llm.RoleUser, Content: "mutated live history"}
	if got := noticeMessages(w.saved.Messages); len(got) != 1 || got[0] != original {
		t.Fatal("checkpoint aliased mutable history")
	}
	path := filepath.Join(t.TempDir(), "notices.json")
	if err := team.SaveSession(path); err != nil {
		t.Fatal(err)
	}

	retry := &noticeClient{}
	restored := newTestTeam(t, retry)
	if err := restored.LoadSession(path); err != nil {
		t.Fatal(err)
	}
	a = restored.workers[0].agent
	if a.forumPending != original || a.board.Name(a.id) != team.board.Name(a.id) {
		t.Fatal("restore lost pending notice or chosen name")
	}
	a.cfg.Agent.RetryAttempts = 1
	a.compressClient = &noticeClient{call: func(context.Context, []llm.Message) (string, error) { return "bounded context summary", nil }}
	calls := 0
	retry.tools = func(ctx context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		calls++
		got := noticeMessages(messages)
		if len(got) != 1 || got[0] != original {
			t.Fatal("restore/compaction omitted or duplicated pending notice")
		}
		if calls == 1 {
			return llm.ToolResponse{}, errors.New("context length exceeded")
		}
		return toolReply("read_handoff", map[string]any{}), nil
	}
	if _, err := a.chatStream(context.Background(), func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || a.forumPending != "" {
		t.Fatal("successful request failed to release pending notice")
	}
	if err := a.prepareRequest(context.Background(), func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if got := noticeMessages(a.messages); len(got) != 1 {
		t.Fatal("quiet restored turn duplicated notification")
	}
}

func TestActiveVerifierReceivesStreamingNoticeWithoutName(t *testing.T) {
	client := &noticeClient{}
	team := newTestTeam(t, client)
	team.board.Register("audit-1-verify-1", phaseAudit)
	child := newWorker(team.cfg, team.prompts, client, client, team.registry.Fork(), "audit-1-verify-1", phaseAudit, team.board)
	defer child.tools.Close()
	child.cfg.OpenAI.Stream = true
	step := 0
	client.tools = func(ctx context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		step++
		switch step {
		case 1:
			if _, err := team.board.Post("user", "user", "*", 0, "verifier update", "literal evidence"); err != nil {
				t.Fatal(err)
			}
			return toolReply("read_file", map[string]any{"path": "entry1.go", "limit": 1}), nil
		default:
			got := noticeMessages(messages)
			if len(got) != 1 || !strings.Contains(got[0], "verifier update") {
				t.Fatal("active verifier missed notice")
			}
			return toolReply("read_file", map[string]any{"path": "entry1.go", "limit": 1}), nil
		}
	}
	client.call = func(_ context.Context, messages []llm.Message) (string, error) {
		for _, message := range messages {
			if message.Type != "" {
				t.Fatal("final text-only verification request retained native items")
			}
		}
		return "verified conclusion", nil
	}
	conclusion, err := child.runVerification(context.Background(), func(Event) {}, verifyFindingArgs{Title: "candidate"})
	if err != nil || conclusion != "verified conclusion" || child.forumPending != "" || step != child.verificationTurnLimit() {
		t.Fatalf("verifier did not finish cleanly: %q %v", conclusion, err)
	}
}
