package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Registry struct {
	workspace          string
	maxToolResultChars int
	todos              []Todo
	findings           []Finding
	files              []FileReview
	variables          []VariableReview
	flows              []FlowReview
	audit              AuditState
	nextTodoID         int
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
	Files     []FileReview     `json:"files"`
	Variables []VariableReview `json:"variables"`
	Flows     []FlowReview     `json:"flows"`
	Audit     AuditState       `json:"audit"`
}

func (r *Registry) RestoreSnapshot(snapshot Snapshot) {
	r.todos = append([]Todo(nil), snapshot.Todos...)
	r.findings = append([]Finding(nil), snapshot.Findings...)
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
	r := &Registry{workspace: abs, maxToolResultChars: maxToolResultChars, nextTodoID: 1}
	r.refreshFileInventory()
	return r, nil
}

func (r *Registry) SetWorkspace(workspace string) error {
	abs, err := cleanWorkspace(workspace)
	if err != nil {
		return err
	}
	r.workspace = abs
	r.todos = nil
	r.findings = nil
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
	case "report_finding":
		res = r.reportFinding(raw)
	case "end_audit":
		res = r.endAudit(raw)
	default:
		res = Result{OK: false, Error: "unknown tool: " + name}
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error())
	}
	out := string(data)
	if r.maxToolResultChars > 0 && len(out) > r.maxToolResultChars {
		out = out[:r.maxToolResultChars] + "\n...truncated..."
	}
	return out
}

func IsTerminalTool(name string) bool {
	return name == "end_audit"
}

func (r *Registry) Snapshot() Snapshot {
	return Snapshot{Todos: r.Todos(), Findings: r.Findings(), Files: r.Files(), Variables: r.Variables(), Flows: r.Flows(), Audit: r.Audit()}
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
- file_review_update：更新文件排查状态，支持单个或批量。单个参数：path、status、note。批量参数可用 paths、status、note，也可用 items:[{path,status,note}]。还支持选择器批量更新：dir/dirs、suffix/suffixes、pattern/patterns，例如 dir:"static"、suffix:".js"、pattern:"docs/*.md"、pattern:"*"。status 使用 unseen、reviewing、reviewed、skipped。note 用中文说明文件用途、当前发现或排查结论。不要在未读取文件内容时批量标记 reviewed；除非非常确定是与审计无关的资源文件、生成物或明显非项目审计对象，否则必须先 read_file 看清内容再标 reviewed。对 png/gif/jpg/svg/css/map 等静态资源或明显非代码文件，应优先用目录、后缀或模式一次批量标记 skipped，不要逐个刷屏。
- variable_review_update：更新变量/函数/符号排查状态。参数：name、path、status、note。status 使用 tracking、reviewed、suspicious、benign。note 用中文说明变量来源、传播路径、风险或排查结论。
- flow_review_update：更新跨文件调用链/数据流排查状态。参数：name、status、entry、files、variables、evidence、next_step、note。status 使用 tracking、reviewed、suspicious、confirmed、benign。用于记录入口函数、跨文件调用路径、变量传播、证据和下一步。
- flow_review_delete：删除已经闭环或不再需要展示的跨文件 flow。参数：name。flow 是临时工作队列，不要长期攒着。
- review_state：查看当前 todo、文件排查、变量排查、跨文件 flow 和漏洞状态。参数：limit。
- verify_finding：启动一个不会压缩上下文的漏洞验证子 agent，复核候选漏洞是否真实成立。子 agent 可以继续调用工具、复读多文件利用链，并最终返回中文验证结论。参数：severity、title、path、line、evidence、impact、recommendation、cwe。
- report_finding：只提交高置信度、证据清晰、利用链清楚、能造成实际危害的严重安全漏洞。调用前必须先用 variable_review_update 记录关键变量排查，并用 flow_review_update 记录跨文件入口、调用链、sink 和证据。参数：severity、title、path、line、evidence、impact、recommendation、cwe。severity 必须按系统提示词的严重性分级规则选择，不要随便报 high/critical；除 path 和 cwe 外尽量使用中文。
- end_audit：结束审计。参数：summary、next_steps。必须使用中文。调用前应先调用 review_state 检查文件排查状态。理想情况下文件都已 reviewed/skipped 再结束；不要因为只发现一个漏洞或主观觉得“没有审计价值”就提前结束，必须继续深挖同一入口、同一模块、相邻文件和同类路径，尽可能一次找全问题。只有关键入口和高价值链路已经尽可能覆盖后，才允许在收到第一次提醒后第二次再次调用 end_audit 直接结束。summary 必须总结 todo、文件覆盖情况、变量/flow 和漏洞结论。

怀疑发现漏洞时，先调用 variable_review_update 和 flow_review_update 补全变量传播和跨文件链路，然后优先调用 verify_finding 让漏洞验证子 agent 再次复核完整证据和利用链，不要直接调用 report_finding。flow 是临时排查队列：排查闭环、确认无害、已转成漏洞或不再需要时，必须调用 flow_review_delete 删除，不要攒着。只有确认严重安全漏洞且变量/flow 排查闭合，并且已经参考 verify_finding 返回的验证结论后，才能调用 report_finding。不要提交垃圾代码、普通 bug、低影响、无关紧要、无法证明危害或缺少清晰证据的问题。宁可少报，也不要乱报。发现一个漏洞后必须继续深挖相关入口、同类模式和相邻模块，尽可能找出更多问题；不要因为已有漏洞或主观觉得没有审计价值就结束。end_audit 在仍有未关闭文件时会先提醒一次；如果确认剩余文件无需继续逐个排查，第二次再次调用即可结束。`
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
	b.WriteString("## 文件结构与排查状态\n")
	counts := map[string]int{}
	for _, file := range r.files {
		counts[file.Status]++
	}
	b.WriteString(fmt.Sprintf("总文件数：%d，未看：%d，正在审计：%d，已看：%d，跳过：%d。\n\n", len(r.files), counts["unseen"], counts["reviewing"], counts["reviewed"], counts["skipped"]))
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
	var files []FileReview
	_ = filepath.WalkDir(r.workspace, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != r.workspace && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "dist" || name == "build") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(r.workspace, path)
		if err != nil {
			return nil
		}
		files = append(files, FileReview{Path: filepath.ToSlash(rel), Status: "unseen"})
		return nil
	})
	r.files = files
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
