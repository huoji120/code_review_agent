package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"code-review-agent/internal/config"
	"code-review-agent/internal/forum"
	"code-review-agent/internal/llm"
	"code-review-agent/internal/prompt"
	"code-review-agent/internal/tools"
)

type Agent struct {
	cfg               config.Config
	prompts           prompt.Prompts
	client            llm.Client
	compressClient    llm.Client
	nameClient        llm.Client
	announcedName     string
	tools             *tools.Registry
	phase             string
	messages          []llm.Message
	pendingEndAudit   bool
	trace             *traceLog
	tracePath         string
	traceErr          error
	traceBootstrapped bool
	id                string
	board             *forum.Board
	assignment        string
	handoff           string
	plan              *auditPlanDoneArgs
	completed         bool
	runErr            error
	turn              int
	forumCursor       int64
	forumPending      string
	checkpoint        func(*Agent)
}

const (
	phaseRecon = "recon"
	phaseAudit = "audit"
)

type Event struct {
	Kind         string
	Content      string
	Phase        string
	Skills       []string
	VerifyTitle  string
	VerifyTurn   int
	VerifyLimit  int
	VerifyStatus string
	Todos        []tools.Todo
	Findings     []tools.Finding
	Project      tools.ProjectNote
	Files        []tools.FileReview
	Variables    []tools.VariableReview
	Flows        []tools.FlowReview
	Audit        tools.AuditState
	AgentID      string
	Workers      []WorkerStatus
	Forum        *forum.Message
}

type ToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func newWorker(cfg config.Config, prompts prompt.Prompts, client, compressClient llm.Client, registry *tools.Registry, id, phase string, board *forum.Board) *Agent {
	if compressClient == nil {
		compressClient = client
	}
	prompts.SetLoadedSkills(prompts.LoadedSkillNames())
	return &Agent{cfg: cfg, prompts: prompts, client: client, compressClient: compressClient, tools: registry, id: id, phase: phase, board: board}
}

func (a *Agent) sanitizeMessages() {
	if len(a.messages) == 0 {
		a.messages = []llm.Message{{Role: llm.RoleSystem, Content: a.systemPrompt()}}
		return
	}
	var cleaned []llm.Message
	hasSystem := false
	for _, msg := range a.messages {
		if msg.Role == llm.RoleSystem {
			if hasSystem {
				continue
			}
			hasSystem = true
		}
		cleaned = append(cleaned, msg)
	}
	if len(cleaned) == 0 || cleaned[0].Role != llm.RoleSystem {
		cleaned = append([]llm.Message{{Role: llm.RoleSystem, Content: a.systemPrompt()}}, cleaned...)
	} else {
		cleaned[0] = llm.Message{Role: llm.RoleSystem, Content: a.systemPrompt()}
	}
	a.messages = cleaned
}

func (a *Agent) systemPrompt() string {
	if a.phase == phaseRecon {
		return a.planSystemPrompt() + "\n\n" + a.collaborationPrompt()
	}
	system := a.prompts.SystemWithSkills()
	system += "\n\n" + a.tools.GitPrompt()
	system += "\n\n" + a.tools.ToolPrompt()
	system += "\n\n" + a.skillToolPrompt()
	if a.cfg.Agent.AutoPlan {
		system += "\n\n" + a.render("auto_plan", nil)
	}
	system += "\n\n" + a.render("tool_protocol_guard", nil)
	return system + "\n\n" + a.collaborationPrompt()
}

func (a *Agent) planSystemPrompt() string {
	system := a.prompts.PlanSystemWithSkills()
	system += "\n\n" + a.planWorkspacePrompt()
	system += "\n\n" + a.planToolPrompt()
	system += "\n\n" + a.skillToolPrompt()
	system += "\n\n" + a.render("tool_protocol_guard", nil)
	return system
}

func (a *Agent) planWorkspacePrompt() string {
	return "# 当前工作区\n\n- 工作区：" + filepath.ToSlash(a.tools.Workspace()) + "\n- 规划阶段不会暴露 Git 审计工具；如需增量审计、blame 或 diff，必须等切换到执行阶段后再使用。"
}

func (a *Agent) planToolPrompt() string {
	return `# 规划阶段工具协议

每次只能调用一个工具。工具调用必须使用下面格式：

<tool_call>
{"name":"tool_name","arguments":{"key":"value"}}
</tool_call>

规划阶段只允许使用这些工具：

- review_state：查看当前文件清单、todo、项目笔记、文件地图、变量和 flow 状态。参数：limit。
- list_files：按目录、深度或模式补充文件地图。参数：root、pattern、max_depth、include_hidden、limit。
- read_file：只读取配置、入口、路由、鉴权、依赖描述等少量关键文件用于建图，不做漏洞结论。参数：path、offset、limit。
- search_content：搜索用于建图的关键词，例如 route、controller、auth、upload、admin、plugin、template、config、action。参数：query、mode、root、include、limit、case_insensitive、case_sensitive。mode 支持 literal、regex、fuzzy；literal 是默认模式，query 按普通字符串包含搜索，不解析 .*、|、\b 等正则语法；使用正则语法时必须显式传 mode:"regex"。
- todo_create：创建执行阶段必须审计的具体 todo。todo 必须绑定地图优先级、具体文件/模块/入口/变量/审计点。
- todo_update：修正规划阶段 todo。参数：id、status、title、priority。
- file_review_update：绘制本次 one-shot 文件地图。文件排查默认为空，必须由你显式选择文件加入。支持 path 单文件、paths 多文件、dir/dirs + suffix/suffixes、pattern/patterns 从本地 inventory 批量加入。只能把文件标记为 reviewing 或 skipped，不要在规划阶段标记 reviewed。note 写明为什么纳入 one-shot 审计范围或为什么跳过。
- project_note_update：更新项目级详细自由文本笔记。参数：note。必须像人工审计员工作笔记一样尽量详细，主动记录项目架构、运行行为、登录认证、鉴权机制、攻击面、数据/状态流、关键文件角色、已知结论和待确认问题；每次获得新信息后都应更新，不要只写摘要。
- audit_plan_done：提交你自己的侦察交接资料并结束本 worker；所有侦察 Agent 完成后才启动独立审计团队。参数：summary、audit_map、audit_files、execute_instructions。audit_files 必须是具体文件路径列表。
- load_skill：按需加载 skill。参数：name。
- read_tool_buffer：按需读取超长工具结果。参数：buffer_id、offset、limit；offset/limit 是 UTF-8 字节，下一页使用 next_offset，不要猜偏移。预览不是全部结果。

规划阶段禁止调用 verify_finding、report_finding、end_audit、flow_review_update、flow_review_delete、variable_review_update。规划阶段不能提交漏洞、不能结束审计、不能把猜测当证据。`
}

func (a *Agent) skillToolPrompt() string {
	if len(a.prompts.Skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Skill Loading\n\n")
	b.WriteString("如需加载 skill，调用：<tool_call>{\"name\":\"load_skill\",\"arguments\":{\"name\":\"skill-name\"}}</tool_call>\n")
	b.WriteString("你可以按需加载多个不同 skill 并组合使用；同一个 skill 不能重复加载。只有在当前任务明确需要时才加载。可用 skills：\n")
	for _, skill := range a.prompts.Skills {
		b.WriteString("- ")
		b.WriteString(skill.Name)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func (a *Agent) render(name string, vars map[string]string) string {
	out := a.prompts.RenderTemplate(name, vars)
	if out == "" {
		return ""
	}
	return strings.TrimSpace(out)
}

func (a *Agent) Phase() string {
	if a.phase == "" {
		return phaseRecon
	}
	return a.phase
}

func (a *Agent) TracePath() string {
	if a.trace != nil {
		return a.trace.Path()
	}
	return a.tracePath
}

func (a *Agent) ensureTrace() (*traceLog, error) {
	if !a.cfg.Agent.LogSession {
		return nil, nil
	}
	if a.trace != nil {
		return a.trace, nil
	}
	var (
		log *traceLog
		err error
	)
	if a.tracePath != "" {
		log, err = resumeTraceLog(a.tracePath)
	} else {
		log, err = newTraceLog(a.cfg.Agent.LogSessionDir)
	}
	if err != nil {
		a.traceErr = err
		return nil, err
	}
	a.trace = log
	a.tracePath = log.Path()
	a.traceErr = nil
	return log, nil
}

func (a *Agent) bootstrapTrace() {
	log, err := a.ensureTrace()
	if err != nil || log == nil || a.traceBootstrapped {
		return
	}
	if log.IsNewFile() {
		for _, msg := range a.messages {
			if err := log.AppendMessage(msg); err != nil {
				a.traceErr = err
				return
			}
		}
	}
	a.traceBootstrapped = true
}

func (a *Agent) appendTraceMessage(message llm.Message) {
	if !a.cfg.Agent.LogSession {
		return
	}
	if !a.traceBootstrapped {
		a.bootstrapTrace()
	}
	log, err := a.ensureTrace()
	if err != nil || log == nil {
		return
	}
	if err := log.AppendMessage(message); err != nil {
		a.traceErr = err
	}
}

func (a *Agent) Run(ctx context.Context, input string, emit func(Event)) {
	a.runErr = nil
	a.completed = false
	a.sanitizeMessages()
	a.bootstrapTrace()
	defer a.emitState(emit)
	defer a.startNaming(ctx, emit)()
	initialTemplate := "initial_audit_instruction"
	if a.phase == phaseRecon {
		initialTemplate = "initial_plan_instruction"
	}
	initial := a.render(initialTemplate, map[string]string{"input": input, "review_state": "工作区已在本地索引。调用 review_state 获取文件地图、分配范围和当前状态；超长结果按需用 read_tool_buffer 读取。"})
	if a.handoff != "" {
		initial += "\n\n已有前一阶段结构化交接，请调用 read_handoff 按需读取完整地图、笔记、todo 与未决问题；不要假设已看过原始侦察对话。"
	}
	a.addMessage(llm.Message{Role: llm.RoleUser, Content: initial})
	a.emitState(emit)
	for turn := 1; ; turn++ {
		if ctx.Err() != nil {
			a.runErr = ctx.Err()
			return
		}
		if a.cfg.Agent.MaxTurns > 0 && turn > a.cfg.Agent.MaxTurns {
			a.runErr = fmt.Errorf("达到当前 worker 的 max_turns，阶段未完成；可用 go 继续")
			emit(Event{Kind: "error", Content: a.runErr.Error()})
			return
		}
		a.turn++
		emit(Event{Kind: "turn"})
		// Request preparation injects notices before checking the context budget.
		answer, err := a.chatStream(ctx, emit)
		if err != nil {
			a.runErr = err
			if !errors.Is(err, context.Canceled) {
				emit(Event{Kind: "error", Content: err.Error()})
			}
			return
		}
		a.addMessage(llm.Message{Role: llm.RoleAssistant, Content: answer})
		emit(Event{Kind: "assistant_done"})
		call, ok := parseToolCall(answer)
		if !ok {
			a.addMessage(llm.Message{Role: llm.RoleUser, Content: a.render("no_tool_retry", nil)})
			continue
		}
		if call.Name != "read_tool_buffer" {
			a.tools.ClearBuffer()
		}
		if correction := a.phaseToolCorrection(call.Name); correction != "" {
			a.addMessage(llm.Message{Role: llm.RoleUser, Content: correction})
			continue
		}
		if call.Name == "end_audit" {
			if blocker := a.endAuditNeedsConfirmation(); blocker != "" {
				a.addMessage(llm.Message{Role: llm.RoleUser, Content: a.boundedText("end_audit_confirmation", blocker)})
				continue
			}
		} else {
			a.pendingEndAudit = false
		}
		emit(Event{Kind: "tool", Content: "calling " + call.Name})
		result, fullResult := a.callTool(ctx, emit, call)
		a.messages = append(a.messages, llm.Message{Role: llm.RoleUser, Content: "Tool result for " + call.Name + ":\n" + result})
		a.appendTraceMessage(llm.Message{Role: llm.RoleUser, Content: "Tool result for " + call.Name + ":\n" + fullResult})
		if !tools.IsKnownTool(call.Name) && !forum.IsTool(call.Name) && call.Name != "read_handoff" {
			a.addMessage(llm.Message{Role: llm.RoleUser, Content: a.unknownToolCorrection(call.Name)})
		}
		if call.Name == "audit_plan_done" && a.plan != nil && auditPlanAccepted(fullResult) {
			a.completed = true
		}
		if call.Name == "end_audit" && a.tools.Audit().Ended && auditPlanAccepted(fullResult) {
			a.completed = true
		}
		a.emitState(emit)
		if a.completed {
			return
		}
	}
}

func (a *Agent) phaseToolCorrection(name string) string {
	if forum.IsTool(name) || name == "read_tool_buffer" || name == "read_handoff" {
		return ""
	}
	if a.phase == phaseRecon {
		if len(a.prompts.Skills) > 0 && len(a.prompts.LoadedSkillNames()) == 0 && name != "load_skill" {
			return "当前处于规划建图阶段，且尚未加载任何 skill。首次启动审计必须先根据项目文件类型和 Inventory 摘要选择并调用 load_skill 加载至少一个 skill，然后才能继续 review_state/list_files/read_file/search_content/todo/file_review/audit_plan_done。下一条回复只能输出一个 load_skill 的 <tool_call> JSON。"
		}
		switch name {
		case "review_state", "list_files", "read_file", "search_content", "search_context", "todo_create", "todo_update", "file_review_update", "project_note_update", "audit_plan_done", "load_skill":
			return ""
		default:
			return "当前处于规划建图阶段，禁止调用 " + name + "。你必须继续绘制审计地图、创建具体 todo、标记本次 one-shot 要审计的文件；规划完成后只能调用 audit_plan_done 切换到执行阶段。下一条回复只能输出一个合法 <tool_call> JSON。"
		}
	}
	if name == "audit_plan_done" {
		return "当前已经处于执行审计阶段，不能再次调用 audit_plan_done。请按规划阶段产出的审计地图继续 read_file/search_content/flow_review_update/verify_finding/report_finding。下一条回复只能输出一个合法 <tool_call> JSON。"
	}
	return ""
}

func (a *Agent) unknownToolCorrection(name string) string {
	var b strings.Builder
	b.WriteString("你刚才调用了不存在的工具：")
	b.WriteString(name)
	b.WriteString("。下一条回复必须只输出一个裸的 <tool_call> JSON，且 name 必须从下面可用工具列表中选择；不要继续调用不存在的工具，不要解释，不要输出 markdown。\n\n")
	b.WriteString("请重新阅读 system prompt 中的工具列表，当前可用工具如下：\n\n")
	b.WriteString(a.tools.ToolPrompt())
	return b.String()
}

func (a *Agent) callTool(ctx context.Context, emit func(Event), call ToolCall) (string, string) {
	if call.Name != "read_tool_buffer" {
		a.tools.ClearBuffer()
	}
	var result string
	switch {
	case forum.IsTool(call.Name):
		if a.board == nil {
			result = `{"ok":false,"error":"forum unavailable"}`
		} else {
			if call.Name == "forum_wait" {
				a.board.SetStatus(a.id, "waiting")
				emit(Event{Kind: "waiting", Content: "等待论坛回复"})
			}
			result = a.board.Call(ctx, a.id, a.phase, call.Name, call.Arguments)
			if call.Name == "forum_wait" {
				a.board.SetStatus(a.id, "running")
				emit(Event{Kind: "worker", Content: "论坛等待结束"})
			}
		}
	case call.Name == "read_handoff":
		result = a.handoff
		if result == "" {
			result = `{"ok":false,"error":"no prior stage handoff"}`
		}
	case call.Name == "load_skill":
		result = a.loadSkill(call.Arguments)
	case call.Name == "audit_plan_done":
		result = a.auditPlanDone(call.Arguments)
	case call.Name == "verify_finding":
		result = a.verifyFinding(ctx, emit, call.Arguments)
	default:
		return a.tools.CallWithFullResult(ctx, call.Name, call.Arguments)
	}
	return a.tools.BoundResult(call.Name, result), result
}

type auditPlanDoneArgs struct {
	Summary             string   `json:"summary"`
	AuditMap            string   `json:"audit_map"`
	AuditFiles          []string `json:"audit_files"`
	ExecuteInstructions string   `json:"execute_instructions"`
}

func (a *Agent) auditPlanDone(raw json.RawMessage) string {
	if len(a.prompts.Skills) > 0 && len(a.prompts.LoadedSkillNames()) == 0 {
		data, _ := json.MarshalIndent(tools.Result{OK: false, Error: "plan phase must load at least one skill before audit_plan_done"}, "", "  ")
		return string(data)
	}
	args, err := decodeAuditPlanDoneArgs(raw)
	if err != nil {
		data, _ := json.MarshalIndent(tools.Result{OK: false, Error: err.Error()}, "", "  ")
		return string(data)
	}
	applied := a.tools.ApplyAuditScope(args.AuditFiles)
	if len(applied) == 0 {
		data, _ := json.MarshalIndent(tools.Result{OK: false, Error: "audit_files did not match any files in the current workspace; use exact relative paths from review_state/list_files/search_content"}, "", "  ")
		return string(data)
	}
	args.AuditFiles = applied
	a.plan = &args
	data, _ := json.MarshalIndent(tools.Result{OK: true, Data: args, Message: "侦察交接已提交；等待其余侦察 Agent 完成后启动审计团队"}, "", "  ")
	return string(data)
}

func auditPlanAccepted(result string) bool {
	var parsed struct {
		OK bool `json:"ok"`
	}
	return json.Unmarshal([]byte(result), &parsed) == nil && parsed.OK
}

func decodeAuditPlanDoneArgs(raw json.RawMessage) (auditPlanDoneArgs, error) {
	var args auditPlanDoneArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return args, err
	}
	if strings.TrimSpace(args.Summary) == "" {
		return args, fmt.Errorf("summary is required")
	}
	if strings.TrimSpace(args.AuditMap) == "" {
		return args, fmt.Errorf("audit_map is required")
	}
	if len(args.AuditFiles) == 0 {
		return args, fmt.Errorf("audit_files must include concrete files selected for execute phase")
	}
	return args, nil
}

type verifyFindingArgs struct {
	Severity       string `json:"severity"`
	Title          string `json:"title"`
	Path           string `json:"path"`
	Line           int    `json:"line"`
	Evidence       string `json:"evidence"`
	Impact         string `json:"impact"`
	Recommendation string `json:"recommendation"`
	CWE            string `json:"cwe"`
}

func (a *Agent) loadSkill(raw json.RawMessage) string {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		data, _ := json.MarshalIndent(tools.Result{OK: false, Error: err.Error()}, "", "  ")
		return string(data)
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		data, _ := json.MarshalIndent(tools.Result{OK: false, Error: "name is required"}, "", "  ")
		return string(data)
	}
	if !a.prompts.HasSkill(name) {
		data, _ := json.MarshalIndent(tools.Result{OK: false, Error: "unknown skill: " + name}, "", "  ")
		return string(data)
	}
	loaded := a.prompts.LoadSkill(name)
	result := tools.Result{OK: true, Data: map[string]any{"name": name, "loaded": loaded, "skills": a.prompts.LoadedSkillNames()}}
	if loaded {
		for _, skill := range a.prompts.LoadedSkills() {
			if skill.Name == name {
				result.Message = "skill loaded"
				result.Data = map[string]any{"name": name, "loaded": true, "skills": a.prompts.LoadedSkillNames(), "content": skill.Content}
				break
			}
		}
	} else {
		result.Message = "skill already loaded"
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	return string(data)
}

func (a *Agent) verifyFinding(ctx context.Context, emit func(Event), raw json.RawMessage) string {
	args, err := decodeVerifyFindingArgs(raw)
	if err != nil {
		data, _ := json.MarshalIndent(tools.Result{OK: false, Error: err.Error()}, "", "  ")
		return string(data)
	}
	registry := a.tools.Fork()
	defer registry.Close()
	registry.RestoreSnapshot(a.tools.Snapshot())
	childPrompts := a.prompts
	childPrompts.SetLoadedSkills(a.prompts.LoadedSkillNames())
	child := newWorker(a.cfg, childPrompts, a.client, a.compressClient, registry, fmt.Sprintf("%s-verify-%d", a.id, a.turn), phaseAudit, a.board)
	child.nameClient = a.nameClient
	child.assignment = "独立复核候选漏洞，论坛内容只作为线索，必须亲自读取源码验证。read_handoff 包含完整候选证据和父 Agent 审计状态；摘要未显示的证据必须按需读取。"
	var prior json.RawMessage
	if a.handoff != "" {
		prior = json.RawMessage(a.handoff)
	}
	handoff, _ := json.Marshal(tools.Result{OK: true, Data: map[string]any{"candidate": args, "parent_state": a.tools.Snapshot(), "recon_handoff": prior}})
	child.handoff = string(handoff)
	if a.board != nil {
		a.board.Register(child.id, phaseAudit)
	}
	child.messages = []llm.Message{{Role: llm.RoleSystem, Content: child.systemPrompt()}}
	if emit != nil {
		emit(Event{Kind: "verify_progress", VerifyTitle: args.Title, VerifyTurn: 0, VerifyLimit: child.verificationTurnLimit(), VerifyStatus: "准备验证"})
	}
	conclusion, err := child.runVerification(ctx, emit, args)
	if a.board != nil {
		status := "completed"
		if err != nil {
			status = "failed"
		}
		if ctx.Err() != nil {
			status = "cancelled"
		}
		a.board.SetStatus(child.id, status)
	}
	if emit != nil {
		status := "验证完成"
		if err != nil {
			status = "验证失败"
		}
		emit(Event{Kind: "verify_done", VerifyTitle: args.Title, VerifyLimit: child.verificationTurnLimit(), VerifyStatus: status})
	}
	if err != nil {
		data, _ := json.MarshalIndent(tools.Result{OK: false, Error: err.Error()}, "", "  ")
		return string(data)
	}
	data, _ := json.MarshalIndent(tools.Result{OK: true, Data: map[string]any{"title": args.Title, "path": args.Path, "line": args.Line, "conclusion": conclusion}, Message: "verification completed"}, "", "  ")
	return string(data)
}

func decodeVerifyFindingArgs(raw json.RawMessage) (verifyFindingArgs, error) {
	var args verifyFindingArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return args, err
	}
	if strings.TrimSpace(args.Title) == "" {
		return args, fmt.Errorf("title is required")
	}
	if strings.TrimSpace(args.Path) == "" {
		return args, fmt.Errorf("path is required")
	}
	if strings.TrimSpace(args.Evidence) == "" {
		return args, fmt.Errorf("evidence is required")
	}
	return args, nil
}

func (a *Agent) runVerification(ctx context.Context, emit func(Event), args verifyFindingArgs) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	defer a.startNaming(ctx, emit)()
	verifyPrompt := a.render("verify_finding", map[string]string{
		"title":          args.Title,
		"severity":       args.Severity,
		"path":           args.Path,
		"line":           fmt.Sprint(args.Line),
		"evidence":       args.Evidence,
		"impact":         args.Impact,
		"recommendation": args.Recommendation,
		"cwe":            args.CWE,
		"review_state":   a.boundedText("review_state", a.tools.ReviewPrompt(80)),
	})
	a.messages = append(a.messages, llm.Message{Role: llm.RoleUser, Content: a.boundedText("verification_input", verifyPrompt)})
	maxTurns := a.verificationTurnLimit()
	for turn := 1; turn <= maxTurns; turn++ {
		if emit != nil {
			emit(Event{Kind: "verify_progress", VerifyTitle: args.Title, VerifyTurn: turn, VerifyLimit: maxTurns, VerifyStatus: "等待子 agent 响应"})
		}
		answer, err := a.chatStream(ctx, func(Event) {})
		if err != nil {
			return "", err
		}
		a.messages = append(a.messages, llm.Message{Role: llm.RoleAssistant, Content: answer})
		call, ok := parseToolCall(answer)
		if !ok {
			if emit != nil {
				emit(Event{Kind: "verify_progress", VerifyTitle: args.Title, VerifyTurn: turn, VerifyLimit: maxTurns, VerifyStatus: summarizeVerificationText(answer)})
			}
			return answer, nil
		}
		if emit != nil {
			emit(Event{Kind: "verify_progress", VerifyTitle: args.Title, VerifyTurn: turn, VerifyLimit: maxTurns, VerifyStatus: describeVerificationToolCall(call)})
		}
		if call.Name == "verify_finding" || call.Name == "report_finding" || call.Name == "end_audit" || call.Name == "audit_plan_done" {
			a.messages = append(a.messages, llm.Message{Role: llm.RoleUser, Content: "验证子 agent 禁止调用该工具。请继续读取代码并在最后直接输出验证结论，不要再调用工具。"})
			if emit != nil {
				emit(Event{Kind: "verify_progress", VerifyTitle: args.Title, VerifyTurn: turn, VerifyLimit: maxTurns, VerifyStatus: "工具不允许，要求直接总结"})
			}
			continue
		}
		result, _ := a.callTool(ctx, emit, call)
		a.messages = append(a.messages, llm.Message{Role: llm.RoleUser, Content: "Tool result for " + call.Name + ":\n" + result})
	}
	if emit != nil {
		emit(Event{Kind: "verify_progress", VerifyTitle: args.Title, VerifyTurn: maxTurns, VerifyLimit: maxTurns, VerifyStatus: "达到上限，强制总结"})
	}
	a.messages = append(a.messages, llm.Message{Role: llm.RoleUser, Content: "已经达到验证轮数上限。现在禁止继续调用工具，请立刻基于已有证据输出最终中文验证结论，按约定格式总结是否成立、是否建议提交、原因、利用链复核、关键证据和仍需补充。"})
	if emit != nil {
		emit(Event{Kind: "verify_progress", VerifyTitle: args.Title, VerifyTurn: maxTurns, VerifyLimit: maxTurns, VerifyStatus: "达到上限，正在强制总结"})
	}
	answer, err := a.chatStream(ctx, func(Event) {})
	if err != nil {
		return "", err
	}
	a.messages = append(a.messages, llm.Message{Role: llm.RoleAssistant, Content: answer})
	return answer, nil
}

func summarizeVerificationText(text string) string {
	text = strings.TrimSpace(text)
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", " ")
	for strings.Contains(text, "  ") {
		text = strings.ReplaceAll(text, "  ", " ")
	}
	if text == "" {
		return "输出最终结论"
	}
	runes := []rune(text)
	if len(runes) > 32 {
		return string(runes[:32]) + "..."
	}
	return text
}

func describeVerificationToolCall(call ToolCall) string {
	var args map[string]any
	_ = json.Unmarshal(call.Arguments, &args)
	summary := call.Name
	switch call.Name {
	case "read_file":
		summary = "读取 " + truncateVerifyValue(stringArg(args, "path"), 24)
	case "search_content":
		summary = "搜索 " + truncateVerifyValue(stringArg(args, "query"), 24)
	case "review_state":
		summary = "查看当前排查状态"
	case "variable_review_update":
		summary = "记录变量 " + truncateVerifyValue(stringArg(args, "name"), 20)
	case "flow_review_update":
		summary = "记录链路 " + truncateVerifyValue(stringArg(args, "name"), 20)
	}
	return summary
}

func stringArg(args map[string]any, key string) string {
	if value, ok := args[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func truncateVerifyValue(text string, maxLen int) string {
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	return string(runes[:maxLen]) + "..."
}

func (a *Agent) verificationTurnLimit() int {
	return 8
}

func (a *Agent) endAuditNeedsConfirmation() string {
	var files []string
	for _, file := range a.tools.Files() {
		if file.Status == "unseen" || file.Status == "reviewing" {
			files = append(files, fmt.Sprintf("- [%s] %s", file.Status, file.Path))
		}
	}
	if len(files) == 0 {
		a.pendingEndAudit = false
		return ""
	}
	if a.pendingEndAudit {
		a.pendingEndAudit = false
		return ""
	}
	a.pendingEndAudit = true
	if len(files) > 80 {
		files = append(files[:80], fmt.Sprintf("- ...还有 %d 个文件未列出", len(files)-80))
	}
	return a.render("end_audit_confirmation", map[string]string{"files": strings.Join(files, "\n")})
}

func (a *Agent) emitState(emit func(Event)) {
	if a.checkpoint != nil {
		a.checkpoint(a)
	}
	snapshot := a.tools.Snapshot()
	emit(Event{Kind: "state", Phase: a.Phase(), Skills: a.prompts.LoadedSkillNames(), Todos: snapshot.Todos, Findings: snapshot.Findings, Project: snapshot.Project, Files: snapshot.Files, Variables: snapshot.Variables, Flows: snapshot.Flows, Audit: snapshot.Audit})
}

func (a *Agent) chatStream(ctx context.Context, emit func(Event)) (string, error) {
	if len(a.messages) == 0 {
		a.sanitizeMessages()
	}
	if !a.cfg.OpenAI.Stream {
		return a.chatStreamOnce(ctx, emit)
	}
	attempts := a.retryAttempts()
	var lastErr error
	for attempt := 0; attempt <= attempts; attempt++ {
		if attempt > 0 {
			emit(Event{Kind: "info", Content: fmt.Sprintf("开始第 %d/%d 次模型重试请求。", attempt+1, attempts+1)})
		}
		answer, err := a.chatStreamOnce(ctx, emit)
		if err == nil {
			return answer, nil
		}
		if errors.Is(err, context.Canceled) {
			return answer, err
		}
		if isContextLengthError(err) {
			if compressErr := a.compressContext(ctx, emit, "model context limit exceeded"); compressErr != nil {
				return "", compressErr
			}
			lastErr = err
			continue
		}
		lastErr = err
		if attempt < attempts {
			emit(Event{Kind: "error", Content: fmt.Sprintf("模型请求失败：%s。正在自动重试 %d/%d。", err.Error(), attempt+1, attempts)})
		}
	}
	return "", lastErr
}

func (a *Agent) chatStreamOnce(ctx context.Context, emit func(Event)) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if !a.cfg.OpenAI.Stream {
		answer, err := a.chatCurrentWithRetry(ctx, emit)
		if err != nil {
			return "", err
		}
		parser := newThinkParser(emit)
		parser.Write(answer)
		parser.Flush()
		return answer, nil
	}
	if err := a.prepareRequest(ctx, emit); err != nil {
		return "", err
	}
	var fullThinking strings.Builder
	var fullContent strings.Builder
	parser := newThinkParser(emit)
	var contentBuf strings.Builder
	var thinkBuf strings.Builder
	lastFlush := time.Now()
	flush := func() {
		if thinkBuf.Len() > 0 {
			emit(Event{Kind: "think_delta", Content: thinkBuf.String()})
			thinkBuf.Reset()
		}
		if contentBuf.Len() > 0 {
			parser.Write(contentBuf.String())
			contentBuf.Reset()
		}
		lastFlush = time.Now()
	}
	err := a.client.ChatStream(ctx, a.messages, func(delta llm.Delta) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if delta.Thinking != "" {
			fullThinking.WriteString(delta.Thinking)
			thinkBuf.WriteString(delta.Thinking)
		}
		if delta.Content != "" {
			fullContent.WriteString(delta.Content)
			contentBuf.WriteString(delta.Content)
		}
		if contentBuf.Len()+thinkBuf.Len() >= 512 || time.Since(lastFlush) >= 80*time.Millisecond {
			flush()
		}
		return nil
	})
	answer := joinAssistantMessage(fullThinking.String(), fullContent.String())
	if ctx.Err() != nil {
		return answer, ctx.Err()
	}
	flush()
	parser.Flush()
	if err == nil {
		a.forumPending = ""
	}
	return answer, err
}

func (a *Agent) chatCurrentWithRetry(ctx context.Context, emit func(Event)) (string, error) {
	attempts := a.retryAttempts()
	var lastErr error
	for attempt := 0; attempt <= attempts; attempt++ {
		if attempt > 0 && emit != nil {
			emit(Event{Kind: "info", Content: fmt.Sprintf("开始第 %d/%d 次模型重试请求。", attempt+1, attempts+1)})
		}
		if err := a.prepareRequest(ctx, emit); err != nil {
			return "", err
		}
		answer, err := a.client.Chat(ctx, a.messages)
		if err == nil {
			a.forumPending = ""
			return answer, nil
		}
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		if isContextLengthError(err) {
			if compressErr := a.compressContext(ctx, emit, "model context limit exceeded"); compressErr != nil {
				return "", compressErr
			}
			lastErr = err
			continue
		}
		lastErr = err
		if attempt < attempts && emit != nil {
			emit(Event{Kind: "error", Content: fmt.Sprintf("模型请求失败：%s。正在自动重试 %d/%d。", err.Error(), attempt+1, attempts)})
		}
	}
	return "", lastErr
}

func joinAssistantMessage(thinking, content string) string {
	if thinking == "" {
		return content
	}
	var b strings.Builder
	b.WriteString("<think>")
	b.WriteString(thinking)
	b.WriteString("</think>")
	b.WriteString(content)
	return b.String()
}

func (a *Agent) chatWithRetry(ctx context.Context, messages []llm.Message, emit func(Event)) (string, error) {
	return a.chatWithRetryClient(ctx, a.client, messages, emit)
}

func (a *Agent) chatWithRetryClient(ctx context.Context, client llm.Client, messages []llm.Message, emit func(Event)) (string, error) {
	attempts := a.retryAttempts()
	var lastErr error
	for attempt := 0; attempt <= attempts; attempt++ {
		if attempt > 0 && emit != nil {
			emit(Event{Kind: "info", Content: fmt.Sprintf("开始第 %d/%d 次模型重试请求。", attempt+1, attempts+1)})
		}
		answer, err := client.Chat(ctx, messages)
		if err == nil {
			return answer, nil
		}
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		lastErr = err
		if isContextLengthError(err) {
			return "", err
		}
		if attempt < attempts && emit != nil {
			emit(Event{Kind: "error", Content: fmt.Sprintf("模型请求失败：%s。正在自动重试 %d/%d。", err.Error(), attempt+1, attempts)})
		}
	}
	return "", lastErr
}

func isContextLengthError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "maximum context length") ||
		strings.Contains(text, "context length") ||
		strings.Contains(text, "input_tokens")
}

func (a *Agent) retryAttempts() int {
	if a.cfg.Agent.RetryAttempts < 0 {
		return 0
	}
	if a.cfg.Agent.RetryAttempts == 0 {
		return 3
	}
	return a.cfg.Agent.RetryAttempts
}

func (a *Agent) compressIfNeeded(ctx context.Context, emit func(Event)) error {
	limit, err := a.cfg.CompressionThreshold()
	if err != nil {
		return err
	}
	if estimateTokens(a.messages) < limit {
		return nil
	}
	return a.compressContext(ctx, emit, "estimated context budget reached")
}

func (a *Agent) compressContext(ctx context.Context, emit func(Event), reason string) error {
	emit(Event{Kind: "tool", Content: "compressing context: " + reason})
	messages := a.messages
	if len(messages) > 0 && messages[0].Role == llm.RoleSystem {
		messages = messages[1:]
	}
	return a.compressMessages(ctx, emit, reason, messages)
}

func (a *Agent) compressMessages(ctx context.Context, emit func(Event), reason string, messages []llm.Message) error {
	limit, err := a.cfg.CompressionThreshold()
	if err != nil {
		return err
	}
	budget, err := a.cfg.CompressionInputBudget()
	if err != nil {
		return err
	}
	state := a.boundedText("review_state", a.statePrompt(80))
	resumeTemplate := "resume_after_compress"
	if a.phase == phaseRecon {
		resumeTemplate = "plan_resume_after_compress"
	}
	// Build a candidate locally. Neither errors nor the summarizer may consume
	// pending notifications or replace authoritative conversation history.
	replacement := []llm.Message{
		{Role: llm.RoleSystem, Content: a.systemPrompt()},
		{Role: llm.RoleAssistant, Content: "Compressed audit context:\n"},
		{Role: llm.RoleUser, Content: a.render("state_after_compress", map[string]string{"state": state})},
		{Role: llm.RoleUser, Content: a.render(resumeTemplate, nil)},
	}
	if a.forumPending != "" {
		replacement = append(replacement, llm.Message{Role: llm.RoleUser, Content: a.forumPending})
	}
	summaryBudget := limit / 4
	if available := limit - estimateTokens(replacement) - 1; available < summaryBudget {
		summaryBudget = available
	}
	if summaryBudget <= 0 {
		return fmt.Errorf("系统提示与压缩后状态仍超过上下文预算，请调整模型上下文/输出配置")
	}
	prefix := fmt.Sprintf("Compression reason: %s\nKeep the summary within %d estimated tokens; prioritize remaining tasks and exact evidence.\n\n%s\nConversation to compress:\n", reason, summaryBudget, state)
	requestFor := func(history []llm.Message) []llm.Message {
		var b strings.Builder
		b.WriteString(prefix)
		for _, msg := range history {
			b.WriteString(string(msg.Role))
			b.WriteString(":\n")
			b.WriteString(msg.Content)
			b.WriteString("\n\n")
		}
		return []llm.Message{
			{Role: llm.RoleSystem, Content: a.prompts.Compress},
			{Role: llm.RoleUser, Content: a.render("compress_user", map[string]string{"state_and_conversation": b.String()})},
		}
	}
	request := requestFor(nil)
	baseCost := estimateTokens(request)
	if baseCost > budget {
		return fmt.Errorf("压缩模型完整基础请求超过输入预算，请降低输出上限或提高 compress_openai.max_context_tokens")
	}
	history := a.compressionHistory(messages, budget-baseCost)
	request = requestFor(history)
	// Rendered templates can repeat the history or add separators. Admit only
	// the final two-message request, including both roles and reserved output.
	for estimateTokens(request) > budget && len(history) > 0 {
		history = history[1:]
		request = requestFor(history)
	}
	if estimateTokens(request) > budget {
		return fmt.Errorf("压缩模型完整请求超过输入预算")
	}
	compressed, err := a.chatWithRetryClient(ctx, a.compressClient, request, emit)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(compressed) == "" {
		return fmt.Errorf("压缩模型返回空摘要，原始上下文已保留")
	}
	summary := a.boundedText("compressed_context", compressed)
	if estimateTextTokens(summary) > summaryBudget {
		return fmt.Errorf("压缩摘要超过恢复预算，原始上下文已保留")
	}
	replacement[1].Content += summary
	if estimateTokens(replacement) >= limit {
		return fmt.Errorf("系统提示与压缩后状态仍超过上下文预算，请调整模型上下文/输出配置")
	}
	a.messages = replacement
	emit(Event{Kind: "ui_compact", Content: "## 上下文已压缩\n\n" + compressed})
	a.emitState(emit)
	return nil
}

func sanitizeMessagesForCompression(messages []llm.Message) []llm.Message {
	cleaned := make([]llm.Message, 0, len(messages))
	for _, msg := range messages {
		content := msg.Content
		if msg.Role == llm.RoleAssistant {
			content = removeThinkBlocks(content)
		}
		if msg.Role == llm.RoleUser && strings.HasPrefix(content, "Tool result for ") {
			content = omitToolResultForCompression(content)
		}
		cleaned = append(cleaned, llm.Message{Role: msg.Role, Content: strings.TrimSpace(content)})
	}
	return cleaned
}

func removeThinkBlocks(content string) string {
	if !strings.Contains(content, "<think>") {
		return content
	}
	var visible strings.Builder
	for {
		start := strings.Index(content, "<think>")
		if start < 0 {
			visible.WriteString(content)
			return visible.String()
		}
		visible.WriteString(content[:start])
		content = content[start+len("<think>"):]
		for depth := 1; depth > 0; {
			end := strings.Index(content, "</think>")
			if end < 0 {
				// An unfinished reasoning block is never executable output.
				return visible.String()
			}
			nested := strings.Index(content, "<think>")
			if nested >= 0 && nested < end {
				depth++
				content = content[nested+len("<think>"):]
			} else {
				depth--
				content = content[end+len("</think>"):]
			}
		}
	}
}

func omitToolResultForCompression(content string) string {
	lineEnd := strings.IndexByte(content, '\n')
	if lineEnd < 0 {
		return content + "\n[tool result omitted during compression]"
	}
	return strings.TrimSpace(content[:lineEnd]) + "\n[tool result omitted during compression]"
}

func (a *Agent) statePrompt(limit int) string {
	var b strings.Builder
	b.WriteString("# 当前审计状态快照\n\n")
	b.WriteString("## 当前阶段\n")
	b.WriteString("- phase: ")
	b.WriteString(a.phase)
	b.WriteString("\n\n")
	b.WriteString("## 项目笔记\n")
	b.WriteString(formatAgentProjectNote(a.tools.ProjectNote()))
	b.WriteString("\n")
	audit := a.tools.Audit()
	b.WriteString("## 审计结束状态\n")
	b.WriteString(fmt.Sprintf("- ended: %v\n", audit.Ended))
	if audit.Summary != "" {
		b.WriteString("- summary: ")
		b.WriteString(audit.Summary)
		b.WriteString("\n")
	}
	if audit.NextSteps != "" {
		b.WriteString("- next_steps: ")
		b.WriteString(audit.NextSteps)
		b.WriteString("\n")
	}

	b.WriteString("\n## Todo 状态\n")
	todos := a.tools.Todos()
	if len(todos) == 0 {
		b.WriteString("暂无 todo。\n")
	} else {
		for _, todo := range todos {
			b.WriteString(fmt.Sprintf("- #%d [%s/%s] %s\n", todo.ID, todo.Status, todo.Priority, todo.Title))
		}
	}

	b.WriteString("\n## 已提交漏洞\n")
	findings := a.tools.Findings()
	if len(findings) == 0 {
		b.WriteString("暂无已提交漏洞。\n")
	} else {
		for _, finding := range findings {
			b.WriteString(fmt.Sprintf("- #%d [%s] %s at %s:%d\n", finding.ID, finding.Severity, finding.Title, finding.Path, finding.Line))
			if finding.Evidence != "" {
				b.WriteString("  evidence: ")
				b.WriteString(finding.Evidence)
				b.WriteString("\n")
			}
		}
	}

	b.WriteString("\n")
	b.WriteString(a.tools.ReviewPrompt(limit))
	return b.String()
}

func formatAgentProjectNote(note tools.ProjectNote) string {
	if strings.TrimSpace(note.Note) == "" {
		return "暂无项目笔记。规划阶段必须调用 project_note_update 维护详细自由文本笔记；压缩后也会保留并要求继续更新。\n"
	}
	return note.Note + "\n"
}

func estimateTokens(messages []llm.Message) int {
	tokens := 0
	for _, msg := range messages {
		tokens += estimateTextTokens(msg.Content) + 4
	}
	return tokens

}

func estimateTextTokens(text string) int {
	if text == "" {
		return 0
	}
	asciiRun := 0
	tokens := 0
	flushASCII := func() {
		if asciiRun == 0 {
			return
		}
		tokens += (asciiRun + 3) / 4
		asciiRun = 0
	}
	for _, r := range text {
		if r <= 0x7f {
			asciiRun++
			continue
		}
		flushASCII()
		if r >= 0x4e00 && r <= 0x9fff {
			tokens++
		} else {
			tokens += 2
		}
	}
	flushASCII()
	if tokens == 0 {
		return 1
	}
	return tokens
}

func parseToolCall(text string) (ToolCall, bool) {
	text = removeThinkBlocks(text)
	if payload, ok := extractTaggedPayload(text, "tool_call"); ok {
		if call, ok := decodeToolCallPayload(payload); ok {
			return call, true
		}
		return ToolCall{}, false
	}
	if call, ok := parseDSMLToolCall(text); ok {
		return call, true
	}
	return parseInvokeToolCall(text)
}

// parseDSMLToolCall accepts the complete Qwen DSML envelope seen in Responses
// content. Some model versions emit a valid JSON tool payload followed by
// DSML closing tags instead of </tool_call>; only complete JSON is accepted.
// Thinking has already been removed by parseToolCall, so examples in reasoning
// can never become executable calls.
func parseDSMLToolCall(text string) (ToolCall, bool) {
	const (
		callsOpen      = "<｜｜DSML｜｜ calls>"
		invokeOpen     = "<｜｜DSML｜｜ invoke"
		parameterClose = "</｜｜DSML｜｜ parameter>"
		invokeClose    = "</｜｜DSML｜｜ invoke>"
		callsClose     = "</｜｜DSML｜｜ calls>"
	)
	if idx := strings.Index(text, callsOpen); idx >= 0 {
		text = text[idx+len(callsOpen):]
	}
	invoke := strings.Index(text, invokeOpen)
	if invoke < 0 {
		return parseDSMLToolPayload(text, parameterClose)
	}
	text = text[invoke+len(invokeOpen):]
	end := strings.Index(text, ">")
	if end < 0 {
		return ToolCall{}, false
	}
	attrs := strings.TrimSpace(text[:end])
	name := ""
	if strings.HasPrefix(attrs, "name=") {
		quoted := strings.TrimPrefix(attrs, "name=")
		if len(quoted) < 2 || (quoted[0] != '"' && quoted[0] != '\'') || quoted[len(quoted)-1] != quoted[0] {
			return ToolCall{}, false
		}
		name = quoted[1 : len(quoted)-1]
	}
	if name == "" {
		return ToolCall{}, false
	}
	body := text[end+1:]
	closeAt := strings.Index(body, parameterClose)
	if closeAt < 0 {
		closeAt = strings.Index(body, invokeClose)
	}
	if closeAt < 0 {
		closeAt = strings.Index(body, callsClose)
	}
	if closeAt < 0 {
		return ToolCall{}, false
	}
	payload := strings.TrimSpace(body[:closeAt])
	if payload == "" {
		payload = "{}"
	}
	var args json.RawMessage
	if err := json.Unmarshal([]byte(payload), &args); err != nil || len(args) == 0 {
		return ToolCall{}, false
	}
	if string(args) == "null" {
		return ToolCall{}, false
	}
	return ToolCall{Name: name, Arguments: args}, true
}

// A standard <tool_call> opener can be closed by a DSML parameter tag. The
// JSON decoder still requires a complete object, preventing execution of a
// streamed/incomplete payload.
func parseDSMLToolPayload(text, closeTag string) (ToolCall, bool) {
	open := "<tool_call>"
	idx := strings.Index(text, open)
	if idx < 0 {
		return ToolCall{}, false
	}
	body := text[idx+len(open):]
	end := strings.Index(body, closeTag)
	if end < 0 {
		return ToolCall{}, false
	}
	return decodeToolCallPayload(body[:end])
}

func decodeToolCallPayload(text string) (ToolCall, bool) {
	trimmed := strings.TrimSpace(text)
	var call ToolCall
	if err := json.Unmarshal([]byte(trimmed), &call); err == nil && call.Name != "" {
		return call, true
	}
	if nested, ok := extractTaggedPayload(trimmed, "tool_call"); ok {
		return decodeToolCallPayload(nested)
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != '{' {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(trimmed[i:]))
		if err := decoder.Decode(&call); err == nil && call.Name != "" {
			return call, true
		}
	}
	return ToolCall{}, false
}

var invokeRe = regexp.MustCompile(`(?s)<invoke\s+name=["']([^"']+)["']\s*>(.*?)</invoke>`)
var parameterRe = regexp.MustCompile(`(?s)<parameter\s+name=["']([^"']+)["'][^>]*>(.*?)</parameter>`)

func parseInvokeToolCall(text string) (ToolCall, bool) {
	match := invokeRe.FindStringSubmatch(text)
	if len(match) != 3 {
		return ToolCall{}, false
	}
	args := map[string]string{}
	for _, param := range parameterRe.FindAllStringSubmatch(match[2], -1) {
		if len(param) == 3 {
			args[param[1]] = strings.TrimSpace(param[2])
		}
	}
	data, err := json.Marshal(args)
	if err != nil {
		return ToolCall{}, false
	}
	return ToolCall{Name: match[1], Arguments: data}, match[1] != ""
}

func extractTaggedPayload(text, tag string) (string, bool) {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	start := strings.Index(text, open)
	if start < 0 {
		return "", false
	}
	start += len(open)
	end := strings.Index(text[start:], close)
	if end < 0 {
		return "", false
	}
	return strings.TrimSpace(text[start : start+end]), true
}

type thinkParser struct {
	emit   func(Event)
	mode   string
	buffer string
}

func newThinkParser(emit func(Event)) *thinkParser {
	return &thinkParser{emit: emit}
}

func (p *thinkParser) Write(text string) {
	p.buffer += text
	for {
		if p.mode != "" {
			close := "</" + p.mode + ">"
			idx := strings.Index(p.buffer, close)
			if idx < 0 {
				if p.mode != "tool_call" {
					p.emitBuffered(p.mode + "_delta")
				}
				return
			}
			if idx > 0 {
				p.emit(Event{Kind: p.mode + "_delta", Content: p.buffer[:idx]})
			}
			p.buffer = p.buffer[idx+len(close):]
			p.mode = ""
			continue
		}

		idx, mode := p.nextSpecialTag()
		if idx < 0 {
			p.emitSafeAssistant()
			return
		}
		if idx > 0 {
			p.emit(Event{Kind: "assistant_delta", Content: p.buffer[:idx]})
		}
		p.buffer = p.buffer[idx+len("<"+mode+">"):]
		p.mode = mode
	}
}

func (p *thinkParser) Flush() {
	if p.buffer == "" {
		return
	}
	if p.mode != "" {
		if p.mode == "tool_call" {
			p.emit(Event{Kind: "assistant_delta", Content: "<tool_call>" + p.buffer})
		} else {
			p.emit(Event{Kind: p.mode + "_delta", Content: p.buffer})
		}
	} else {
		p.emit(Event{Kind: "assistant_delta", Content: p.buffer})
	}
	p.buffer = ""
}

func (p *thinkParser) emitBuffered(kind string) {
	if p.buffer == "" {
		return
	}
	p.emit(Event{Kind: kind, Content: p.buffer})
	p.buffer = ""
}

func (p *thinkParser) emitSafeAssistant() {
	idx := strings.LastIndex(p.buffer, "<")
	if idx >= 0 && (strings.HasPrefix("<think>", p.buffer[idx:]) || strings.HasPrefix("<tool_call>", p.buffer[idx:])) {
		if idx > 0 {
			p.emit(Event{Kind: "assistant_delta", Content: p.buffer[:idx]})
			p.buffer = p.buffer[idx:]
		}
		return
	}
	p.emitBuffered("assistant_delta")
}

func (p *thinkParser) nextSpecialTag() (int, string) {
	thinkIdx := strings.Index(p.buffer, "<think>")
	toolIdx := strings.Index(p.buffer, "<tool_call>")
	if thinkIdx < 0 {
		return toolIdx, "tool_call"
	}
	if toolIdx < 0 || thinkIdx < toolIdx {
		return thinkIdx, "think"
	}
	return toolIdx, "tool_call"
}
