package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"code-review-agent/internal/llm"
)

func TestModeratorWakesReadsAssignsSleepsAndRestores(t *testing.T) {
	var mu sync.Mutex
	activation, step := 0, 0
	entered := make(chan int, 32)
	client := &noticeClient{tools: func(ctx context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		if !strings.Contains(messages[0].Content, "内部路由 ID 是 moderator") {
			return llm.ToolResponse{}, fmt.Errorf("not an independent moderator request")
		}
		lastActivation := ""
		for _, m := range messages {
			if strings.HasPrefix(m.Content, "管理员激活：") {
				lastActivation = m.Content
			}
		}
		mu.Lock()
		defer mu.Unlock()
		// Each activation ends with idle, therefore the latest user activation is
		// immediately before the first model call (possibly followed by notices).
		newActivation := true
		for i := len(messages) - 1; i >= 0; i-- {
			if strings.HasPrefix(messages[i].Content, "管理员激活：") {
				break
			}
			if messages[i].Role == llm.RoleAssistant || messages[i].Type == "function_call" {
				newActivation = false
				break
			}
		}
		if newActivation {
			activation++
			step = 0
			entered <- activation
		}
		step++
		switch step {
		case 1:
			return toolReply("read_file", map[string]any{"path": "entry1.go", "offset": 1, "limit": 1}), nil
		case 2:
			found := false
			for _, m := range messages {
				if m.Type == "function_call_output" && strings.Contains(m.Content, "package fixture") {
					found = true
				}
			}
			if !found {
				return llm.ToolResponse{}, fmt.Errorf("source tool did not expose fixture")
			}
			if activation == 1 {
				return toolReply("moderator_assign", map[string]any{"agent_id": "audit-1", "content": "请复核 entry1.go:1 的入口可达性，并提供调用链证据。"}), nil
			}
			if strings.Contains(lastActivation, "review_stage=audit") {
				return toolReply("moderator_decide", map[string]any{"stage": "audit", "decision": "approve", "reason": "源码入口与交接已核验"}), nil
			}
		}
		return toolReply("moderator_idle", map[string]any{}), nil
	}}
	team := newTestTeam(t, client)
	team.cfg.Agent.ModeratorEnabled = nil
	team.resetBoard()
	team.phase = phaseAudit
	team.createStageLocked(phaseAudit)
	team.ensureModeratorLocked()
	ticks := make(chan time.Time, 2)
	team.moderatorTicks = ticks
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := team.startModerator(ctx)
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()
	awaitActivation := func() {
		t.Helper()
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("moderator activation missing")
		}
	}
	awaitIdle := func() {
		t.Helper()
		deadline := time.After(3 * time.Second)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-deadline:
				t.Fatal("moderator did not idle")
			case <-ticker.C:
				team.mu.Lock()
				idle := team.moderator.saved.Status.Status == "idle"
				hasHistory := len(team.moderator.saved.Messages) > 1
				team.mu.Unlock()
				if idle && hasHistory {
					return
				}
			}
		}
	}
	awaitActivation()
	awaitIdle()
	if posts := team.ForumMessages(); len(posts) != 1 || posts[0].AgentID != phaseModerator || posts[0].To != "audit-1" {
		t.Fatalf("assignment not publicly attributed: %+v", posts)
	}
	select {
	case <-entered:
		t.Fatal("moderator woke on own assignment")
	case <-time.After(50 * time.Millisecond):
	}
	ticks <- time.Now()
	awaitActivation()
	awaitIdle()
	if err := team.PostMessage("请核验源码证据"); err != nil {
		t.Fatal(err)
	}
	awaitActivation()
	awaitIdle()
	team.mu.Lock()
	team.pendingAssignments = nil
	team.mu.Unlock()
	result := make(chan bool, 1)
	go func() { result <- team.reviewStage(ctx, phaseAudit) }()
	awaitActivation()
	select {
	case approved := <-result:
		if !approved {
			t.Fatal("explicit source-backed stage approval lost")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("review barrier stuck")
	}
	stop()
	stopped = true
	path := filepath.Join(t.TempDir(), "moderator.json")
	if err := team.SaveSession(path); err != nil {
		t.Fatal(err)
	}
	restored := newTestTeam(t, client)
	restored.cfg.Agent.ModeratorEnabled = nil
	if err := restored.LoadSession(path); err != nil {
		t.Fatal(err)
	}
	if len(restored.workers) != 4 || restored.moderator == nil || len(restored.moderator.saved.Messages) < 2 {
		t.Fatal("moderator history restored as ordinary worker or lost")
	}
	for _, w := range restored.workers {
		if w.agent.moderateTool != nil {
			t.Fatal("moderator permission leaked to worker")
		}
	}
	restored.moderatorTicks = ticks
	resumeStop := restored.startModerator(ctx)
	awaitActivation()
	resumeStop()
}

func TestModeratorReviewsBothStageBarriersWithoutExtraVoters(t *testing.T) {
	ordinary := newStagedClient(4, 4)
	client := &noticeClient{}
	var mu sync.Mutex
	reviews := map[string]int{}
	client.tools = func(ctx context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		if !strings.Contains(messages[0].Content, "内部路由 ID 是 moderator") {
			return ordinary.ChatTools(ctx, messages, nil, nil)
		}
		activation := ""
		decided := false
		for _, m := range messages {
			if strings.HasPrefix(m.Content, "管理员激活：") {
				activation = m.Content
				decided = false
			}
			if m.Type == "function_call" && m.Name == "moderator_decide" {
				decided = true
			}
		}
		if !decided {
			for _, stage := range []string{phaseRecon, phaseAudit} {
				if strings.Contains(activation, "review_stage="+stage+"。") {
					mu.Lock()
					reviews[stage]++
					mu.Unlock()
					return toolReply("moderator_decide", map[string]any{"stage": stage, "decision": "approve", "reason": "已审阅当前团队状态与证据"}), nil
				}
			}
		}
		return toolReply("moderator_idle", map[string]any{}), nil
	}
	team := newTestTeam(t, client)
	team.cfg.Agent.ModeratorEnabled = nil
	team.resetBoard()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	team.Run(ctx, "inspect fixtures", func(Event) {})
	if ctx.Err() != nil || team.Phase() != "completed" {
		t.Fatalf("moderator blocked valid worker consensus: %s (%v)", team.Phase(), ctx.Err())
	}
	mu.Lock()
	defer mu.Unlock()
	if reviews[phaseRecon] != 0 || reviews[phaseAudit] != 1 || len(team.workers) != 8 || len(team.Statuses()) != 9 {
		t.Fatalf("reviews=%v workers=%d statuses=%d", reviews, len(team.workers), len(team.Statuses()))
	}
	if status := team.Statuses()[8]; status.Status != "completed" {
		t.Fatalf("moderator retained live status after shutdown: %+v", status)
	}
}

func TestModeratorReviewPreemptsOrdinaryGeneration(t *testing.T) {
	entered := make(chan struct{})
	drained := make(chan struct{})
	client := &noticeClient{tools: func(ctx context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		activation := ""
		for _, m := range messages {
			if strings.HasPrefix(m.Content, "管理员激活：") {
				activation = m.Content
			}
		}
		if strings.Contains(activation, "review_stage=audit。") {
			select {
			case <-drained:
			default:
				t.Error("review started before ordinary request drained")
			}
			return toolReply("moderator_decide", map[string]any{"stage": phaseAudit, "decision": "approve", "reason": "已核验交接"}), nil
		}
		close(entered)
		<-ctx.Done()
		close(drained)
		// A cancelled response must not execute even if the provider returns a call.
		return toolReply("moderator_assign", map[string]any{"agent_id": "audit-1", "content": "stale cancelled assignment"}), nil
	}}
	team := newTestTeam(t, client)
	team.cfg.Agent.ModeratorEnabled = nil
	team.resetBoard()
	team.phase = phaseAudit
	team.createStageLocked(phaseAudit)
	team.ensureModeratorLocked()
	team.moderatorTicks = make(chan time.Time)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stop := team.startModerator(ctx)
	defer stop()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("ordinary request did not start")
	}
	if !team.reviewStage(ctx, phaseAudit) {
		t.Fatal("stage review failed to preempt ordinary generation and approve")
	}
	team.mu.Lock()
	defer team.mu.Unlock()
	if len(team.pendingAssignments) != 0 {
		t.Fatal("cancelled ordinary response executed a stale assignment")
	}
}

func TestModeratorDecisionEndsActivationWithoutIdle(t *testing.T) {
	for _, decision := range []string{"approve", "request_work"} {
		t.Run(decision, func(t *testing.T) {
			calls := 0
			client := &noticeClient{tools: func(ctx context.Context, messages []llm.Message) (llm.ToolResponse, error) {
				calls++
				if calls != 1 {
					return llm.ToolResponse{}, fmt.Errorf("unexpected generation after decision")
				}
				args := map[string]any{"stage": phaseAudit, "decision": decision, "reason": "已核验当前证据"}
				if decision == "request_work" {
					args["assignments"] = []moderatorAssignment{{AgentID: "audit-1", Content: "继续核验入口可达性"}}
				}
				return toolReply("moderator_decide", args), nil
			}}
			team := newTestTeam(t, client)
			team.cfg.Agent.ModeratorEnabled = nil
			team.resetBoard()
			team.phase = phaseAudit
			team.createStageLocked(phaseAudit)
			team.ensureModeratorLocked()
			approved := team.moderatorActivation(context.Background(), team.moderator, "阶段结束审查", phaseAudit)
			if approved != (decision == "approve") || calls != 1 {
				t.Fatalf("decision=%s approved=%v requests=%d", decision, approved, calls)
			}
			if decision == "request_work" && len(team.pendingAssignments["audit-1"]) != 1 {
				t.Fatal("follow-up assignment lost during immediate return")
			}
			messages := team.moderator.saved.Messages
			if len(messages) < 2 || messages[len(messages)-1].Type != "function_call_output" || messages[len(messages)-1].CallID != messages[len(messages)-2].CallID {
				t.Fatal("decision returned without a paired native tool result")
			}
		})
	}
}
