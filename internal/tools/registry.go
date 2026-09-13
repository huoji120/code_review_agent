package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type Registry struct {
	workspace          string
	maxToolResultChars int
	inventory          []InventoryEntry
	interesting        interestingPathsConfig
	todos              []Todo
	findings           []Finding
	projectNote        ProjectNote
	files              []FileReview
	variables          []VariableReview
	flows              []FlowReview
	audit              AuditState
	nextTodoID         int
	bufferMu           sync.Mutex
	buffer             resultBuffer
}

type resultBuffer struct {
	id      string
	content string
}

type InventoryEntry struct {
	Path        string `json:"path"`
	Size        int64  `json:"size,omitempty"`
	Ext         string `json:"ext,omitempty"`
	Dir         string `json:"dir,omitempty"`
	LowValue    bool   `json:"low_value,omitempty"`
	Interesting bool   `json:"interesting,omitempty"`
}

type interestingPathsConfig struct {
	Keywords     []string `json:"keywords"`
	LowValueDirs []string `json:"low_value_dirs"`
}

type Result struct {
	OK      bool        `json:"ok"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
	Trunc   bool        `json:"truncated,omitempty"`
	Message string      `json:"message,omitempty"`
}

type Todo struct {
	ID       int    `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

type Snapshot struct {
	Todos     []Todo           `json:"todos"`
	Findings  []Finding        `json:"findings"`
	Project   ProjectNote      `json:"project_note"`
	Files     []FileReview     `json:"files"`
	Variables []VariableReview `json:"variables"`
	Flows     []FlowReview     `json:"flows"`
	Audit     AuditState       `json:"audit"`
}

type ProjectNote struct {
	Note string `json:"note,omitempty"`
}

func (r *Registry) RestoreSnapshot(snapshot Snapshot) {
	r.todos = append([]Todo(nil), snapshot.Todos...)
	r.findings = append([]Finding(nil), snapshot.Findings...)
	r.projectNote = snapshot.Project
	r.files = append([]FileReview(nil), snapshot.Files...)
	r.variables = append([]VariableReview(nil), snapshot.Variables...)
	r.flows = append([]FlowReview(nil), snapshot.Flows...)
	r.audit = snapshot.Audit
	r.nextTodoID = 1
	for _, todo := range r.todos {
		if todo.ID >= r.nextTodoID {
			r.nextTodoID = todo.ID + 1
		}
	}
}

type AuditState struct {
	Ended     bool   `json:"ended"`
	Summary   string `json:"summary,omitempty"`
	NextSteps string `json:"next_steps,omitempty"`
}

type FileReview struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

type VariableReview struct {
	Name   string `json:"name"`
	Path   string `json:"path,omitempty"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

type FlowReview struct {
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	Entry     string   `json:"entry,omitempty"`
	Files     []string `json:"files,omitempty"`
	Variables []string `json:"variables,omitempty"`
	Evidence  string   `json:"evidence,omitempty"`
	NextStep  string   `json:"next_step,omitempty"`
	Note      string   `json:"note,omitempty"`
}

func NewRegistry(workspace string, maxToolResultChars int) (*Registry, error) {
	abs, err := cleanWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	if maxToolResultChars <= 0 {
		maxToolResultChars = 12000
	}
	if maxToolResultChars < 1024 {
		maxToolResultChars = 1024
	}
	if maxToolResultChars > 65536 {
		maxToolResultChars = 65536
	}
	r := &Registry{workspace: abs, maxToolResultChars: maxToolResultChars, nextTodoID: 1, interesting: loadInterestingPathsConfig()}
	r.refreshFileInventory()
	return r, nil
}
func (r *Registry) SetWorkspace(workspace string) error {
	abs, err := cleanWorkspace(workspace)
	if err != nil {
		return err
	}
	if err := r.Close(); err != nil {
		return err
	}
	r.workspace = abs
	r.inventory = nil
	r.todos = nil
	r.findings = nil
	r.projectNote = ProjectNote{}
	r.files = nil
	r.variables = nil
	r.flows = nil
	r.audit = AuditState{}
	r.nextTodoID = 1
	r.refreshFileInventory()
	return nil
}

func (r *Registry) Workspace() string { return r.workspace }
func cleanWorkspace(workspace string) (string, error) {
	if workspace == "" {
		workspace = "."
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace is not a directory: %s", abs)
	}
	return abs, nil
}

func (r *Registry) Call(name string, raw json.RawMessage) string {
	out, _ := r.CallWithFullResult(context.Background(), name, raw)
	return out
}
func (r *Registry) CallWithFullResult(ctx context.Context, name string, raw json.RawMessage) (string, string) {
	if name != "read_tool_buffer" {
		r.ClearBuffer()
	}
	var res Result
	switch name {
	case "list_files":
		res = r.listFiles(raw)
	case "read_file":
		res = r.readFile(raw)
	case "search_content", "search_context":
		res = r.searchContent(raw)
	case "git_inspect":
		res = r.gitInspect(raw)
	case "todo_create":
		res = r.todoCreate(raw)
	case "todo_update":
		res = r.todoUpdate(raw)
	case "file_review_update":
		res = r.fileReviewUpdate(raw)
	case "variable_review_update":
		res = r.variableReviewUpdate(raw)
	case "flow_review_update":
		res = r.flowReviewUpdate(raw)
	case "flow_review_delete":
		res = r.flowReviewDelete(raw)
	case "review_state":
		res = r.reviewState(raw)
	case "project_note_update":
		res = r.projectNoteUpdate(raw)
	case "report_finding":
		res = r.reportFinding(raw)
	case "end_audit":
		res = r.endAudit(raw)
	case "read_tool_buffer":
		page := r.readToolBuffer(raw)
		return page, page
	default:
		res = Result{OK: false, Error: "unknown tool: " + name}
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		out := fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error())
		return out, out
	}
	full := string(data)
	return r.BoundResult(name, full), full
}

func IsKnownTool(name string) bool {
	switch name {
	case "list_files", "read_file", "search_content", "search_context", "git_inspect", "todo_create", "todo_update", "file_review_update", "variable_review_update", "flow_review_update", "flow_review_delete", "review_state", "project_note_update", "report_finding", "end_audit", "read_tool_buffer", "load_skill", "verify_finding", "audit_plan_done":
		return true
	default:
		return false
	}
}

func (r *Registry) ApplyAuditScope(paths []string) []string {
	var applied []string
	for _, p := range paths {
		p = normalizeInventoryPath(p)
		if p == "" || !r.inventoryHasPath(p) {
			continue
		}
		applied = append(applied, p)
		found := false
		for i := range r.files {
			if r.files[i].Path != p {
				continue
			}
			found = true
			if r.files[i].Status == "skipped" {
				r.files[i].Status = "reviewing"
			}
			if r.files[i].Note == "" {
				r.files[i].Note = "规划阶段纳入 one-shot 审计范围"
			}
			break
		}
		if !found {
			r.files = append(r.files, FileReview{Path: p, Status: "reviewing", Note: "规划阶段纳入 one-shot 审计范围"})
		}
	}
	return applied
}

func (r *Registry) inventoryHasPath(path string) bool {
	path = normalizeInventoryPath(path)
	for _, item := range r.inventory {
		if item.Path == path {
			return true
		}
	}
	return false
}

func IsTerminalTool(name string) bool {
	return name == "end_audit"
}

func (r *Registry) Snapshot() Snapshot {
	return Snapshot{Todos: r.Todos(), Findings: r.Findings(), Project: r.ProjectNote(), Files: r.Files(), Variables: r.Variables(), Flows: r.Flows(), Audit: r.Audit()}
}

func (r *Registry) ProjectNote() ProjectNote {
	return r.projectNote
}

func (r *Registry) Audit() AuditState {
	return r.audit
}

func (r *Registry) ToolPrompt() string {
	return `# 审计工具使用原则

只通过本轮提供的原生工具调用执行操作，每次仅调用一个工具。工具名称与参数以原生定义为准；普通正文、历史文本与示例不构成调用。

优先使用工具返回的工作区相对路径。文件排查范围默认为空，未纳入范围的 inventory 文件不代表已跳过；不要在未读取内容时批量标记 reviewed。待办应使用具体中文标题，记录文件、目录、模块、入口函数或变量。

持续维护详细项目笔记，记录架构、运行行为、登录认证、鉴权、攻击面、数据/状态流、文件角色、证据和待确认问题。发现疑似漏洞时先记录关键变量及跨文件调用链、sink 与源码证据，再请求独立验证；参考验证结论后才提交有实际危害的高置信度严重漏洞。报告后在论坛公开证据邀请反驳，并继续排查同源入口与相邻模块。临时 flow 闭环或已转为漏洞后应移除，不要长期累积。

结束前检查 review_state 和实际覆盖；关闭请求 pending 不等于完成，拒绝或超时后应继续复核、检查尚未覆盖的代码或收集证据，不要无限等待或重复刷投票。`
}

func (r *Registry) Files() []FileReview {
	out := make([]FileReview, len(r.files))
	copy(out, r.files)
	return out
}

func (r *Registry) Variables() []VariableReview {
	out := make([]VariableReview, len(r.variables))
	copy(out, r.variables)
	return out
}

func (r *Registry) Flows() []FlowReview {
	out := make([]FlowReview, len(r.flows))
	copy(out, r.flows)
	return out
}

func (r *Registry) ReviewPrompt(limit int) string {
	if limit <= 0 {
		limit = 120
	}
	var b strings.Builder
	b.WriteString("# 当前排查状态\n\n")
	b.WriteString("## 项目笔记\n")
	b.WriteString(formatProjectNote(r.projectNote))
	b.WriteString("\n")
	b.WriteString("## Inventory 摘要\n")
	b.WriteString(r.inventorySummary(limit))
	b.WriteString("\n## 已纳入审计范围\n")
	counts := map[string]int{}
	for _, file := range r.files {
		counts[file.Status]++
	}
	b.WriteString(fmt.Sprintf("已选择文件数：%d，未看：%d，正在审计：%d，已看：%d，跳过：%d。file_review 默认只显示模型显式纳入 one-shot 的文件；未列出的 inventory 文件不代表 skipped。\n\n", len(r.files), counts["unseen"], counts["reviewing"], counts["reviewed"], counts["skipped"]))
	if len(r.files) == 0 {
		b.WriteString("暂无已纳入审计范围的文件。规划阶段应使用 file_review_update 通过 path/paths/dir+suffix/pattern 选择文件。\n")
	}
	for i, file := range r.files {
		if i >= limit {
			b.WriteString("- ...文件列表已截断，可用 review_state 查看更多。\n")
			break
		}
		b.WriteString(fmt.Sprintf("- [%s] %s", file.Status, file.Path))
		if file.Note != "" {
			b.WriteString("：")
			b.WriteString(file.Note)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n## 变量/符号排查状态\n")
	if len(r.variables) == 0 {
		b.WriteString("暂无变量或符号排查记录。\n")
	} else {
		for i, variable := range r.variables {
			if i >= limit {
				b.WriteString("- ...变量列表已截断，可用 review_state 查看更多。\n")
				break
			}
			b.WriteString(fmt.Sprintf("- [%s] %s", variable.Status, variable.Name))
			if variable.Path != "" {
				b.WriteString(" @ ")
				b.WriteString(variable.Path)
			}
			if variable.Note != "" {
				b.WriteString("：")
				b.WriteString(variable.Note)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\n## 跨文件调用链/数据流排查状态\n")
	if len(r.flows) == 0 {
		b.WriteString("暂无跨文件 flow 记录。\n")
	} else {
		for i, flow := range r.flows {
			if i >= limit {
				b.WriteString("- ...flow 列表已截断，可用 review_state 查看更多。\n")
				break
			}
			b.WriteString(fmt.Sprintf("- [%s] %s", flow.Status, flow.Name))
			if flow.Entry != "" {
				b.WriteString("，入口：")
				b.WriteString(flow.Entry)
			}
			if len(flow.Files) > 0 {
				b.WriteString("，文件链：")
				b.WriteString(strings.Join(flow.Files, " -> "))
			}
			if flow.NextStep != "" {
				b.WriteString("，下一步：")
				b.WriteString(flow.NextStep)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func (r *Registry) refreshFileInventory() {
	var inventory []InventoryEntry
	_ = filepath.WalkDir(r.workspace, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != r.workspace && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(r.workspace, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, _ := d.Info()
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		entry := InventoryEntry{Path: rel, Size: size, Ext: strings.ToLower(filepath.Ext(rel)), Dir: filepath.ToSlash(filepath.Dir(rel))}
		if entry.Dir == "." {
			entry.Dir = ""
		}
		entry.LowValue = r.isLowValuePath(rel)
		entry.Interesting = r.isInterestingPath(rel)
		inventory = append(inventory, entry)
		return nil
	})
	sort.Slice(inventory, func(i, j int) bool { return inventory[i].Path < inventory[j].Path })
	r.inventory = inventory
}

func (r *Registry) inventorySummary(limit int) string {
	if limit <= 0 {
		limit = 120
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("总文件数：%d。完整清单只保存在本地 inventory，不会全量塞进上下文。\n", len(r.inventory)))
	b.WriteString("扩展名分布：")
	b.WriteString(formatTopCounts(countInventoryExts(r.inventory), 8))
	b.WriteString("\n顶层目录分布：")
	b.WriteString(formatTopCounts(countInventoryTopDirs(r.inventory), 10))
	b.WriteString("\n低价值折叠目录：")
	b.WriteString(strings.Join(r.interesting.LowValueDirs, ", "))
	b.WriteString("\n\nInteresting Paths（来自 prompts/interesting_paths.json）：\n")
	interesting := r.interestingInventory(limit)
	if len(interesting) == 0 {
		b.WriteString("暂无命中。\n")
	} else {
		for _, item := range interesting {
			b.WriteString("- ")
			b.WriteString(item.Path)
			if item.LowValue {
				b.WriteString(" [low-value]")
			}
			b.WriteString("\n")
		}
		if len(interesting) >= limit {
			b.WriteString("- ...Interesting Paths 已截断，可用 list_files 按目录/模式继续查询。\n")
		}
	}
	b.WriteString("\n选择文件方式：file_review_update 支持 path、paths、dir/dirs + suffix/suffixes、pattern/patterns、items；批量选择上限 200 个文件。\n")
	return b.String()
}

func (r *Registry) interestingInventory(limit int) []InventoryEntry {
	var out []InventoryEntry
	for _, item := range r.inventory {
		if !item.Interesting {
			continue
		}
		out = append(out, item)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func countInventoryExts(items []InventoryEntry) map[string]int {
	counts := map[string]int{}
	for _, item := range items {
		ext := item.Ext
		if ext == "" {
			ext = "[no-ext]"
		}
		counts[ext]++
	}
	return counts
}

func countInventoryTopDirs(items []InventoryEntry) map[string]int {
	counts := map[string]int{}
	for _, item := range items {
		top := item.Path
		if idx := strings.Index(top, "/"); idx >= 0 {
			top = top[:idx] + "/"
		} else {
			top = "./"
		}
		counts[top]++
	}
	return counts
}

func formatTopCounts(counts map[string]int, limit int) string {
	if len(counts) == 0 {
		return " 无"
	}
	type pair struct {
		Name  string
		Count int
	}
	items := make([]pair, 0, len(counts))
	for name, count := range counts {
		items = append(items, pair{Name: name, Count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count == items[j].Count {
			return items[i].Name < items[j].Name
		}
		return items[i].Count > items[j].Count
	})
	if len(items) > limit {
		items = items[:limit]
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, fmt.Sprintf("%s=%d", item.Name, item.Count))
	}
	return " " + strings.Join(parts, ", ")
}

func (r *Registry) isLowValuePath(path string) bool {
	parts := strings.Split(strings.ToLower(path), "/")
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		for _, low := range r.interesting.LowValueDirs {
			if part == strings.ToLower(low) {
				return true
			}
		}
	}
	return false
}

func (r *Registry) isInterestingPath(path string) bool {
	lower := strings.ToLower(path)
	for _, keyword := range r.interesting.Keywords {
		keyword = strings.ToLower(strings.TrimSpace(keyword))
		if keyword != "" && strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

func loadInterestingPathsConfig() interestingPathsConfig {
	cfg := defaultInterestingPathsConfig()
	data, err := os.ReadFile(filepath.Join("prompts", "interesting_paths.json"))
	if err != nil {
		return cfg
	}
	var fromFile interestingPathsConfig
	if err := json.Unmarshal(data, &fromFile); err != nil {
		return cfg
	}
	if len(fromFile.Keywords) > 0 {
		cfg.Keywords = fromFile.Keywords
	}
	if len(fromFile.LowValueDirs) > 0 {
		cfg.LowValueDirs = fromFile.LowValueDirs
	}
	return cfg
}

func defaultInterestingPathsConfig() interestingPathsConfig {
	return interestingPathsConfig{
		Keywords:     []string{"admin", "api", "route", "router", "controller", "handler", "action", "auth", "login", "permission", "user", "session", "token", "upload", "file", "storage", "template", "theme", "plugin", "backup", "import", "export", "config", "install", "sql", "db", "database", "middleware"},
		LowValueDirs: []string{"vendor", "node_modules", "dist", "build", "target", "coverage", "static", "assets", "css", "img", "image", "images", "font", "fonts", "examples", "example", "testdata"},
	}
}

func normalizeInventoryPath(path string) string {
	path = strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	path = strings.TrimPrefix(path, "./")
	return strings.Trim(path, "/")
}

func (r *Registry) safePath(rel string) (string, error) {
	if rel == "" {
		rel = "."
	}
	path := rel
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.workspace, rel)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	root := filepath.Clean(r.workspace)
	clean := filepath.Clean(abs)
	if clean != root && !strings.HasPrefix(clean, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes workspace: %s", rel)
	}
	return clean, nil
}

func decodeArgs[T any](raw json.RawMessage) (T, error) {
	var args T
	if len(raw) == 0 {
		return args, nil
	}
	return args, json.Unmarshal(raw, &args)
}
