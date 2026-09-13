package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"code-review-agent/internal/agent"
	"code-review-agent/internal/forum"
	"code-review-agent/internal/tools"
	tea "github.com/charmbracelet/bubbletea"
)

func (m *Model) submit(value string) tea.Cmd {
	command, arg := parseCommand(value)
	if strings.HasPrefix(value, "/") || command == "go" || command == "budget" || command == "restore" || command == "report" || command == "list" || command == "export" || command == "save" || command == "session" || command == "sessions" || command == "agents" {
		switch command {
		case "say":
			return m.openBroadcast(arg)
		case "reply":
			idText, content, ok := strings.Cut(arg, " ")
			id, err := strconv.ParseInt(idText, 10, 64)
			if !ok || err != nil || id <= 0 {
				m.addEvent("用法：/reply <帖子或回复ID> <内容>")
				return nil
			}
			if err := m.runner.ReplyMessage(id, content); err != nil {
				m.addEvent("回复失败：" + err.Error())
			} else {
				m.refreshForumPage()
			}
		case "thread":
			id, err := strconv.ParseInt(arg, 10, 64)
			if err != nil || id <= 0 {
				m.addEvent("用法：/thread <帖子ID>")
				return nil
			}
			messages := m.runner.ForumMessages()
			var selected *forum.Post
			for _, post := range forum.GroupPosts(messages) {
				if post.ID == id {
					copy := post
					selected = &copy
					break
				}
			}
			if selected == nil {
				m.addEvent("帖子不存在或已过期")
				return nil
			}
			m.forumPage = forum.PostPage{Posts: []forum.Post{*selected}, Page: 1, PageSize: forum.DefaultPageSize, TotalPosts: 1, TotalPages: 1, Gap: len(messages) > 0 && messages[0].ID > 1}
			m.selectedPost = id
			m.expandedPosts = map[int64]bool{id: true}
			m.reconcileForum()
			m.threadID = id
			m.showForumPost(*selected)
			m.focus = 1
			m.scroll[1] = 0
			m.follow[1] = true
		case "forum":
			page := 1
			if arg != "" {
				var err error
				page, err = strconv.Atoi(arg)
				if err != nil || page < 1 {
					m.addEvent("用法：/forum [正整数页码]")
					return nil
				}
			}
			m.threadID = 0
			m.forumPage.Page = page
			m.selectedPost = 0
			m.focus = 1
			m.scroll[1] = 0
			m.follow[1] = true
			m.refreshForumPage()
		case "search":
			m.threadID = 0
			m.forumSearch = arg
			m.forumPage.Page = 1
			m.selectedPost = 0
			m.focus = 1
			m.scroll[1] = 0
			m.follow[1] = true
			m.refreshForumPage()
		case "go":
			if !m.workspaceReady {
				m.addEvent("请先输入审计目录，或 /restore <会话文件>。")
				return nil
			}
			query := "继续基于当前团队状态和论坛审计，不要从头开始。"
			if m.lastQuery != "" {
				query += "\n上次任务：\n" + m.lastQuery
			}
			// Do not recursively embed prior go prompts in the next run.
			previous := m.lastQuery
			cmd := m.runQuery(query)
			m.lastQuery = previous
			return cmd
		case "budget":
			if m.busy || m.saving {
				m.addEvent("运行或保存期间不可修改预算。")
				return nil
			}
			return m.openBudgetPrompt()
		case "dir":
			if m.budgetCfg.InfiniteMode {
				m.pendingDir = arg
				return m.openBudgetPrompt()
			}
			return m.startDirectory(arg)
		case "save":
			return m.saveSession(arg)
		case "session", "sessions":
			m.listSessions(arg)
		case "restore":
			m.restoreSession(arg)
		case "list":
			m.showFindings(arg)
		case "report":
			m.showReport()
		case "export":
			m.exportReport(arg)
		case "files":
			m.showFiles(arg)
		case "agents":
			m.showAgents()
		case "help":
			m.showDetail("操作帮助", []string{
				"输入目录或 /dir <目录>：开始新审计；支持带空格的引号路径。",
				"/budget：停止时打开预算表单；Tab 切换字段、←→ 选择模式、Ctrl+S 保存。0 表示不限；仅无限模式执行时间/token限额。",
				"Esc：对话框内关闭/取消；其他时候请求暂停，worker 停止后 go 继续。",
				"/say [消息]：打开广播草稿；Enter 换行，Ctrl+S 发送，Esc 取消。",
				"/thread <ID>：展开帖子及楼内回复；/forum [页码]：返回倒序帖子列表。",
				fmt.Sprintf("/search <关键词>：搜索标题、正文与回复；/search 清除；每页 %d 帖。", forum.DefaultPageSize),
				"/reply <ID> <内容>：回复帖子；论坛 Home/End 跳至当前页首/尾。",
				"/help、/report、/files [页码]、/sessions：独立对话框；只读对话框 gg 跳到底部。",
				"/list [页码]：漏洞选择列表；↑↓ 选择、Enter 详情、Esc 返回/关闭；后台继续运行。",
				"/agents：精简实时列表；↑↓/翻页/滚轮选择、Enter 完整诊断、Esc 返回列表。",
				"/export [文件]：完整 JSON 报告，默认 report.json。",
				"/save [文件]：保存当前会话；/session：选择会话，↑↓选择、Enter恢复。",
				"/restore [文件/片段]：支持省略.json、唯一片段；多个匹配弹窗选择，运行或保存时不可恢复。",
				"Tab 切换面板；论坛空输入时 ↑↓ 选帖、Enter 展开/收起、←→ 翻页。",
				"点击帖子标题展开/收起；PgUp/PgDn、鼠标滚轮在当前页独立滚动。",
				"主面板连续两次 Ctrl+G 跳到底部，不占用命令输入中的 g。",
				"/status：回到运行事件；窄屏用 Tab 切换状态与论坛。",
			})
		case "status":
			m.focus = 0
			m.follow[0] = true
		default:
			m.addEvent("未知命令；输入 /help 查看帮助。")
		}
		return nil
	}
	if m.busy {
		m.addEvent("运行期间请使用 /say [消息] 打开广播草稿，Ctrl+S 确认发送。")
		return nil
	}
	if m.saving {
		m.addEvent("请等待保存完成。")
		return nil
	}
	path := cleanInputDir(value)
	if !m.workspaceReady {
		if m.budgetCfg.InfiniteMode {
			m.pendingDir = path
			return m.openBudgetPrompt()
		}
		return m.startDirectory(path)
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		if m.budgetCfg.InfiniteMode {
			m.pendingDir = path
			return m.openBudgetPrompt()
		}
		return m.startDirectory(path)
	}
	return m.runQuery(value)
}
func parseCommand(value string) (string, string) {
	value = strings.TrimSpace(strings.TrimPrefix(value, "/"))
	parts := strings.Fields(value)
	if len(parts) == 0 {
		return "", ""
	}
	return strings.ToLower(parts[0]), strings.TrimSpace(value[len(parts[0]):])
}
func cleanInputDir(path string) string {
	path = strings.TrimSpace(path)
	if len(path) >= 2 && ((path[0] == '"' && path[len(path)-1] == '"') || (path[0] == '\'' && path[len(path)-1] == '\'')) {
		path = path[1 : len(path)-1]
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimLeft(path[1:], "/\\"))
		}
	}
	return path
}
func (m *Model) startDirectory(dir string) tea.Cmd {
	if m.busy || m.saving {
		m.addEvent("请等待当前运行或保存完成后再切换目录。")
		return nil
	}
	dir = cleanInputDir(dir)
	if dir == "" {
		m.addEvent("请输入审计目录。")
		return nil
	}
	if err := m.runner.SetWorkspace(dir); err != nil {
		m.addEvent("目录打开失败：" + err.Error())
		return nil
	}
	m.workspaceReady = true
	m.threadID = 0
	m.forumSearch = ""
	m.forumPage = forum.PostPage{}
	m.selectedPost = 0
	m.expandedPosts = nil
	m.workspace = dir
	m.sessionPath = ""
	m.modal = nil
	m.events = nil
	m.scroll = [2]int{}
	m.follow = [2]bool{true, true}
	m.syncTeam()
	return m.runQuery("请对当前工作区进行安全审计。先独立侦察并通过论坛交接，再由审计团队验证和报告有证据的问题。工作区：" + dir)
}
func (m *Model) saveSession(path string) tea.Cmd {
	if !m.workspaceReady {
		m.addEvent("尚未打开工作区，无法保存。")
		return nil
	}
	if m.saving {
		m.addEvent("会话正在保存，请稍候。")
		return nil
	}
	if path != "" {
		path = cleanInputDir(path)
	} else {
		path = m.sessionPath
	}
	if path == "" {
		path = filepath.Join(m.sessionDir, time.Now().Format("20060102-150405.000")+".json")
	}
	m.sessionPath = path
	m.saving = true
	runner, session := m.runner, m.session
	return func() tea.Msg {
		err := runner.SaveSession(path)
		return saveDoneMsg{session: session, path: path, err: err}
	}
}
func (m *Model) restoreSession(path string) {
	if m.busy || m.saving {
		m.addEvent("请等待当前运行或保存完成后再恢复会话。")
		return
	}
	path = cleanInputDir(path)
	if path == "" {
		m.listSessions("")
		return
	}
	if p, ok := m.resolveSessionPath(path); ok {
		path = p
	} else {
		matches := m.sessionMatches(path)
		if len(matches) == 1 {
			path = matches[0]
		} else if len(matches) > 1 {
			m.openSessionPicker(matches)
			return
		} else {
			m.addEvent("未找到会话：" + path)
			return
		}
	}
	if err := m.runner.LoadSession(path); err != nil {
		m.addEvent("恢复会话失败：" + err.Error())
		if m.modal != nil {
			m.modal.err = "恢复失败：" + err.Error()
		}
		return
	}
	m.session++
	m.sessionPath = path
	m.workspaceReady = true
	m.threadID = 0
	m.forumSearch = ""
	m.forumPage = forum.PostPage{}
	m.selectedPost = 0
	m.expandedPosts = nil
	m.workspace = "已恢复会话"
	m.lastQuery = ""
	m.modal = nil
	m.input.Focus()
	m.events = nil
	m.scroll = [2]int{}
	m.follow = [2]bool{true, true}
	m.syncTeam()
	m.addEvent("已恢复会话：" + path + "；输入 go 继续。")
}
func (m *Model) resolveSessionPath(path string) (string, bool) {
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return path, true
	}
	for _, p := range []string{path + ".json", filepath.Join(m.sessionDir, path), filepath.Join(m.sessionDir, path+".json")} {
		if filepath.IsAbs(path) && p != path+".json" {
			continue
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, true
		}
	}
	return "", false
}
func (m *Model) sessionMatches(query string) []string {
	entries, _ := os.ReadDir(m.sessionDir)
	q := strings.ToLower(strings.TrimSpace(query))
	if q != "" {
		q = strings.TrimSuffix(strings.ToLower(filepath.Base(q)), ".json")
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		n := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		l := strings.ToLower(n)
		if strings.HasPrefix(l, q) || strings.Contains(l, q) {
			out = append(out, filepath.Join(m.sessionDir, e.Name()))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}
func pageRange(arg string, total, pageSize int) (int, int, int) {
	page, _ := strconv.Atoi(strings.TrimSpace(arg))
	page = max(1, page)
	pages := max(1, (total+pageSize-1)/pageSize)
	page = min(page, pages)
	start := (page - 1) * pageSize
	return start, min(total, start+pageSize), page
}
func (m *Model) showDetail(title string, lines []string) {
	m.modal = &modalState{title: title, lines: lines}
	m.input.Blur()
	m.resizeModal()
}

func (m *Model) showAgents() {
	m.modal = &modalState{agents: true, agentState: &agentsModal{}}
	m.input.Blur()
	m.refreshAgentsModal()
}

func (m *Model) agentDetailLines() []string {
	state := m.modal
	lines := []string{fmt.Sprintf("当前阶段：%s · Agent 数：%d", safeText(m.runner.Phase()), len(m.workers)), ""}
	state.agentState.lineStarts = state.agentState.lineStarts[:0]
	if len(m.workers) == 0 {
		return append(lines, "当前没有已创建的 Agent。")
	}
	if !state.agentDetails {
		for i, status := range m.workers {
			state.agentState.lineStarts = append(state.agentState.lineStarts, len(lines))
			marker := "  "
			if i == state.agentIndex {
				marker = "> "
			}
			activity := agentSummary(status.Activity, 64)
			if activity == "" {
				activity = "暂无活动"
			}
			if status.Generation != nil {
				activity = generationLabel(status.Generation) + " · " + activity
			}
			lines = append(lines,
				fmt.Sprintf("%s[%s] %s · #%d", marker, agentSummary(status.Status, 24), agentSummary(agentName(status), 36), status.Turn),
				"  "+activity, "")
		}
		return lines
	}
	if state.agentIndex < 0 || state.agentIndex >= len(m.workers) {
		return append(lines, "该 Agent 已不在当前团队中。")
	}
	status := m.workers[state.agentIndex]
	lines = append(lines, "── 状态 ──", "名称："+agentName(status), "ID："+status.ID,
		fmt.Sprintf("[%s] 阶段 %s · 回合 #%d", status.Status, status.Phase, status.Turn),
		"活动："+agentValue(status.Activity), "", "── 当前请求输出 ──")
	if progress := status.Generation; progress != nil {
		lines = append(lines, generationLabel(progress)+fmt.Sprintf(" · reasoning %d tok", progress.ReasoningTokens))
		if progress.Estimated {
			lines = append(lines, fmt.Sprintf("tool≈%d tok · UTF-8 字节估算", progress.ToolTokens))
		} else {
			lines = append(lines, "提供方用量；不单独提供工具 token 数。")
		}
		lines = append(lines, "以上为当前请求输出，非上下文 token 数。")
		if progress.ReceivedAt == "" {
			lines = append(lines, "当前请求等待首段输出；尚无输出数据时间。")
		} else {
			lines = append(lines, "当前请求最后输出数据："+progress.ReceivedAt)
		}
	} else {
		lines = append(lines, "当前没有请求输出计数；不表示上下文为空。")
	}
	lines = append(lines, "", "── 活动时钟 ──", "最近模型输出："+agentValue(status.LastModelActivity),
		"最近工具："+agentValue(status.LastTool), "最近工具时间："+agentValue(status.LastToolActivity),
		"", "── 最近输出摘录 ──")
	if status.OutputPartial {
		lines = append(lines, "[partial] 未完成的输出摘录，不代表已执行工具或完整结论。")
	}
	lines = append(lines, agentValue(status.OutputExcerpt))
	for i := range lines {
		lines[i] = safeText(lines[i])
	}
	return lines
}
func (m *Model) openSessionPicker(items []string) {
	state := &modalState{title: "已保存会话 · ↑↓ 选择 Enter 恢复", sessions: &sessionsModal{items: append([]string(nil), items...)}}
	state.lines = make([]string, len(items))
	for i, item := range items {
		state.lines[i] = "  " + filepath.Base(item)
	}
	if len(items) > 0 {
		state.lines[0] = "> " + filepath.Base(items[0])
	}
	m.modal = state
	m.input.Blur()
	m.resizeModal()
}
func (m *Model) listSessions(arg string) {
	if _, err := os.ReadDir(m.sessionDir); err != nil {
		m.addEvent("读取会话目录失败：" + err.Error())
		return
	}
	matches := m.sessionMatches(arg)
	if strings.TrimSpace(arg) != "" && len(matches) == 0 {
		m.addEvent("未找到会话：" + arg)
		return
	}
	m.openSessionPicker(matches)
}
func (m *Model) showFindings(arg string) {
	m.snapshot = m.runner.Snapshot()
	start, _, _ := pageRange(arg, len(m.snapshot.Findings), 5)
	m.openFindings(m.snapshot.Findings, start)
}
func (m *Model) showFiles(arg string) {
	m.snapshot = m.runner.Snapshot()
	start, end, page := pageRange(arg, len(m.snapshot.Files), 20)
	lines := []string{fmt.Sprintf("第 %d 页 · 共 %d 个；/files <页码>", page, len(m.snapshot.Files))}
	for _, f := range m.snapshot.Files[start:end] {
		lines = append(lines, "["+f.Status+"] "+boundedText(f.Path, 500), boundedText(f.Note, 500))
	}
	m.showDetail("文件覆盖详情", lines)
}
func (m *Model) showReport() {
	m.snapshot = m.runner.Snapshot()
	m.showDetail("审计报告摘要", []string{
		fmt.Sprintf("漏洞 %d · 文件 %d · Todo %d · 变量 %d · 链路 %d", len(m.snapshot.Findings), len(m.snapshot.Files), len(m.snapshot.Todos), len(m.snapshot.Variables), len(m.snapshot.Flows)),
		"结论：" + boundedText(m.snapshot.Audit.Summary, 4000), "下一步：" + boundedText(m.snapshot.Audit.NextSteps, 2000),
		"项目笔记：" + boundedText(m.snapshot.Project.Note, 4000), "/list 查看证据；/export 导出完整数据。",
	})
}

type reportFiles struct {
	Reviewed   []tools.FileReview `json:"reviewed"`
	Unreviewed []tools.FileReview `json:"unreviewed"`
}

func (m *Model) exportReport(path string) {
	if path == "" {
		path = "report.json"
	} else {
		path = cleanInputDir(path)
	}
	snapshot := m.runner.Snapshot()
	revocations := m.runner.Revocations()
	files := reportFiles{Reviewed: []tools.FileReview{}, Unreviewed: []tools.FileReview{}}
	for _, f := range snapshot.Files {
		if f.Status == "reviewed" || f.Status == "skipped" {
			files.Reviewed = append(files.Reviewed, f)
		} else {
			files.Unreviewed = append(files.Unreviewed, f)
		}
	}
	// Preserve every previous report field, and include project notes that were
	// formerly available only through session persistence. UI truncation is not used.
	report := struct {
		GeneratedAt string                    `json:"generated_at"`
		Audit       tools.AuditState          `json:"audit"`
		Count       int                       `json:"count"`
		Findings    []tools.Finding           `json:"findings"`
		Revocations []agent.FindingRevocation `json:"revocations"`
		Todos       []tools.Todo              `json:"todos"`
		Files       reportFiles               `json:"files"`
		Variables   []tools.VariableReview    `json:"variables"`
		Flows       []tools.FlowReview        `json:"flows"`
		Project     tools.ProjectNote         `json:"project_note"`
	}{time.Now().Format(time.RFC3339), snapshot.Audit, len(snapshot.Findings), snapshot.Findings, revocations, snapshot.Todos, files, snapshot.Variables, snapshot.Flows, snapshot.Project}
	data, err := json.MarshalIndent(report, "", "  ")
	if err == nil {
		err = os.WriteFile(path, data, 0600)
	}
	if err != nil {
		m.addEvent("导出失败：" + err.Error())
		return
	}
	m.addEvent(fmt.Sprintf("已完整导出 %d 个漏洞及全部审计记录：%s", len(snapshot.Findings), path))
}
