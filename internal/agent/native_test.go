package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"code-review-agent/internal/llm"
)

func TestNativeLoopRejectsProtocolFailuresWithoutExecutingTextOrBatches(t *testing.T) {
	cases := map[string]llm.ToolResponse{
		"text":       {Content: `<tool_call>{"name":"forum_post","arguments":{"content":"unsafe"}}</tool_call>`, Thinking: "example is not execution"},
		"missing_id": {Calls: []llm.FunctionCall{{Name: "forum_post", Arguments: `{"content":"unsafe"}`}}},
		"malformed":  {Calls: []llm.FunctionCall{{CallID: "bad", Name: "forum_post", Arguments: `{"content":`}}},
		"multiple":   {Calls: []llm.FunctionCall{{CallID: "one", Name: "forum_post", Arguments: `{"content":"unsafe"}`}, {CallID: "two", Name: "forum_post", Arguments: `{"content":"unsafe"}`}}},
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			calls := 0
			client := &noticeClient{tools: func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
				calls++
				if calls > 1 {
					feedback := messages[len(messages)-1]
					if feedback.Role != llm.RoleUser || !strings.Contains(feedback.Content, "function_call") || !strings.Contains(feedback.Content, "arguments={}") {
						t.Fatal("protocol retry omitted concrete native correction")
					}
				}
				return response, nil
			}}
			team := newTestTeam(t, client)
			team.createStageLocked(phaseRecon)
			a := team.workers[0].agent
			a.Run(context.Background(), "inspect", func(Event) {})
			if calls != 3 || !errors.Is(a.runErr, ErrToolProtocolFailures) || len(a.board.Messages()) != 0 {
				t.Fatalf("protocol gate calls=%d err=%v", calls, a.runErr)
			}
			if err := validateNativeHistory(a.messages); err != nil {
				t.Fatal(err)
			}
			for _, message := range a.messages {
				if message.Type != "" {
					t.Fatal("invalid batch became native history")
				}
			}
			if !strings.Contains(a.messages[len(a.messages)-1].Content, "3/3") {
				t.Fatal("third corrective feedback was not retained")
			}
		})
	}
}

func TestNativeUnauthorizedCallPairsErrorAndValidCallResetsStreak(t *testing.T) {
	calls := 0
	client := &noticeClient{}
	team := newTestTeam(t, client)
	team.createStageLocked(phaseRecon)
	a := team.workers[0].agent
	a.cfg.Agent.MaxTurns = 2
	client.tools = func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		calls++
		switch calls {
		case 1:
			return toolReply("moderator_idle", map[string]any{}), nil
		case 2:
			if err := validateNativeHistory(messages); err != nil {
				t.Fatal(err)
			}
			paired := false
			for _, m := range messages {
				if m.Type == "function_call_output" && strings.Contains(m.Content, `"ok":false`) {
					paired = true
				}
			}
			if !paired {
				t.Fatal("unauthorized native call has no paired error")
			}
			return toolReply("read_file", map[string]any{"path": "missing-file.go"}), nil
		case 3, 4:
			return llm.ToolResponse{Thinking: "still selecting"}, nil
		default:
			return toolReply("read_file", map[string]any{"path": "entry1.go", "limit": 1}), nil
		}
	}
	a.Run(context.Background(), "inspect", func(Event) {})
	if calls != 5 || errors.Is(a.runErr, ErrToolProtocolFailures) || a.protocolFailures != 0 {
		t.Fatalf("valid business error did not reset streak: calls=%d err=%v streak=%d", calls, a.runErr, a.protocolFailures)
	}
	if err := validateNativeHistory(a.messages); err != nil {
		t.Fatal(err)
	}
}

func TestNativeRoleDefinitionsMatchExecutionBoundary(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	for _, role := range []string{phaseRecon, phaseAudit, phaseModerator, "verifier"} {
		phase := role
		if role == "verifier" {
			phase = phaseAudit
		}
		a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), role, phase, team.board)
		defer a.tools.Close()
		a.verifying = role == "verifier"
		names := map[string]bool{}
		for _, d := range a.toolDefinitions() {
			names[d.Name] = true
			if !json.Valid(d.Parameters) {
				t.Fatalf("invalid schema %s", d.Name)
			}
		}
		if !names["read_file"] || !names["read_handoff"] {
			t.Fatalf("%s cannot inspect evidence", role)
		}
		if names["moderator_idle"] != (role == phaseModerator) || names["forum_announce"] != (role == phaseModerator) || names["forum_moderate"] != (role == phaseModerator) {
			t.Fatalf("moderator privilege leaked to %s", role)
		}
		if names["audit_plan_done"] != (role == phaseRecon) || names["report_finding"] != (role == phaseAudit) || names["end_audit"] != (role == phaseAudit) || names["verify_finding"] != (role == phaseAudit) {
			t.Fatalf("role boundary leaked: %s %v", role, names)
		}
	}
}

func TestNativeResultsTraceRestoreAndCompressionPreserveEvidence(t *testing.T) {
	client := &noticeClient{tools: func(context.Context, []llm.Message) (llm.ToolResponse, error) {
		return toolReply("read_file", map[string]any{"path": "entry1.go", "limit": 10}), nil
	}}
	team := newTestTeam(t, client)
	team.createStageLocked(phaseRecon)
	a := team.workers[0].agent
	a.cfg.Agent.MaxTurns = 1
	a.cfg.Agent.LogSession = true
	a.cfg.Agent.LogSessionDir = t.TempDir()
	a.Run(context.Background(), "inspect", func(Event) {})
	if err := validateNativeHistory(a.messages); err != nil {
		t.Fatal(err)
	}
	var native []llm.Message
	for _, m := range a.messages {
		if m.Type != "" {
			native = append(native, m)
		}
	}
	if len(native) != 2 || native[0].CallID != native[1].CallID || !strings.Contains(native[1].Content, "buffer_id") {
		t.Fatal("bounded native result lost pairing/buffer")
	}
	raw, err := os.ReadFile(a.TracePath())
	if err != nil {
		t.Fatal(err)
	}
	var trace []llm.Message
	if err = json.Unmarshal(raw, &trace); err != nil {
		t.Fatal(err)
	}
	full := false
	for _, m := range trace {
		if m.Type == "function_call_output" && m.CallID == native[0].CallID && strings.Contains(m.Content, strings.Repeat("中文证据", 1000)) {
			full = true
		}
	}
	if !full {
		t.Fatal("trace omitted full result or native call id")
	}
	session := filepath.Join(t.TempDir(), "native.json")
	if err = team.SaveSession(session); err != nil {
		t.Fatal(err)
	}
	restored := newTestTeam(t, client)
	if err = restored.LoadSession(session); err != nil {
		t.Fatal(err)
	}
	b := restored.workers[0].agent
	var restoredNative []llm.Message
	for _, m := range b.messages {
		if m.Type != "" {
			restoredNative = append(restoredNative, m)
		}
	}
	if !reflect.DeepEqual(native, restoredNative) {
		t.Fatal("restore altered exact native records")
	}
	b.phase = phaseModerator
	b.prompts.Templates["compress_user"] = "!{state_and_conversation}"
	summaryCalls := 0
	b.compressClient = &noticeClient{call: func(_ context.Context, messages []llm.Message) (string, error) {
		summaryCalls++
		for _, m := range messages {
			if m.Type != "" {
				t.Fatal("summarizer received executable tool items")
			}
		}
		if !strings.Contains(messages[1].Content, native[0].CallID) || !strings.Contains(messages[1].Content, "read_file") {
			t.Fatal("summarizer lost native evidence")
		}
		return "read_file evidence retained", nil
	}}
	if err = b.compressContext(context.Background(), func(Event) {}, "native history"); err != nil {
		t.Fatal(err)
	}
	if summaryCalls != 1 || !strings.Contains(b.messages[0].Content, "论坛管理员") {
		t.Fatal("moderator compression lost role")
	}
	for _, m := range b.messages {
		if m.Type != "" {
			t.Fatal("compaction retained orphan native records")
		}
	}
}
