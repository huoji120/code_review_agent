package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"code-review-agent/internal/config"
	"code-review-agent/internal/llm"
)

func TestInfiniteModeHasNoEndToolAndIgnoresTurnLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	client := &ircClient{call: func(_ context.Context, m []llm.Message, d []llm.ToolDefinition, _ func(llm.Delta) error) (llm.ToolResponse, error) {
		for _, def := range d {
			if def.Name == "end_audit" {
				t.Fatal("infinite mode exposed end tool")
			}
		}
		calls++
		if calls == 4 {
			cancel()
			return llm.ToolResponse{}, context.Canceled
		}
		return toolReply("review_state", map[string]any{}), nil
	}}
	team := newTestTeam(t, client)
	if err := team.ConfigureBudget(true, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	team.createStageLocked(phaseAudit)
	a := team.workers[0].agent
	a.cfg.Agent.MaxTurns = 1
	a.Run(ctx, "continue", func(Event) {})
	if calls != 4 || a.completed || a.tools.Audit().Ended {
		t.Fatalf("model stopped at turn limit: calls=%d completed=%v", calls, a.completed)
	}
	if result := a.requestEndAudit(context.Background(), json.RawMessage(`{"vote":"approve","summary":"stop"}`)); !strings.Contains(result, `"ok":false`) {
		t.Fatal(result)
	}
	for _, phase := range []string{phaseRecon, phaseModerator} {
		a.phase = phase
		defs := a.toolDefinitions()
		present := map[string]bool{}
		for _, d := range defs {
			present[d.Name] = true
		}
		if present["end_audit"] || present["moderator_decide"] {
			t.Fatal("completion tool leaked")
		}
		if phase == phaseRecon && !present["audit_plan_done"] {
			t.Fatal("recon handoff removed")
		}
	}
}

func TestTimeBudgetCancelsAllWorkersAndPersistsAcrossResume(t *testing.T) {
	var calls atomic.Int32
	client := &noticeClient{tools: func(ctx context.Context, _ []llm.Message) (llm.ToolResponse, error) {
		calls.Add(1)
		<-ctx.Done()
		return llm.ToolResponse{}, ctx.Err()
	}}
	team := newTestTeam(t, client)
	if err := team.ConfigureBudget(true, 0, 1, 0); err != nil {
		t.Fatal(err)
	}
	team.budget.elapsed = time.Minute - 100*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	team.Run(ctx, "fixture", func(Event) {})
	status := team.BudgetStatus()
	if ctx.Err() != nil || !strings.Contains(status.StopReason, "时间") || status.Elapsed < time.Minute || calls.Load() != 4 || status.Running || team.Phase() == "completed" {
		t.Fatalf("bad budget stop: %+v calls=%d", status, calls.Load())
	}
	path := filepath.Join(t.TempDir(), "budget.json")
	if err := team.SaveSession(path); err != nil {
		t.Fatal(err)
	}
	restored := newTestTeam(t, client)
	if err := restored.LoadSession(path); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	restored.Run(ctx, "resume", func(Event) {})
	if calls.Load() != before || restored.BudgetStatus().StopReason == "" {
		t.Fatal("resume bypassed exhausted time budget")
	}
	if err := restored.ConfigureBudget(true, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if restored.BudgetStatus().StopReason != "" || restored.BudgetStatus().Elapsed < status.Elapsed {
		t.Fatal("budget change reset usage or remained blocked")
	}
}

func TestTeamTokenBudgetStopsAtProviderUsageAndAccountsCompression(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"summary"},"finish_reason":"stop"}],"usage":{"prompt_tokens":700,"completion_tokens":300,"total_tokens":1000}}`)
	}))
	defer server.Close()
	client := llm.NewOpenAIClient(config.OpenAIConfig{BaseURL: server.URL, APIInterface: "chat_completions", APIKey: "fixture", Model: "fixture", MaxOutputTokens: 100, MaxContextTokens: 10000})
	team := newTestTeam(t, client)
	if err := team.ConfigureBudget(true, 8, 0, 1500); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	team.mu.Lock()
	team.running = true
	ctx = team.startBudgetLocked(ctx, cancel)
	team.mu.Unlock()
	messages := []llm.Message{{Role: llm.RoleUser, Content: "short input"}}
	if _, err := client.Chat(ctx, messages); err != nil {
		t.Fatal(err)
	}
	if status := team.BudgetStatus(); status.UsedTokens != 1000 || status.Estimated || status.StopReason != "" {
		t.Fatalf("first usage not exact: %+v", status)
	}
	// Compression uses the same plain Chat path, inherited observer and task budget.
	worker := tokenBudgetWorker(t, client, client)
	worker.prompts.Compress = "brief"
	worker.prompts.Templates["compress_user"] = "short input"
	err := worker.compressContext(ctx, func(Event) {}, "fixture")
	if err == nil || ctx.Err() == nil {
		t.Fatal("token budget did not cancel compressor")
	}
	status := team.BudgetStatus()
	if status.UsedTokens != 2000 || !strings.Contains(status.StopReason, "token") || status.Estimated || requests.Load() != 2 {
		t.Fatalf("bad cumulative provider usage: %+v requests=%d", status, requests.Load())
	}
	team.finishBudget()
	team.mu.Lock()
	team.running = false
	team.mu.Unlock()
}
