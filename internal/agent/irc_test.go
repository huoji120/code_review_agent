package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"code-review-agent/internal/llm"
)

type ircClient struct {
	call func(context.Context, []llm.Message, []llm.ToolDefinition, func(llm.Delta) error) (llm.ToolResponse, error)
}

func (c *ircClient) Chat(context.Context, []llm.Message) (string, error) {
	return "", fmt.Errorf("unexpected non-native request")
}
func (c *ircClient) ChatStream(context.Context, []llm.Message, func(llm.Delta) error) error {
	return fmt.Errorf("unexpected plain streaming request")
}
func (c *ircClient) ChatTools(ctx context.Context, messages []llm.Message, definitions []llm.ToolDefinition, emit func(llm.Delta) error) (llm.ToolResponse, error) {
	return c.call(ctx, messages, definitions, emit)
}

func newIRCTestTeam(t *testing.T, client llm.Client) *Team {
	t.Helper()
	team := newTestTeam(t, client)
	team.cfg.Agent.ReconAgents, team.cfg.Agent.AuditAgents = 1, 1
	team.resetBoard()
	team.board.Register(phaseModerator, phaseModerator)
	team.createStageLocked(phaseRecon)
	team.running, team.stageOpen = true, true
	team.workerCancels = make(map[string]map[string]context.CancelFunc)
	t.Cleanup(func() { team.mu.Lock(); team.running, team.stageOpen = false, false; team.mu.Unlock() })
	return team
}

func sendTestIRC(t *testing.T, team *Team, content string) IRCMessage {
	t.Helper()
	var result struct {
		OK      bool       `json:"ok"`
		Message IRCMessage `json:"message"`
	}
	raw := team.sendIRC(context.Background(), moderatorAssignment{AgentID: "recon-1", Content: content})
	if err := json.Unmarshal([]byte(raw), &result); err != nil || !result.OK || result.Message.ID == 0 {
		t.Fatalf("send: %s (%v)", raw, err)
	}
	return result.Message
}

func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not reach expected boundary")
	}
}

func priorityID(messages []llm.Message) int64 {
	for i := len(messages) - 1; i >= 0; i-- {
		var id int64
		if _, err := fmt.Sscanf(messages[i].Content, "优先 IRC message_id=%d", &id); err == nil {
			return id
		}
	}
	return 0
}

func TestIRCInterruptsStreamingWithoutPartialMutationOrDuplicateRun(t *testing.T) {
	entered, resumed := make(chan struct{}), make(chan struct{})
	var active, maximum atomic.Int32
	step := 0
	client := &ircClient{call: func(ctx context.Context, messages []llm.Message, _ []llm.ToolDefinition, emit func(llm.Delta) error) (llm.ToolResponse, error) {
		n := active.Add(1)
		defer active.Add(-1)
		if n > maximum.Load() {
			maximum.Store(n)
		}
		step++
		if err := validateNativeHistory(messages); err != nil {
			return llm.ToolResponse{}, err
		}
		switch step {
		case 1:
			emit(llm.Delta{Thinking: "partial thought", Progress: &llm.GenerationProgress{OutputTokens: 4, ReasoningTokens: 4, Estimated: true, ReceivedAt: time.Now().Format(time.RFC3339Nano)}})
			close(entered)
			<-ctx.Done()
			// An adapter returning a stale/unfinished native call must still be inert.
			return toolReply("forum_post", map[string]any{"content": "MUST_NOT_EXECUTE"}), nil
		case 2, 4:
			id := priorityID(messages)
			if id == 0 {
				return llm.ToolResponse{}, fmt.Errorf("priority message missing")
			}
			return toolReply("worker_irc_reply", map[string]any{"message_id": id, "content": fmt.Sprintf("checked-%d", id)}), nil
		case 3:
			close(resumed)
			<-ctx.Done()
			return llm.ToolResponse{}, ctx.Err()
		case 5:
			return toolReply("audit_plan_done", map[string]any{"summary": "done", "audit_map": "entry", "audit_files": []string{"entry1.go"}}), nil
		}
		return llm.ToolResponse{}, fmt.Errorf("unexpected request")
	}}
	team := newIRCTestTeam(t, client)
	w := team.workers[0]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); w.agent.Run(ctx, "ORIGINAL_ONCE", func(e Event) { team.receive(w, e) }) }()
	waitSignal(t, entered)
	first := sendTestIRC(t, team, "first")
	waitSignal(t, resumed)
	second := sendTestIRC(t, team, "second")
	waitSignal(t, done)
	if maximum.Load() != 1 || !w.agent.completed || w.agent.protocolFailures != 0 {
		t.Fatalf("not serial/completed: max=%d err=%v", maximum.Load(), w.agent.runErr)
	}
	if err := validateNativeHistory(w.agent.messages); err != nil {
		t.Fatal(err)
	}
	initial, deliveries := 0, 0
	for _, m := range w.agent.messages {
		if strings.Contains(m.Content, "ORIGINAL_ONCE") {
			initial++
		}
		if strings.HasPrefix(m.Content, "优先 IRC message_id=") {
			deliveries++
		}
	}
	if initial != 1 || deliveries != 2 {
		t.Fatalf("duplicate/lost insertion initial=%d delivery=%d", initial, deliveries)
	}
	for _, m := range team.ForumMessages() {
		if strings.Contains(m.Content, "MUST_NOT_EXECUTE") {
			t.Fatal("partial call executed")
		}
	}
	for _, id := range []int64{first.ID, second.ID} {
		var read struct {
			Messages []IRCMessage `json:"messages"`
		}
		json.Unmarshal([]byte(team.readIRC(ctx, json.RawMessage(fmt.Sprintf(`{"message_id":%d}`, id)))), &read)
		if len(read.Messages) != 1 || read.Messages[0].State != "replied" || read.Messages[0].ReplyForumID == 0 {
			t.Fatalf("missing correlated reply: %+v", read)
		}
	}
}

func TestIRCCancelsForumWaitAndPreservesNativePair(t *testing.T) {
	waiting := make(chan struct{})
	step := 0
	client := &noticeClient{tools: func(ctx context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		step++
		switch step {
		case 1:
			return toolReply("forum_wait", map[string]any{"from": "user", "timeout_seconds": 120}), nil
		case 2:
			if err := validateNativeHistory(messages); err != nil {
				return llm.ToolResponse{}, err
			}
			found := false
			for _, m := range messages {
				if m.Type == "function_call_output" && strings.Contains(m.Content, `"cancelled":true`) {
					found = true
				}
			}
			if !found {
				return llm.ToolResponse{}, fmt.Errorf("wait cancellation output missing")
			}
			return toolReply("worker_irc_reply", map[string]any{"message_id": priorityID(messages), "content": "wait interrupted"}), nil
		default:
			return toolReply("audit_plan_done", map[string]any{"summary": "done", "audit_map": "entry", "audit_files": []string{"entry1.go"}}), nil
		}
	}}
	team := newIRCTestTeam(t, client)
	w := team.workers[0]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.agent.Run(ctx, "wait", func(e Event) {
			team.receive(w, e)
			if e.Kind == "waiting" {
				close(waiting)
			}
		})
	}()
	waitSignal(t, waiting)
	sendTestIRC(t, team, "stop waiting")
	waitSignal(t, done)
	if err := validateNativeHistory(w.agent.messages); err != nil {
		t.Fatal(err)
	}
	if !w.agent.completed {
		t.Fatalf("interruption ended worker: %v", w.agent.runErr)
	}
}

func TestIRCReplyAuthorizationWaitAndPausedRestore(t *testing.T) {
	team := newIRCTestTeam(t, &noticeClient{})
	w := team.workers[0]
	w.agent.sanitizeMessages()
	first := sendTestIRC(t, team, "deliver once")
	if !strings.Contains(team.replyIRC(context.Background(), w.agent.id, first.ID, "too early"), `"ok":false`) {
		t.Fatal("reply before delivery accepted")
	}
	team.deliverWorkerIRC(context.Background(), w)
	if !strings.Contains(team.replyIRC(context.Background(), "recon-2", first.ID, "wrong recipient"), `"ok":false`) {
		t.Fatal("wrong recipient accepted")
	}
	second := sendTestIRC(t, team, "queued across pause")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	team.deliverWorkerIRC(ctx, w)
	path := filepath.Join(t.TempDir(), "irc.json")
	team.running = false
	if err := team.SaveSession(path); err != nil {
		t.Fatal(err)
	}
	restored := newTestTeam(t, &noticeClient{})
	if err := restored.LoadSession(path); err != nil {
		t.Fatal(err)
	}
	rw := restored.workers[0]
	before := len(rw.agent.messages)
	restored.deliverWorkerIRC(context.Background(), rw)
	if len(rw.agent.messages) != before+1 {
		t.Fatal("delivered message duplicated or queued message lost on restore")
	}
	var firstState, secondState IRCMessage
	for _, m := range restored.ircMessages {
		if m.ID == first.ID {
			firstState = m
		}
		if m.ID == second.ID {
			secondState = m
		}
	}
	if firstState.State != "delivered" || secondState.State != "delivered" {
		t.Fatal("mailbox state not restored")
	}
	readDone := make(chan string, 1)
	go func() {
		readDone <- restored.readIRC(context.Background(), json.RawMessage(fmt.Sprintf(`{"message_id":%d,"after_id":%d,"timeout_seconds":2}`, first.ID, firstState.ChangeID)))
	}()
	if raw := restored.replyIRC(context.Background(), rw.agent.id, first.ID, "verified after resume"); !strings.Contains(raw, `"ok":true`) {
		t.Fatal(raw)
	}
	select {
	case raw := <-readDone:
		if !strings.Contains(raw, "verified after resume") {
			t.Fatal(raw)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("IRC wait held reply lock")
	}
	if !strings.Contains(restored.replyIRC(context.Background(), rw.agent.id, first.ID, "duplicate"), `"ok":false`) {
		t.Fatal("duplicate reply accepted")
	}
}

func TestIRCCompletedActorRemainsReachableWithoutReopeningStage(t *testing.T) {
	client := &noticeClient{tools: func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		return toolReply("worker_irc_reply", map[string]any{"message_id": priorityID(messages), "content": "completed worker evidence"}), nil
	}}
	team := newTestTeam(t, client)
	team.cfg.Agent.AuditAgents = 1
	team.phase = phaseAudit
	team.resetBoard()
	team.board.Register(phaseModerator, phaseModerator)
	team.createStageLocked(phaseAudit)
	team.running, team.stageOpen = true, true
	team.workerCancels = make(map[string]map[string]context.CancelFunc)
	t.Cleanup(func() { team.mu.Lock(); team.running = false; team.mu.Unlock() })
	w := team.workers[0]
	w.agent.plan = &auditPlanDoneArgs{Summary: "done", AuditMap: "entry", AuditFiles: []string{"entry1.go"}}
	w.agent.completed = true
	w.agent.sanitizeMessages()
	team.capture(w, w.agent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := team.startStageActors(ctx, phaseAudit, "inspect")
	defer stop()
	if !team.waitStageIdle(ctx, phaseAudit) {
		t.Fatal("completed actor blocks barrier")
	}
	var sent struct {
		Message IRCMessage `json:"message"`
	}
	raw := team.sendIRC(ctx, moderatorAssignment{AgentID: "audit-1", Content: "read finished evidence"})
	if err := json.Unmarshal([]byte(raw), &sent); err != nil || sent.Message.ID == 0 {
		t.Fatalf("send failed: %s", raw)
	}
	message := sent.Message
	if !team.waitStageIdle(ctx, phaseAudit) {
		t.Fatal("IRC not drained")
	}
	team.mu.Lock()
	completed := w.saved.Completed
	var state string
	for _, m := range team.ircMessages {
		if m.ID == message.ID {
			state = m.State
		}
	}
	team.mu.Unlock()
	if !completed || state != "replied" {
		t.Fatalf("completion changed or no reply: completed=%v state=%s", completed, state)
	}
	if err := validateNativeHistory(w.agent.messages); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationCaptureRetainsActivityAndImmutableStatus(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.createStageLocked(phaseRecon)
	w := team.workers[0]
	p := &llm.GenerationProgress{OutputTokens: 42, ReasoningTokens: 30, Estimated: true, ReceivedAt: "2026-09-13T01:02:03Z"}
	team.receive(w, Event{Kind: "model_progress", Generation: p})
	team.receive(w, Event{Kind: "tool", Content: "calling read_file"})
	team.receive(w, Event{Kind: "assistant_delta", Content: "partial result"})
	team.capture(w, w.agent)
	status := team.Statuses()[0]
	p.OutputTokens = 999
	status.Generation.OutputTokens = 888
	if actual := team.Statuses()[0]; actual.Generation.OutputTokens != 42 || actual.LastModelActivity != "2026-09-13T01:02:03Z" || actual.LastTool != "read_file" || !actual.OutputPartial {
		t.Fatalf("lost/aliased checkpoint: %+v", actual)
	}
	team.receive(w, Event{Kind: "model_progress", Generation: &llm.GenerationProgress{Estimated: true}})
	if actual := team.Statuses()[0]; actual.LastModelActivity != "2026-09-13T01:02:03Z" || actual.OutputExcerpt != "" {
		t.Fatal("request start fabricated activity or retained prior excerpt")
	}
}
