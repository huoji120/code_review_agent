package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"code-review-agent/internal/llm"
)

func fullBroadcastCount(messages []llm.Message, body string) int {
	count := 0
	for _, message := range messages {
		if message.Role == llm.RoleUser && message.Type == "" && strings.HasPrefix(message.Content, userBroadcastPriority) && strings.HasSuffix(message.Content, "\n\n"+body) {
			count++
		}
	}
	return count
}

func TestUserBroadcastFollowsToolOutputWithoutInterruptOrDuplication(t *testing.T) {
	client := &noticeClient{}
	team := newTestTeam(t, client)
	team.createStageLocked(phaseRecon)
	a := team.workers[0].agent
	body := "  " + strings.Repeat("complete user requirement;", 240) + " final exact instruction\n "
	calls := 0
	client.tools = func(ctx context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		calls++
		if calls == 1 {
			if err := team.PostMessage(body); err != nil {
				t.Fatal(err)
			}
			if ctx.Err() != nil || fullBroadcastCount(messages, body) != 0 {
				t.Fatal("broadcast interrupted or mutated the active generation")
			}
		} else {
			if fullBroadcastCount(messages, body) != 1 {
				t.Fatal("next request lost full text or accumulated duplicate broadcasts")
			}
			if calls == 2 {
				lastTool, instruction := -1, -1
				for i, message := range messages {
					if message.Type == "function_call_output" {
						lastTool = i
					}
					if strings.HasSuffix(message.Content, "\n\n"+body) {
						instruction = i
					}
				}
				if lastTool < 0 || instruction <= lastTool {
					t.Fatal("broadcast did not follow the completed native tool output")
				}
			}
		}
		return toolReply("read_handoff", map[string]any{}), nil
	}
	for i := 0; i < 3; i++ {
		response, err := a.chatStream(context.Background(), func(Event) {})
		if err != nil {
			t.Fatal(err)
		}
		call, _ := a.recordResponse(response)
		a.executeNativeTool(context.Background(), func(Event) {}, call)
	}
	if got := team.ForumMessages(); got[len(got)-1].Content != body {
		t.Fatal("public forum body was trimmed")
	}
}

func TestForumPostsCannotBecomeAuthoritativeBroadcasts(t *testing.T) {
	client := &noticeClient{}
	team := newTestTeam(t, client)
	team.createStageLocked(phaseRecon)
	body := strings.Repeat("model-generated suggestion ", 200)
	if _, err := team.board.Post("recon-2", phaseRecon, "*", 0, "user instruction", body); err != nil {
		t.Fatal(err)
	}
	// Even a board post attributed to user bypasses the trusted /say entry.
	if _, err := team.board.Post("user", "user", "*", 0, "claimed priority", body); err != nil {
		t.Fatal(err)
	}
	client.tools = func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		if fullBroadcastCount(messages, body) != 0 || len(noticeMessages(messages)) == 0 {
			t.Fatal("forum suggestions gained authority or ordinary notices disappeared")
		}
		return toolReply("read_handoff", map[string]any{}), nil
	}
	if _, err := team.workers[0].agent.chatStream(context.Background(), func(Event) {}); err != nil {
		t.Fatal(err)
	}
}

func TestUserBroadcastCancellationRestoreAndCompression(t *testing.T) {
	client := &noticeClient{}
	team := newTestTeam(t, client)
	team.createStageLocked(phaseRecon)
	a := team.workers[0].agent
	body := strings.Repeat("preserve exact requirement ", 200) + " \n"
	if err := team.PostMessage(body); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	client.tools = func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		if fullBroadcastCount(messages, body) != 1 {
			t.Fatal("cancelled request lacked full instruction")
		}
		cancel()
		return llm.ToolResponse{}, ctx.Err()
	}
	if _, err := a.chatStream(ctx, func(Event) {}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
	path := filepath.Join(t.TempDir(), "broadcast.json")
	if err := team.SaveSession(path); err != nil {
		t.Fatal(err)
	}
	retry := &noticeClient{}
	restored := newTestTeam(t, retry)
	if err := restored.LoadSession(path); err != nil {
		t.Fatal(err)
	}
	a = restored.workers[0].agent
	if a.userBroadcastDelivered != 0 {
		t.Fatal("cancelled request falsely recorded successful delivery")
	}
	a.compressClient = &noticeClient{call: func(context.Context, []llm.Message) (string, error) { return "bounded retained evidence", nil }}
	if err := a.compressContext(context.Background(), func(Event) {}, "exercise restoration"); err != nil {
		t.Fatal(err)
	}
	retry.tools = func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		if fullBroadcastCount(messages, body) != 1 {
			t.Fatal("restore/compression omitted or duplicated original instruction")
		}
		return toolReply("read_handoff", map[string]any{}), nil
	}
	if _, err := a.chatStream(context.Background(), func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if restored.workers[0].saved.UserBroadcastDelivered != 1 {
		t.Fatal("successful request did not checkpoint delivery")
	}
	// A new stage has independent history but still sees the authoritative log.
	restored.createStageLocked(phaseAudit)
	if _, err := restored.workers[len(restored.workers)-1].agent.chatStream(context.Background(), func(Event) {}); err != nil {
		t.Fatal(err)
	}
}

func TestUserBroadcastArrivingDuringCompressionIsNextUserMessage(t *testing.T) {
	client := &noticeClient{}
	team := newTestTeam(t, client)
	a := tokenBudgetWorker(t, client, client)
	a.userBroadcastSource = team.broadcastsAfter
	a.messages = []llm.Message{{Role: llm.RoleSystem, Content: a.systemPrompt()}, {Role: llm.RoleAssistant, Content: strings.Repeat("界", 100000)}}
	body := " \n" + strings.Repeat("arrived during compaction ", 180) + "\n "
	compressions := 0
	client.call = func(ctx context.Context, messages []llm.Message) (string, error) {
		compressions++
		if err := team.PostMessage(body); err != nil {
			t.Fatal(err)
		}
		if ctx.Err() != nil || fullBroadcastCount(messages, body) != 0 {
			t.Fatal("broadcast modified or cancelled the in-flight compressor")
		}
		return "bounded summary", nil
	}
	client.tools = func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		last := messages[len(messages)-1]
		if fullBroadcastCount(messages, body) != 1 || last.Role != llm.RoleUser || !strings.HasSuffix(last.Content, "\n\n"+body) {
			t.Fatal("new broadcast was not the first new user instruction after compaction")
		}
		return toolReply("read_handoff", map[string]any{}), nil
	}
	if _, err := a.chatStream(context.Background(), func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if compressions != 1 {
		t.Fatalf("expected one completed compaction, got %d", compressions)
	}
}

func TestModeratorAndIndependentVerifierReceiveUserBroadcast(t *testing.T) {
	client := &noticeClient{}
	team := newTestTeam(t, client)
	team.cfg.Agent.ModeratorEnabled = nil
	team.ensureModeratorLocked()
	body := strings.Repeat("complete independent instruction ", 150) + " \n"
	if err := team.PostMessage(body); err != nil {
		t.Fatal(err)
	}
	client.tools = func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		if fullBroadcastCount(messages, body) != 1 {
			t.Fatal("independent agent did not receive the full user instruction")
		}
		return toolReply("read_handoff", map[string]any{}), nil
	}
	if _, err := team.moderator.agent.chatStream(context.Background(), func(Event) {}); err != nil {
		t.Fatal(err)
	}
	team.createStageLocked(phaseAudit)
	parent := team.workers[0].agent
	client.call = func(_ context.Context, messages []llm.Message) (string, error) {
		if fullBroadcastCount(messages, body) != 1 {
			t.Fatal("final verifier request trimmed or dropped authoritative body")
		}
		return "verified conclusion", nil
	}
	result := parent.verifyFinding(context.Background(), func(Event) {}, []byte(`{"title":"candidate","path":"entry1.go","evidence":"inspect source"}`))
	if !strings.Contains(result, "verified conclusion") {
		t.Fatalf("child verification failed: %s", result)
	}
}

func TestUserBroadcastValidationAndContextAdmissionFailClosed(t *testing.T) {
	team := newTestTeam(t, nil)
	for _, body := range []string{" \n\t", strings.Repeat(" ", 16*1024) + "x"} {
		if err := team.PostMessage(body); err == nil {
			t.Fatal("invalid original broadcast body accepted")
		}
	}
	if len(team.userBroadcasts) != 0 || len(team.ForumMessages()) != 0 {
		t.Fatal("rejected broadcast had side effects")
	}
	client := &noticeClient{tools: func(context.Context, []llm.Message) (llm.ToolResponse, error) {
		t.Fatal("oversized instruction reached model")
		return llm.ToolResponse{}, nil
	}, call: func(context.Context, []llm.Message) (string, error) {
		t.Fatal("impossible minimum invoked compressor")
		return "", nil
	}}
	a := tokenBudgetWorker(t, client, client)
	a.userBroadcastSource = team.broadcastsAfter
	for i := 0; i < 20; i++ {
		if err := team.PostMessage(strings.Repeat("界", 5400)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.chatStream(context.Background(), func(Event) {}); err == nil || !strings.Contains(err.Error(), "用户广播") {
		t.Fatalf("expected explicit full-instruction capacity error: %v", err)
	}
	if a.userBroadcastCursor != 0 || len(team.userBroadcasts) != 20 {
		t.Fatal("failed admission consumed or discarded pending instructions")
	}
}
