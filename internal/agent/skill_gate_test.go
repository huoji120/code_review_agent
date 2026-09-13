package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"code-review-agent/internal/llm"
	"code-review-agent/internal/prompt"
)

// A nonempty skill catalog is not proof that any skill applies to the target.
// Exercise the real HTTP boundary and complete a native recon handoff without
// loading an unrelated skill; this previously hid read tools and paused the team.
func TestReconNativeHTTPWithUnloadedSkillsCanInspectAndComplete(t *testing.T) {
	sequence := []struct{ name, args string }{
		{"review_state", `{"limit":1}`},
		{"list_files", `{"limit":1,"max_depth":1}`},
		{"read_file", `{"path":"entry1.go","limit":1}`},
		{"audit_plan_done", `{"summary":"Entry source inspected; catalog skills do not apply","audit_map":"entry1.go","audit_files":["entry1.go"]}`},
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []llm.ToolDefinition                    `json:"tools"`
			Input []struct{ Type, CallID, Output string } `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, err.Error(), 400)
			return
		}
		available := map[string]bool{}
		for _, tool := range body.Tools {
			available[tool.Name] = true
		}
		for _, name := range []string{"review_state", "list_files", "read_file", "audit_plan_done", "load_skill"} {
			if !available[name] {
				t.Errorf("unloaded skill incorrectly hides native %s", name)
			}
		}
		for _, name := range []string{"report_finding", "end_audit", "moderator_decide"} {
			if available[name] {
				t.Errorf("recon gained forbidden native %s", name)
			}
		}
		for _, item := range body.Input {
			if item.Type == "function_call_output" {
				var result struct {
					OK    bool   `json:"ok"`
					Error string `json:"error"`
				}
				if err := json.Unmarshal([]byte(item.Output), &result); err != nil || !result.OK {
					t.Errorf("valid recon tool rejected: %s (%v)", item.Output, err)
				}
			}
		}
		step := requests
		requests++
		if step >= len(sequence) {
			http.Error(w, "unexpected retry", 400)
			return
		}
		call := sequence[step]
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "function_call", "id": fmt.Sprintf("item_%d", step), "call_id": fmt.Sprintf("call_%d", step), "name": call.name, "arguments": call.args}}})
	}))
	defer server.Close()
	team := newTestTeam(t, nil)
	team.prompts.Skills = []prompt.Skill{{Name: "web-only", Content: "Use only for HTTP application security; not applicable to this native source fixture."}}
	cfg := team.cfg.OpenAI
	cfg.BaseURL = server.URL
	cfg.APIInterface = "responses"
	cfg.APIKey = "local-fixture"
	cfg.Stream = false
	cfg.TimeoutSeconds = 2
	client := llm.NewOpenAIClient(cfg)
	team.client = client
	team.createStageLocked(phaseRecon)
	a := team.workers[0].agent
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.Run(ctx, "Inspect the entry source without loading unrelated skills", func(Event) {})
	if a.runErr != nil || !a.completed || requests != len(sequence) {
		t.Fatalf("native recon did not complete: requests=%d completed=%v error=%v", requests, a.completed, a.runErr)
	}
	if len(a.prompts.LoadedSkillNames()) != 0 {
		t.Fatal("unrelated skill was force-loaded")
	}
	if err := validateNativeHistory(a.messages); err != nil {
		t.Fatal(err)
	}
	var final struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(a.messages[len(a.messages)-1].Content), &final); err != nil || !final.OK {
		t.Fatalf("handoff blocked without skill: %+v %v", final, err)
	}
}
