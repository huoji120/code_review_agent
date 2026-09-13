package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"code-review-agent/internal/llm"
)

func TestReconHandsOffDespiteModeratorWorkAndPendingIRC(t *testing.T) {
	ordinary := newStagedClient(4, 4)
	held := make(chan struct{})
	drained := make(chan struct{})
	var mu sync.Mutex
	reconModeratorCalls, auditReviews := 0, 0
	client := &ircClient{call: func(ctx context.Context, messages []llm.Message, definitions []llm.ToolDefinition, emit func(llm.Delta) error) (llm.ToolResponse, error) {
		if strings.Contains(messages[0].Content, "内部路由 ID 是 moderator") {
			activation := ""
			for _, m := range messages {
				if strings.HasPrefix(m.Content, "管理员激活：") {
					activation = m.Content
				}
			}
			if strings.Contains(activation, "current_stage=recon；") {
				if strings.Contains(activation, "review_stage=recon。") {
					return llm.ToolResponse{}, fmt.Errorf("recon must not require moderator approval")
				}
				mu.Lock()
				reconModeratorCalls++
				n := reconModeratorCalls
				mu.Unlock()
				switch n {
				case 1:
					return toolReply("moderator_assign", map[string]any{"agent_id": "recon-1", "content": "DEFERRED_AUDIT_CHECK"}), nil
				case 2:
					return toolReply("moderator_irc_send", map[string]any{"agent_id": "recon-1", "content": "UNANSWERED_RECON_IRC"}), nil
				default:
					close(held)
					<-ctx.Done()
					close(drained)
					return llm.ToolResponse{}, ctx.Err()
				}
			}
			if strings.Contains(activation, "review_stage=audit。") {
				mu.Lock()
				auditReviews++
				mu.Unlock()
				return toolReply("moderator_decide", map[string]any{"stage": phaseAudit, "decision": "approve", "reason": "审计证据齐备"}), nil
			}
			return toolReply("moderator_idle", map[string]any{}), nil
		}
		identity := workerIdentity.FindStringSubmatch(messages[0].Content)
		if len(identity) != 3 {
			return llm.ToolResponse{}, fmt.Errorf("missing identity")
		}
		if identity[2] == phaseRecon {
			select {
			case <-held:
			case <-ctx.Done():
				return llm.ToolResponse{}, ctx.Err()
			}
			planning := false
			for _, d := range definitions {
				if d.Name == "audit_plan_done" {
					planning = true
				}
			}
			if !planning {
				<-ctx.Done()
				return llm.ToolResponse{}, ctx.Err()
			}
		} else {
			select {
			case <-drained:
			default:
				return llm.ToolResponse{}, fmt.Errorf("audit started before moderator drained")
			}
		}
		return ordinary.ChatTools(ctx, messages, definitions, emit)
	}}
	team := newTestTeam(t, client)
	team.cfg.Agent.ModeratorEnabled = nil
	team.resetBoard()
	team.moderatorTicks = make(chan time.Time)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	team.Run(ctx, "inspect fixtures", func(Event) {})
	if ctx.Err() != nil || team.Phase() != "completed" {
		t.Fatalf("recon blocked audit transition: phase=%s err=%v", team.Phase(), ctx.Err())
	}
	ordinary.mu.Lock()
	for id, steps := range ordinary.steps {
		if strings.HasPrefix(id, "recon-") && steps != 5 {
			t.Errorf("recon reopened: %s steps=%d", id, steps)
		}
	}
	ordinary.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	if auditReviews != 1 {
		t.Fatalf("audit final review lost: %d", auditReviews)
	}
	var handoff struct {
		Data []struct {
			AgentID     string       `json:"agent_id"`
			Suggestions []string     `json:"moderator_suggestions"`
			IRC         []IRCMessage `json:"irc"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(team.handoff), &handoff); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range handoff.Data {
		if e.AgentID != "recon-1" {
			continue
		}
		found = len(e.Suggestions) == 1 && e.Suggestions[0] == "DEFERRED_AUDIT_CHECK" && len(e.IRC) == 1 && e.IRC[0].Content == "UNANSWERED_RECON_IRC" && e.IRC[0].State == "failed" && e.IRC[0].Reply == ""
	}
	if !found {
		t.Fatal("deferred suggestion or unresolved IRC missing/misreported in audit handoff")
	}
	if len(team.pendingAssignments) != 0 {
		t.Fatal("stale recon assignment blocks audit approval")
	}
	for _, w := range team.workers {
		if err := validateNativeHistory(w.agent.messages); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRestoredCompletedReconCarriesSuggestionsWithoutReopening(t *testing.T) {
	client := newStagedClient(4, 4)
	team := newTestTeam(t, client)
	team.createStageLocked(phaseRecon)
	for _, w := range team.workers {
		w.agent.plan = &auditPlanDoneArgs{Summary: "侦察完成", AuditMap: strings.Repeat("已读入口地图；未验证候选留待审计。", 50), AuditFiles: []string{"entry1.go"}}
		w.agent.completed = true
		w.agent.sanitizeMessages()
		team.capture(w, w.agent)
	}
	team.pendingAssignments = map[string][]string{"recon-1": {"RESTORED_AUDIT_SUGGESTION"}}
	path := t.TempDir() + "/recon.json"
	if err := team.SaveSession(path); err != nil {
		t.Fatal(err)
	}
	restored := newTestTeam(t, client)
	if err := restored.LoadSession(path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	restored.Run(ctx, "continue", func(e Event) {
		if e.Kind == "error" {
			t.Log(e.Content)
		}
	})
	if ctx.Err() != nil || restored.Phase() != "completed" {
		t.Fatalf("restored recon did not advance: %s %v statuses=%+v", restored.Phase(), ctx.Err(), restored.Statuses())
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	for id, calls := range client.steps {
		if strings.HasPrefix(id, "recon-") && calls > 0 {
			t.Fatalf("completed recon restarted: %s", id)
		}
	}
	var handoff struct {
		Data []struct {
			Suggestions []string          `json:"moderator_suggestions"`
			Plan        auditPlanDoneArgs `json:"plan"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(restored.handoff), &handoff); err != nil {
		t.Fatal(err)
	}
	if len(handoff.Data) != 4 || len(handoff.Data[0].Suggestions) != 1 || handoff.Data[0].Suggestions[0] != "RESTORED_AUDIT_SUGGESTION" {
		t.Fatal("restored moderator suggestion lost at phase boundary")
	}
	if err := restored.SaveSession(path); err != nil {
		t.Fatal(err)
	}
	afterAudit := newTestTeam(t, client)
	if err := afterAudit.LoadSession(path); err != nil {
		t.Fatal(err)
	}
	handoff.Data = nil
	if err := json.Unmarshal([]byte(afterAudit.handoff), &handoff); err != nil {
		t.Fatal(err)
	}
	if len(handoff.Data) != 4 || len(handoff.Data[0].Suggestions) != 1 || handoff.Data[0].Suggestions[0] != "RESTORED_AUDIT_SUGGESTION" || handoff.Data[0].Plan.AuditMap != strings.Repeat("已读入口地图；未验证候选留待审计。", 50) {
		t.Fatal("audit-session restoration lost recon plan or deferred suggestion")
	}
}
