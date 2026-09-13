package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"code-review-agent/internal/config"
	"code-review-agent/internal/forum"
	"code-review-agent/internal/llm"
	"code-review-agent/internal/prompt"
	"code-review-agent/internal/tools"
)

type stagedClient struct {
	mu       sync.Mutex
	steps    map[string]int
	arrivals map[string]int
	gates    map[string]chan struct{}
	counts   map[string]int
	blocked  atomic.Bool
	started  chan struct{}
}

var workerIdentity = regexp.MustCompile(`([a-z]+-[0-9]+)，阶段是 ([a-z]+)`)

func newStagedClient(recon, audit int) *stagedClient {
	return &stagedClient{steps: map[string]int{}, arrivals: map[string]int{}, gates: map[string]chan struct{}{"recon": make(chan struct{}), "audit": make(chan struct{})}, counts: map[string]int{"recon": recon, "audit": audit}, started: make(chan struct{}, 32)}
}

var fixtureCallID atomic.Uint64

func toolReply(name string, args any) llm.ToolResponse {
	data, _ := json.Marshal(args)
	return llm.ToolResponse{Calls: []llm.FunctionCall{{CallID: fmt.Sprintf("fixture-%d", fixtureCallID.Add(1)), Name: name, Arguments: string(data)}}}
}
func (c *stagedClient) Chat(context.Context, []llm.Message) (string, error) {
	return "", fmt.Errorf("staged tool fixture must use native calls")
}
func (c *stagedClient) ChatTools(ctx context.Context, messages []llm.Message, _ []llm.ToolDefinition, _ func(llm.Delta) error) (llm.ToolResponse, error) {
	if c.blocked.Load() {
		c.started <- struct{}{}
		<-ctx.Done()
		return llm.ToolResponse{}, ctx.Err()
	}
	identity := workerIdentity.FindStringSubmatch(messages[0].Content)
	if len(identity) != 3 {
		return llm.ToolResponse{}, fmt.Errorf("missing worker identity")
	}
	id, stage := identity[1], identity[2]
	for _, m := range messages {
		if stage == phaseAudit && strings.Contains(m.Content, "PRIVATE_ONLY_recon-") {
			return llm.ToolResponse{}, fmt.Errorf("raw recon history leaked into audit")
		}
		if m.Type == "function_call_output" {
			data := m.Content
			if len(data) > 1024 || !json.Valid([]byte(data)) {
				return llm.ToolResponse{}, fmt.Errorf("unbounded/invalid tool output for %s: %d", id, len(data))
			}
		}
	}
	c.mu.Lock()
	step := c.steps[id]
	c.steps[id]++
	if step == 0 {
		c.arrivals[stage]++
		if c.arrivals[stage] == c.counts[stage] {
			close(c.gates[stage])
		}
	}
	gate := c.gates[stage]
	c.mu.Unlock()
	if step == 0 {
		select {
		case <-ctx.Done():
			return llm.ToolResponse{}, ctx.Err()
		case <-gate:
		}
	}
	var index int
	fmt.Sscanf(id, stage+"-%d", &index)
	path := fmt.Sprintf("entry%d.go", index)
	bufferCall := func() (llm.ToolResponse, error) {
		var raw string
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Type == "function_call_output" {
				raw = messages[i].Content
				break
			}
		}
		var p struct {
			BufferID   string `json:"buffer_id"`
			NextOffset int64  `json:"next_offset"`
		}
		json.Unmarshal([]byte(raw), &p)
		if p.BufferID == "" {
			return llm.ToolResponse{}, fmt.Errorf("large result not buffered")
		}
		return toolReply("read_tool_buffer", map[string]any{"buffer_id": p.BufferID, "offset": p.NextOffset, "limit": 128}), nil
	}
	if stage == phaseRecon {
		switch step {
		case 0:
			return toolReply("read_file", map[string]any{"path": path, "offset": 1, "limit": 10}), nil
		case 1:
			return bufferCall()
		case 2:
			response := toolReply("project_note_update", map[string]any{"note": "NOTE_" + id + strings.Repeat("结构化侦察证据", 400)})
			response.Content = "PRIVATE_ONLY_" + id
			return response, nil
		case 3:
			return toolReply("forum_post", map[string]any{"topic": "侦察交接", "content": "已定位入口 " + path}), nil
		case 4:
			return toolReply("audit_plan_done", map[string]any{"summary": "入口侦察完成", "audit_map": "MAP_" + id, "audit_files": []string{path}, "execute_instructions": "独立复核入口"}), nil
		}
	} else {
		if step >= 7 {
			return toolReply("end_audit", map[string]any{"summary": "分配入口复核完成", "vote": "approve"}), nil
		}
		switch step {
		case 0:
			return toolReply("read_handoff", map[string]any{}), nil
		case 1:
			return bufferCall()
		case 2:
			return toolReply("review_state", map[string]any{"limit": 10}), nil
		case 3:
			return toolReply("read_file", map[string]any{"path": path, "offset": 1, "limit": 10}), nil
		case 4:
			return toolReply("file_review_update", map[string]any{"path": path, "status": "reviewed", "note": "入口已读取复核"}), nil
		case 5:
			return toolReply("forum_post", map[string]any{"topic": "审计交流", "content": "已独立复核 " + path}), nil
		case 6:
			return toolReply("report_finding", map[string]any{"title": "测试入口风险", "path": path, "severity": "high", "evidence": "fixture evidence", "impact": "fixture impact"}), nil
		}
	}
	return llm.ToolResponse{}, fmt.Errorf("unexpected extra step %s:%d", id, step)
}
func (c *stagedClient) ChatStream(ctx context.Context, messages []llm.Message, emit func(llm.Delta) error) error {
	s, err := c.Chat(ctx, messages)
	if err != nil {
		return err
	}
	return emit(llm.Delta{Content: s})
}

func newTestTeam(t *testing.T, client llm.Client) *Team {
	t.Helper()
	workspace := t.TempDir()
	for i := 1; i <= 4; i++ {
		if err := os.WriteFile(filepath.Join(workspace, fmt.Sprintf("entry%d.go", i)), []byte("package fixture\n//"+strings.Repeat("中文证据", 4000)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{Workspace: workspace, OpenAI: config.OpenAIConfig{MaxContextTokens: 64000, MaxOutputTokens: 1000}, Agent: config.AgentConfig{ReconAgents: 4, AuditAgents: 4, ForumWaitSeconds: 1, MaxTurns: 20, RetryAttempts: -1, MaxToolResultChars: 1024, CompressAtRatio: .8, CompressBufferTokens: 1000}}
	disabledModerator := false
	cfg.Agent.ModeratorEnabled = &disabledModerator
	cfg.CompressOpenAI = cfg.OpenAI
	p := prompt.Prompts{System: "audit", PlanSystem: "recon", Templates: map[string]string{"initial_plan_instruction": "!{input}\n!{review_state}", "initial_audit_instruction": "!{input}\n!{review_state}", "end_audit_confirmation": "请完成剩余文件"}}
	r, err := tools.NewRegistry(workspace, 1024)
	if err != nil {
		t.Fatal(err)
	}
	team := NewTeam(cfg, p, client, client, r)
	team.nameClient = &noticeClient{call: func(context.Context, []llm.Message) (string, error) {
		return "", fmt.Errorf("naming disabled in unrelated fixture")
	}}
	t.Cleanup(func() { team.Close() })
	return team
}

func TestStageAssignmentsAreSelfOrganized(t *testing.T) {
	team := newTestTeam(t, nil)
	for _, assignment := range []string{
		stageAssignment(phaseRecon, 4),
		stageAssignment(phaseAudit, 4),
	} {
		if !strings.Contains(assignment, "系统不预设角色、主题或文件范围") {
			t.Fatalf("assignment does not require self-division: %q", assignment)
		}
		for _, forbidden := range []string{"重点：", "首要负责文件", "已将你的", "架构、入口", "认证、授权", "危险 sink"} {
			if strings.Contains(assignment, forbidden) {
				t.Fatalf("preset scope leaked into assignment %q: %q", forbidden, assignment)
			}
		}
	}
	team.mu.Lock()
	team.createStageLocked(phaseAudit)
	team.mu.Unlock()
	for _, worker := range team.workers {
		if files := worker.agent.tools.Snapshot().Files; len(files) != 0 {
			t.Fatalf("audit worker received preassigned files: %+v", files)
		}
	}
}

func TestReceiveUsesStatusOnlyForTurnsAndTools(t *testing.T) {
	team := newTestTeam(t, nil)
	a := &Agent{id: "recon-1", phase: phaseRecon, turn: 1, board: team.board, tools: team.registry}
	w := &teamWorker{agent: a, saved: workerSession{Status: WorkerStatus{ID: a.id, Phase: a.phase}}}
	team.workers = []*teamWorker{w}
	var got []Event
	team.emitter = func(e Event) { got = append(got, e) }
	team.receive(w, Event{Kind: "think_delta", Content: "PRIVATE_REASONING_SENTINEL"})
	team.receive(w, Event{Kind: "turn", Content: "ignored"})
	team.receive(w, Event{Kind: "tool", Content: "calling tool"})
	a.turn = 2
	team.receive(w, Event{Kind: "turn", Content: "ignored"})
	if len(got) != 3 || got[0].Kind != "worker" || got[0].Content != "" || got[0].Workers[0].Activity != "思考中" {
		t.Fatalf("unexpected turn/tool events: %#v", got)
	}
	if got[1].Kind != "tool" || got[1].Content != "calling tool" || got[2].Content != "" || got[2].Workers[0].Activity != "思考中" {
		t.Fatalf("unexpected transition: %#v", got)
	}
	for _, e := range got {
		if strings.Contains(e.Content, "PRIVATE_REASONING_SENTINEL") {
			t.Fatal("raw reasoning escaped")
		}
	}
}

func TestTeamStagesIsolationHandoffAndSession(t *testing.T) {
	client := newStagedClient(4, 4)
	team := newTestTeam(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session := filepath.Join(t.TempDir(), "team.json")
	team.Run(ctx, "审计测试工作区", func(e Event) {
		if e.Kind == "state" {
			if err := team.SaveSession(session); err != nil {
				t.Error(err)
			}
		}
	})
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
	if team.Phase() != "completed" {
		t.Fatalf("phase=%s statuses=%+v", team.Phase(), team.Statuses())
	}
	s := team.Snapshot()
	if !s.Audit.Ended || len(s.Findings) != 4 || len(s.Files) != 4 {
		t.Fatalf("lost independent results: %+v", s)
	}
	for _, f := range s.Files {
		if f.Status != "reviewed" {
			t.Fatal("incorrect file aggregation")
		}
	}
	if strings.Contains(team.handoff, "PRIVATE_ONLY") || !strings.Contains(team.handoff, "NOTE_recon-4") {
		t.Fatal("handoff did not preserve structured notes independently")
	}
	if err := team.SaveSession(session); err != nil {
		t.Fatal(err)
	}
	restored := newTestTeam(t, newStagedClient(4, 4))
	if err := restored.LoadSession(session); err != nil {
		t.Fatal(err)
	}
	if restored.Phase() != "completed" || len(restored.Statuses()) != 8 || len(restored.Snapshot().Findings) != 4 {
		t.Fatal("team checkpoint restore lost results")
	}
	if len(restored.ForumMessages()) != len(team.ForumMessages()) {
		t.Fatal("forum lost on restore")
	}
	for _, worker := range restored.workers {
		if strings.Contains(worker.agent.assignment, "重点：") || strings.Contains(worker.agent.assignment, "首要负责文件") {
			t.Fatalf("restored worker retained preset scope: %q", worker.agent.assignment)
		}
	}
}

func TestTeamCancellationBarrierAndResume(t *testing.T) {
	client := newStagedClient(4, 4)
	client.blocked.Store(true)
	team := newTestTeam(t, client)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); team.Run(ctx, "audit", func(Event) {}) }()
	for i := 0; i < 4; i++ {
		select {
		case <-client.started:
		case <-time.After(3 * time.Second):
			t.Fatal("workers not independently running")
		}
	}
	if err := team.SetWorkspace(t.TempDir()); err == nil {
		t.Fatal("workspace mutation allowed during active run")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel failed to drain workers")
	}
	if team.Phase() != phaseRecon || team.Snapshot().Audit.Ended {
		t.Fatal("cancel advanced to audit/completed")
	}
	for _, s := range team.Statuses() {
		if s.Status != "cancelled" {
			t.Fatalf("bad cancelled status %+v", s)
		}
	}
	client.blocked.Store(false)
	resumeCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	team.Run(resumeCtx, "继续", func(Event) {})
	if team.Phase() != "completed" {
		t.Fatalf("resume failed: %+v", team.Statuses())
	}
}

func TestAgentSpecialToolsInvalidateLastBuffer(t *testing.T) {
	team := newTestTeam(t, newStagedClient(4, 4))
	board := forum.New(time.Second)
	board.Register("audit-1", phaseAudit)
	if err := board.RegisterName("audit-1", "Buffer tester"); err != nil {
		t.Fatal(err)
	}
	a := newWorker(team.cfg, team.prompts, team.client, team.client, team.registry.Fork(), "audit-1", phaseAudit, board)
	defer a.tools.Close()
	for _, name := range []string{"forum_roster", "read_handoff", "load_skill", "verify_finding"} {
		var p struct {
			BufferID string `json:"buffer_id"`
		}
		json.Unmarshal([]byte(a.tools.BoundResult("read_file", `{"ok":true,"data":"`+strings.Repeat("a", 5000)+`"}`)), &p)
		a.boundedText("context maintenance", strings.Repeat("x", 5000))
		args, _ := json.Marshal(map[string]any{"buffer_id": p.BufferID})
		if !strings.Contains(a.tools.Call("read_tool_buffer", args), `"ok":true`) {
			t.Fatal("context maintenance invalidated tool buffer")
		}
		a.callTool(context.Background(), func(Event) {}, ToolCall{Name: name, Arguments: json.RawMessage(`{}`)})
		if strings.Contains(a.tools.Call("read_tool_buffer", args), `"ok":true`) {
			t.Fatalf("%s kept prior buffer", name)
		}
	}
}
