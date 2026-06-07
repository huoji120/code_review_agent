package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"code-review-agent/internal/config"
	"code-review-agent/internal/llm"
	"code-review-agent/internal/prompt"
	"code-review-agent/internal/tools"
)

type Agent struct {
	cfg               config.Config
	prompts           prompt.Prompts
	client            llm.Client
	compressClient    llm.Client
	tools             *tools.Registry
	messages          []llm.Message
	pendingEndAudit   bool
	trace             *traceLog
	tracePath         string
	traceErr          error
	traceBootstrapped bool
}

type Event struct {
	Kind         string
	Content      string
	Skills       []string
	VerifyTitle  string
	VerifyTurn   int
	VerifyLimit  int
	VerifyStatus string
	Todos        []tools.Todo
	Findings     []tools.Finding
	Files        []tools.FileReview
	Variables    []tools.VariableReview
	Flows        []tools.FlowReview
	Audit        tools.AuditState
}

type ToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func New(cfg config.Config, prompts prompt.Prompts, client llm.Client, registry *tools.Registry) *Agent {
	return NewWithCompressClient(cfg, prompts, client, client, registry)
}

func NewWithCompressClient(cfg config.Config, prompts prompt.Prompts, client llm.Client, compressClient llm.Client, registry *tools.Registry) *Agent {
	if compressClient == nil {
		compressClient = client
	}
	a := &Agent{cfg: cfg, prompts: prompts, client: client, compressClient: compressClient, tools: registry}
	a.messages = []llm.Message{{Role: llm.RoleSystem, Content: a.systemPrompt()}}
	return a
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
	system := a.prompts.SystemWithSkills()
	system += "\n\n" + a.tools.GitPrompt()
	system += "\n\n" + a.tools.ToolPrompt()
	system += "\n\n" + a.skillToolPrompt()
	if a.cfg.Agent.AutoPlan {
		system += "\n\n" + a.render("auto_plan", nil)
	}
	system += "\n\n" + a.render("summary_interval", map[string]string{"summary_interval": fmt.Sprint(a.cfg.Agent.SummaryInterval)})
	system += "\n\n" + a.render("tool_protocol_guard", nil)
	return system
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

func (a *Agent) SetWorkspace(workspace string) error {
	if err := a.tools.SetWorkspace(workspace); err != nil {
		return err
	}
	a.sanitizeMessages()
	return nil
}

func (a *Agent) Snapshot() tools.Snapshot {
	return a.tools.Snapshot()
}

func (a *Agent) LoadedSkills() []string {
	return a.prompts.LoadedSkillNames()
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
	a.sanitizeMessages()
	a.bootstrapTrace()
	userMessage := llm.Message{Role: llm.RoleUser, Content: a.render("initial_audit_instruction", map[string]string{"input": input, "review_state": a.tools.ReviewPrompt(160)})}
	a.messages = append(a.messages, userMessage)
	a.appendTraceMessage(userMessage)
	a.emitState(emit)
	for turn := 1; ; turn++ {
		if ctx.Err() != nil {
			return
		}
		if a.cfg.Agent.MaxTurns > 0 && turn > a.cfg.Agent.MaxTurns {
			emit(Event{Kind: "error", Content: "max agent turns reached"})
			return
		}
		if a.cfg.Agent.SummaryInterval > 0 && turn > 1 && (turn-1)%a.cfg.Agent.SummaryInterval == 0 {
			if err := a.compressContext(ctx, emit, fmt.Sprintf("%d agent turns completed", a.cfg.Agent.SummaryInterval)); err != nil {
				emit(Event{Kind: "error", Content: err.Error()})
				return
			}
		}
		if err := a.compressIfNeeded(ctx, emit); err != nil {
			emit(Event{Kind: "error", Content: err.Error()})
			return
		}
		answer, err := a.chatStream(ctx, emit)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			emit(Event{Kind: "error", Content: err.Error()})
			return
		}
		assistantMessage := llm.Message{Role: llm.RoleAssistant, Content: answer}
		a.messages = append(a.messages, assistantMessage)
		a.appendTraceMessage(assistantMessage)
		emit(Event{Kind: "assistant_done"})

		call, ok := parseToolCall(answer)
		if !ok {
			retryMessage := llm.Message{Role: llm.RoleUser, Content: a.render("no_tool_retry", nil)}
			a.messages = append(a.messages, retryMessage)
			a.appendTraceMessage(retryMessage)
			continue
		}
		if call.Name == "end_audit" {
			if blocker := a.endAuditNeedsConfirmation(); blocker != "" {
				emit(Event{Kind: "tool", Content: "end_audit 需要二次确认：仍有未完全审计文件"})
				blockerMessage := llm.Message{Role: llm.RoleUser, Content: blocker}
				a.messages = append(a.messages, blockerMessage)
				a.appendTraceMessage(blockerMessage)
				continue
			}
			emit(Event{Kind: "tool", Content: "AI 调用了审计完成工具 end_audit"})
		} else {
			a.pendingEndAudit = false
			emit(Event{Kind: "tool", Content: fmt.Sprintf("calling %s", call.Name)})
		}
		result, fullResult := a.callTool(ctx, emit, call)
		toolMessage := llm.Message{Role: llm.RoleUser, Content: "Tool result for " + call.Name + ":\n" + result}
		a.messages = append(a.messages, toolMessage)
		a.appendTraceMessage(llm.Message{Role: llm.RoleUser, Content: "Tool result for " + call.Name + ":\n" + fullResult})
		if !tools.IsKnownTool(call.Name) {
			correctionMessage := llm.Message{Role: llm.RoleUser, Content: a.unknownToolCorrection(call.Name)}
			a.messages = append(a.messages, correctionMessage)
			a.appendTraceMessage(correctionMessage)
		}
		emit(Event{Kind: "tool", Content: a.displayToolResult(call.Name, result)})
		a.emitState(emit)
		if tools.IsTerminalTool(call.Name) {
			emit(Event{Kind: "assistant", Content: "## 审计已完成\n\n" + formatEndAuditResult(result)})
			return
		}
	}
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
	if call.Name == "load_skill" {
		result := a.loadSkill(call.Arguments)
		return result, result
	}
	if call.Name == "verify_finding" {
		result := a.verifyFinding(ctx, emit, call.Arguments)
		return result, result
	}
	return a.tools.CallWithFullResult(call.Name, call.Arguments)
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
	registry, err := tools.NewRegistry(a.tools.Workspace(), a.cfg.Agent.MaxToolResultChars)
	if err != nil {
		data, _ := json.MarshalIndent(tools.Result{OK: false, Error: err.Error()}, "", "  ")
		return string(data)
	}
	registry.RestoreSnapshot(a.tools.Snapshot())
	childPrompts := a.prompts
	childPrompts.SetLoadedSkills(a.prompts.LoadedSkillNames())
	child := &Agent{cfg: a.cfg, prompts: childPrompts, client: a.client, compressClient: a.compressClient, tools: registry}
	child.messages = []llm.Message{{Role: llm.RoleSystem, Content: child.systemPrompt()}}
	if emit != nil {
		emit(Event{Kind: "verify_progress", VerifyTitle: args.Title, VerifyTurn: 0, VerifyLimit: child.verificationTurnLimit(), VerifyStatus: "准备验证"})
	}
	conclusion, err := child.runVerification(ctx, emit, args)
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
	verifyPrompt := a.render("verify_finding", map[string]string{
		"title":          args.Title,
		"severity":       args.Severity,
		"path":           args.Path,
		"line":           fmt.Sprint(args.Line),
		"evidence":       args.Evidence,
		"impact":         args.Impact,
		"recommendation": args.Recommendation,
		"cwe":            args.CWE,
		"review_state":   a.tools.ReviewPrompt(200),
	})
	a.messages = append(a.messages, llm.Message{Role: llm.RoleUser, Content: verifyPrompt})
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
		if call.Name == "verify_finding" || call.Name == "report_finding" || call.Name == "end_audit" || call.Name == "load_skill" {
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
	if a.cfg.Agent.SummaryInterval > 0 {
		return a.cfg.Agent.SummaryInterval
	}
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

func (a *Agent) displayToolResult(name, result string) string {
	switch name {
	case "read_file":
		return "read_file 结果已隐藏，完整文件内容已发送给 AI。"
	case "end_audit":
		return formatEndAuditResult(result)
	default:
		return result
	}
}

func formatEndAuditResult(result string) string {
	var parsed struct {
		OK   bool `json:"ok"`
		Data struct {
			Summary   string          `json:"summary"`
			NextSteps string          `json:"next_steps"`
			Findings  json.RawMessage `json:"findings"`
		} `json:"data"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil || !parsed.OK {
		return result
	}
	findingCount := 0
	if len(parsed.Data.Findings) > 0 {
		var findings []json.RawMessage
		if err := json.Unmarshal(parsed.Data.Findings, &findings); err == nil {
			findingCount = len(findings)
		}
	}
	var b strings.Builder
	b.WriteString("审计完成工具 end_audit 已执行。\n")
	if parsed.Data.Summary != "" {
		b.WriteString("\nSummary:\n")
		b.WriteString(parsed.Data.Summary)
		b.WriteString("\n")
	}
	if parsed.Data.NextSteps != "" {
		b.WriteString("\nNext steps:\n")
		b.WriteString(parsed.Data.NextSteps)
		b.WriteString("\n")
	}
	b.WriteString(fmt.Sprintf("\nSubmitted findings: %d", findingCount))
	return b.String()
}

func (a *Agent) emitState(emit func(Event)) {
	snapshot := a.tools.Snapshot()
	emit(Event{Kind: "state", Skills: a.prompts.LoadedSkillNames(), Todos: snapshot.Todos, Findings: snapshot.Findings, Files: snapshot.Files, Variables: snapshot.Variables, Flows: snapshot.Flows, Audit: snapshot.Audit})
}

func (a *Agent) formatFinalFindings() string {
	findings := a.tools.Findings()
	if len(findings) == 0 {
		return "## Audit Finished\n\nNo submitted findings."
	}
	var b strings.Builder
	b.WriteString("## Audit Finished\n\n### Submitted Findings\n")
	for _, finding := range findings {
		b.WriteString(fmt.Sprintf("\n%d. **[%s] %s**\n", finding.ID, finding.Severity, finding.Title))
		b.WriteString(fmt.Sprintf("Path: `%s`", finding.Path))
		if finding.Line > 0 {
			b.WriteString(fmt.Sprintf(":%d", finding.Line))
		}
		b.WriteString("\n")
		if finding.CWE != "" {
			b.WriteString(fmt.Sprintf("CWE: `%s`\n", finding.CWE))
		}
		b.WriteString(fmt.Sprintf("Evidence: %s\n", finding.Evidence))
		if finding.Impact != "" {
			b.WriteString(fmt.Sprintf("Impact: %s\n", finding.Impact))
		}
		if finding.Recommendation != "" {
			b.WriteString(fmt.Sprintf("Recommendation: %s\n", finding.Recommendation))
		}
	}
	return b.String()
}

func (a *Agent) chatStream(ctx context.Context, emit func(Event)) (string, error) {
	a.sanitizeMessages()
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
	return answer, err
}

func (a *Agent) chatCurrentWithRetry(ctx context.Context, emit func(Event)) (string, error) {
	attempts := a.retryAttempts()
	var lastErr error
	for attempt := 0; attempt <= attempts; attempt++ {
		if attempt > 0 && emit != nil {
			emit(Event{Kind: "info", Content: fmt.Sprintf("开始第 %d/%d 次模型重试请求。", attempt+1, attempts+1)})
		}
		answer, err := a.client.Chat(ctx, a.messages)
		if err == nil {
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
	limit := a.compressionLimit()
	if limit <= 0 || estimateTokens(a.messages) < limit {
		return nil
	}
	return a.compressContext(ctx, emit, "context budget reached")
}

func (a *Agent) compressionLimit() int {
	reserved := a.cfg.OpenAI.MaxOutputTokens + a.cfg.Agent.CompressBufferTokens
	limit := a.cfg.OpenAI.MaxContextTokens - reserved
	ratioLimit := int(float64(a.cfg.OpenAI.MaxContextTokens) * a.cfg.Agent.CompressAtRatio)
	if ratioLimit > 0 && ratioLimit < limit {
		limit = ratioLimit
	}
	if limit < 1024 {
		limit = 1024
	}
	return limit
}

func (a *Agent) compressContext(ctx context.Context, emit func(Event), reason string) error {
	a.sanitizeMessages()
	emit(Event{Kind: "tool", Content: "compressing context: " + reason})
	return a.compressMessages(ctx, emit, reason, a.messages[1:])
}

func (a *Agent) compressMessages(ctx context.Context, emit func(Event), reason string, messages []llm.Message) error {
	var b strings.Builder
	b.WriteString("Compression reason: ")
	b.WriteString(reason)
	b.WriteString(fmt.Sprintf("\nSummary interval: %d turns\n", a.cfg.Agent.SummaryInterval))
	b.WriteString(fmt.Sprintf("The compressed summary must include a concrete plan for the next %d agent turns.\n", a.cfg.Agent.SummaryInterval))
	b.WriteString("\n")
	b.WriteString(a.statePrompt(200))
	b.WriteString("\nConversation to compress:\n")
	for _, msg := range sanitizeMessagesForCompression(messages) {
		b.WriteString(string(msg.Role))
		b.WriteString(":\n")
		b.WriteString(msg.Content)
		b.WriteString("\n\n")
	}
	compressed, err := a.chatWithRetryClient(ctx, a.compressClient, []llm.Message{
		{Role: llm.RoleSystem, Content: a.prompts.Compress + "\n\n下面是恢复审计后必须继续遵守的完整系统提示和工具协议：\n\n" + a.systemPrompt()},
		{Role: llm.RoleUser, Content: a.render("compress_user", map[string]string{"state_and_conversation": b.String(), "summary_interval": fmt.Sprint(a.cfg.Agent.SummaryInterval)})},
	}, emit)
	if err != nil {
		return err
	}
	a.messages = []llm.Message{
		{Role: llm.RoleSystem, Content: a.systemPrompt()},
		{Role: llm.RoleAssistant, Content: "Compressed audit context:\n" + compressed},
		{Role: llm.RoleUser, Content: a.render("state_after_compress", map[string]string{"state": a.statePrompt(240)})},
		{Role: llm.RoleUser, Content: a.render("resume_after_compress", map[string]string{"summary_interval": fmt.Sprint(a.cfg.Agent.SummaryInterval)})},
	}
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
	for {
		start := strings.Index(content, "<think>")
		if start < 0 {
			return content
		}
		rest := content[start+len("<think>"):]
		end := strings.Index(rest, "</think>")
		if end < 0 {
			// Drop an unfinished thinking block before compression.
			return content[:start]
		}
		end += start + len("<think>") + len("</think>")
		content = content[:start] + content[end:]
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
	payload, ok := extractTaggedPayload(text, "tool_call")
	if ok {
		if call, ok := decodeToolCallPayload(payload); ok {
			return call, true
		}
		return ToolCall{}, false
	}
	return parseInvokeToolCall(text)
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
