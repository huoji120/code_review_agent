package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	r := &Registry{workspace: abs, maxToolResultChars: maxToolResultChars, nextTodoID: 1, interesting: loadInterestingPathsConfig()}
	r.refreshFileInventory()
	return r, nil
}

func (r *Registry) SetWorkspace(workspace string) error {
	abs, err := cleanWorkspace(workspace)
	if err != nil {
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

func (r *Registry) Workspace() string {
	return r.workspace
}

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
	out, _ := r.CallWithFullResult(name, raw)
	return out
}

func (r *Registry) CallWithFullResult(name string, raw json.RawMessage) (string, string) {
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
	default:
		res = Result{OK: false, Error: "unknown tool: " + name}
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		out := fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error())
		return out, out
	}
	out := string(data)
	full := out
	if r.maxToolResultChars > 0 && len(out) > r.maxToolResultChars {
		out = out[:r.maxToolResultChars] + "\n...truncated..."
	}
	return out, full
}

func IsKnownTool(name string) bool {
	switch name {
	case "list_files", "read_file", "search_content", "search_context", "git_inspect", "todo_create", "todo_update", "file_review_update", "variable_review_update", "flow_review_update", "flow_review_delete", "review_state", "project_note_update", "report_finding", "end_audit", "load_skill", "verify_finding", "audit_plan_done":
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
	return `# 工具调用协议

每次只能调用一个工具。工具调用必须使用下面格式：

<tool_call>
{"name":"tool_name","arguments":{"key":"value"}}
</tool_call>

` + "`<tool_call>`" + ` 标签内部必须是合法 JSON。不要用 markdown 代码块包裹工具调用。

禁止使用 <invoke>、<parameter> 或其他 XML 工具格式。即使模型习惯输出 invoke，也必须改成 <tool_call> JSON。

Windows 路径写入 JSON 时，如果使用反斜杠，必须写成双反斜杠，例如 C:\\path\\to\\repo，避免 JSON 转义破坏路径。

可用工具：

- list_files：列出文件。参数：root、pattern、max_depth、include_hidden、limit。pattern 默认按大小写不敏感匹配。系统启动时已经预载了初始文件清单，通常只在需要刷新视图时再调用。
- read_file：读取文件，支持偏移行读取。参数：path、offset、limit。path 优先传 review_state/list_files/search_content 返回的工作区相对路径，不要自己拼绝对路径；如果误传包含工作区名的绝对路径或唯一文件名，工具会尽量恢复到真实相对路径。返回 path、offset、limit、total_lines 和 lines，total_lines 表示文件总行数。
- search_content：搜索文件内容。参数：query、mode、root、include、limit、case_insensitive、case_sensitive。mode 支持 literal、regex、fuzzy，regex 支持跨行匹配。默认按大小写不敏感搜索；如需区分大小写再显式传 case_sensitive:true。include 支持文件名模式和 globstar，例如 *.java、**/*Controller.java；也支持相对路径模式，例如 src/main/java/*.java、src/main/*Controller.java，并默认按大小写不敏感匹配。工具名必须优先使用 search_content；如果误写成 search_context，系统会兼容转为 search_content。
- git_inspect：只读 Git 检查工具。只有“当前 Git 状态”显示 Git 可用时才调用；不可用或非 Git 仓库时调用会失败。参数：action、base、head、ref、commit、path、line_start、line_end、limit、context、staged、unstaged。action 支持 status、changed_files、diff、log、show、blame。用于增量审计、查看改动文件、diff、提交历史、特定版本文件和行级 blame。禁止用于 commit/push/pull/checkout/reset/merge/rebase 等写操作；本工具不会执行这些操作。
- todo_create：创建详细 todo。参数：title、priority。title 使用中文，必须包含具体文件/目录/模块/入口函数/变量/审计点，禁止空泛描述。
- todo_update：更新详细 todo。参数：id、status、title、priority。完成 todo 时必须传 status:"completed"；也兼容 status:"done"。title 使用中文，必须包含具体文件/目录/模块/入口函数/变量/审计点，禁止空泛描述。
- file_review_update：更新本次 one-shot 已纳入审计范围的文件状态，默认文件排查为空；未列出的 inventory 文件不代表 skipped。支持单个或批量。单个参数：path、status、note。批量参数可用 paths、status、note，也可用 items:[{path,status,note}]。还支持从本地 inventory 选择器批量加入：dir/dirs、suffix/suffixes、pattern/patterns，例如 dir:"admin"、suffix:".php"、pattern:"app/*Controller.java"、pattern:"var/Widget/*.php"。status 使用 unseen、reviewing、reviewed、skipped。note 用中文说明文件用途、当前发现或排查结论。不要在未读取文件内容时批量标记 reviewed；除非非常确定是与审计无关的资源文件、生成物或明显非项目审计对象，否则必须先 read_file 看清内容再标 reviewed。
- variable_review_update：更新变量/函数/符号排查状态。参数：name、path、status、note。status 使用 tracking、reviewed、suspicious、benign。note 用中文说明变量来源、传播路径、风险或排查结论。
- flow_review_update：更新跨文件调用链/数据流排查状态。参数：name、status、entry、files、variables、evidence、next_step、note。status 使用 tracking、reviewed、suspicious、confirmed、benign。用于记录入口函数、跨文件调用路径、变量传播、证据和下一步。
- flow_review_delete：删除已经闭环或不再需要展示的跨文件 flow。参数：name。flow 是临时工作队列，不要长期攒着。
- review_state：查看当前 todo、项目笔记、文件排查、变量排查、跨文件 flow 和漏洞状态。参数：limit。
- project_note_update：更新项目级自由文本笔记。参数：note。note 应该像人工审计员工作笔记一样尽量详细，模型自行组织结构；规划阶段必须积极、频繁维护，重点记录项目架构、运行行为、登录认证机制、鉴权机制、重要攻击面、数据/状态流、关键文件角色、已知结论和待确认问题。每次读到新的架构/认证/鉴权/入口信息后都应更新，不能只写摘要。
- verify_finding：启动一个不会压缩上下文的漏洞验证子 agent，复核候选漏洞是否真实成立。子 agent 可以继续调用工具、复读多文件利用链，并最终返回中文验证结论。参数：severity、title、path、line、evidence、impact、recommendation、cwe。
- report_finding：只提交高置信度、证据清晰、利用链清楚、能造成实际危害的严重安全漏洞。调用前必须先用 variable_review_update 记录关键变量排查，并用 flow_review_update 记录跨文件入口、调用链、sink 和证据。参数：severity、title、path、line、evidence、impact、recommendation、cwe。severity 必须按系统提示词的严重性分级规则选择，不要随便报 high/critical；除 path 和 cwe 外尽量使用中文。
- end_audit：结束审计。参数：summary、next_steps。必须使用中文。调用前应先调用 review_state 检查文件排查状态。理想情况下文件都已 reviewed/skipped 再结束；不要因为只发现一个漏洞、剩余文件很多、避免盲扫、某个方向无法闭环或主观觉得“没有审计价值”就提前结束，必须继续深挖同一入口、同一模块、相邻文件和同类路径，尽可能一次找全问题。只有关键入口、高风险文件类型、高价值链路、同类代表文件和相关搜索模式都已经尽可能覆盖后，才允许在收到第一次提醒后第二次再次调用 end_audit。summary 必须总结 todo、文件覆盖情况、剩余 unseen/reviewing 文件类别、变量/flow、漏洞结论和继续审计剩余项为何不会增加有效安全覆盖。

怀疑发现漏洞时，先调用 variable_review_update 和 flow_review_update 补全变量传播和跨文件链路，然后优先调用 verify_finding 让漏洞验证子 agent 再次复核完整证据和利用链，不要直接调用 report_finding。flow 是临时排查队列：排查闭环、确认无害、已转成漏洞或不再需要时，必须调用 flow_review_delete 删除，不要攒着。只有确认严重安全漏洞且变量/flow 排查闭合，并且已经参考 verify_finding 返回的验证结论后，才能调用 report_finding。不要提交垃圾代码、普通 bug、低影响、无关紧要、无法证明危害或缺少清晰证据的问题。宁可少报，也不要乱报。发现一个漏洞后必须继续深挖相关入口、同类模式和相邻模块，尽可能找出更多问题；不要因为已有漏洞、剩余文件很多、避免盲扫或主观觉得没有审计价值就结束。end_audit 在仍有未关闭文件时会先提醒一次；收到提醒后默认继续审计高价值剩余入口，只有确认剩余项都是静态资源、生成物、第三方样例、重复同类低价值文件，并且已通过代表性读取和搜索覆盖风险面时，第二次再次调用才可结束。`
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
