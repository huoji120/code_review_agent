package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"code-review-agent/internal/llm"
)

func TestReconNativeHTTPCanReadGitHistoryAndHandoff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is required")
	}
	team := newTestTeam(t, nil)
	git := func(args ...string) {
		command := exec.Command("git", append([]string{"-c", "user.name=ReconFixture", "-c", "user.email=recon@example.invalid", "-c", "core.hooksPath=.no-hooks", "-c", "commit.gpgsign=false"}, args...)...)
		command.Dir = team.registry.Workspace()
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("fixture git: %v %s", err, out)
		}
	}
	git("init", "-q")
	git("add", "--", "entry1.go")
	git("commit", "-qm", "RECON_HISTORY_EVIDENCE")
	requests := 0
	sawHistory := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []llm.ToolDefinition `json:"tools"`
			Input []struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Output string `json:"output"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		available := false
		for _, tool := range body.Tools {
			if tool.Name == "git_inspect" {
				available = true
			}
			if tool.Name == "report_finding" || tool.Name == "verify_finding" || tool.Name == "end_audit" {
				t.Errorf("recon gained forbidden tool %s", tool.Name)
			}
		}
		if !available {
			t.Error("recon model cannot select git_inspect: not advertised")
			http.Error(w, "git_inspect unavailable to reconnaissance", 400)
			return
		}
		step := requests
		requests++
		name, args := "git_inspect", `{"action":"log","limit":1}`
		if step == 1 {
			for _, item := range body.Input {
				if item.Type == "function_call_output" && item.CallID == "git_0" {
					var result struct {
						OK   bool `json:"ok"`
						Data struct {
							Output string `json:"output"`
						} `json:"data"`
					}
					if err := json.Unmarshal([]byte(item.Output), &result); err != nil || !result.OK || !strings.Contains(result.Data.Output, "RECON_HISTORY_EVIDENCE") {
						t.Errorf("real Git evidence not delivered: %s", item.Output)
					} else {
						sawHistory = true
					}
				}
			}
			name, args = "audit_plan_done", `{"summary":"History inspected; deeper verification deferred","audit_map":"entry1.go and its initial commit","audit_files":["entry1.go"]}`
		} else if step > 1 {
			http.Error(w, "unexpected retry", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "function_call", "id": fmt.Sprintf("item_%d", step), "call_id": fmt.Sprintf("git_%d", step), "name": name, "arguments": args}}})
	}))
	defer server.Close()
	cfg := team.cfg.OpenAI
	cfg.BaseURL = server.URL
	cfg.APIInterface = "responses"
	cfg.APIKey = "local-fixture"
	cfg.Stream = false
	cfg.TimeoutSeconds = 2
	team.client = llm.NewOpenAIClient(cfg)
	team.createStageLocked(phaseRecon)
	a := team.workers[0].agent
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	a.Run(ctx, "Inspect relevant Git history during reconnaissance", func(Event) {})
	if a.runErr != nil || !a.completed || requests != 2 || !sawHistory {
		t.Fatalf("Git recon did not complete: requests=%d history=%v completed=%v error=%v", requests, sawHistory, a.completed, a.runErr)
	}
	if err := validateNativeHistory(a.messages); err != nil {
		t.Fatal(err)
	}
}
