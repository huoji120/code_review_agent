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
	call func(context.Context, []llm.Message) (string, error)
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
	client.call = func(ctx context.Context, messages []llm.Message) (string, error) {
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
				if strings.HasPrefix(m.Content, "Tool result for read_file:\n") {
					var result struct {
						BufferID string `json:"buffer_id"`
					}
					if err := json.Unmarshal([]byte(strings.TrimPrefix(m.Content, "Tool result for read_file:\n")), &result); err != nil {
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
			if !strings.HasPrefix(last, "Tool result for read_tool_buffer:\n") || !strings.Contains(last, `"ok":true`) {
				t.Fatalf("notice invalidated buffer: %s", last)
			}
			return toolReply("read_tool_buffer", map[string]any{"buffer_id": bufferID, "offset": 0, "limit": 64}), nil
		}
		return "", fmt.Errorf("unexpected model request")
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
	if err := a.board.RegisterName(a.id, "Persistent reader"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.board.Post("user", "user", "*", 0, "pending evidence", "precise excerpt"); err != nil {
		t.Fatal(err)
	}
	a.sanitizeMessages()
	ctx, cancel := context.WithCancel(context.Background())
	client.call = func(ctx context.Context, messages []llm.Message) (string, error) {
		if len(noticeMessages(messages)) != 1 {
			t.Fatal("request lacked notice")
		}
		cancel()
		return "", ctx.Err()
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
	if a.forumPending != original || a.board.Name(a.id) != "Persistent reader" {
		t.Fatal("restore lost pending notice or chosen name")
	}
	a.cfg.Agent.RetryAttempts = 1
	a.compressClient = &noticeClient{call: func(context.Context, []llm.Message) (string, error) { return "bounded context summary", nil }}
	calls := 0
	retry.call = func(ctx context.Context, messages []llm.Message) (string, error) {
		calls++
		got := noticeMessages(messages)
		if len(got) != 1 || got[0] != original {
			t.Fatal("restore/compaction omitted or duplicated pending notice")
		}
		if calls == 1 {
			return "", errors.New("context length exceeded")
		}
		return "continue", nil
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
	client.call = func(ctx context.Context, messages []llm.Message) (string, error) {
		step++
		switch step {
		case 1:
			if _, err := team.board.Post("user", "user", "*", 0, "verifier update", "literal evidence"); err != nil {
				t.Fatal(err)
			}
			return toolReply("read_file", map[string]any{"path": "entry1.go", "limit": 1}), nil
		case 2:
			got := noticeMessages(messages)
			if len(got) != 1 || !strings.Contains(got[0], "verifier update") {
				t.Fatal("active verifier missed notice")
			}
			return "verified conclusion", nil
		}
		return "", fmt.Errorf("unexpected verifier turn")
	}
	conclusion, err := child.runVerification(context.Background(), func(Event) {}, verifyFindingArgs{Title: "candidate"})
	if err != nil || conclusion != "verified conclusion" || child.forumPending != "" || step != 2 {
		t.Fatalf("verifier did not finish cleanly: %q %v", conclusion, err)
	}
}
