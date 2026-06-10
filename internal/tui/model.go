package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"code-review-agent/internal/config"
	"code-review-agent/internal/paniclog"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"golang.org/x/term"

	"code-review-agent/internal/agent"
	"code-review-agent/internal/tools"
)

type Model struct {
	runner         *agent.Agent
	input          textinput.Model
	log            []logEntry
	busy           bool
	width          int
	height         int
	cancel         context.CancelFunc
	events         chan agent.Event
	done           chan struct{}
	autoDir        string
	lastQuery      string
	session        int
	pane           string
	scroll         int
	sideScroll     int
	todos          []tools.Todo
	findings       []tools.Finding
	projectNote    tools.ProjectNote
	files          []tools.FileReview
	filesExpanded  bool
	variables      []tools.VariableReview
	flows          []tools.FlowReview
	audit          tools.AuditState
	loadedSkills   []string
	verifyTitle    string
	verifyTurn     int
	verifyLimit    int
	verifyStatus   string
	verifyVisible  bool
	queuedInputs   []string
	cmdMatchPrefix string
	cmdMatchIndex  int
	cmdSelected    int
	sessionDir     string
	sessionPath    string
	modelName      string
	autoSaveEvery  int
	turnCount      int
	saveCheckpoint int
	workspaceReady bool
	phase          string
	petState       string
	petFrame       int
	petAction      string
	petActiveTool  string
	petPreview     bool
}

type logEntry struct {
	Kind    string
	Content string
}

type eventMsg struct {
	session int
	event   agent.Event
}

type doneMsg struct{}

type cancelledMsg struct{}

type uiRefreshTickMsg struct {
	at     time.Time
	width  int
	height int
}

const welcomeHint = "代码审计 Agent。请输入要审计的目录。按 Ctrl+C 退出。"

const (
	petIdle     = "idle"
	petPlanning = "planning"
	petThinking = "thinking"
	petWriting  = "writing"
	petTool     = "tool"
	petDone     = "done"
	petStopped  = "stopped"

	petVariantNormal    = "normal"
	petVariantDetective = "detective"
	petVariantAlert     = "alert"

	petTickInterval           = 500 * time.Millisecond
	petPreviewFramesPerAction = 10
	uiRefreshInterval         = time.Second
)

type petPreviewAction struct {
	State  string
	Action string
	Label  string
}

var findingPetPreviewActions = []petPreviewAction{
	{State: petThinking, Action: "", Label: "发现线索"},
	{State: petTool, Action: "read", Label: "读证据"},
	{State: petTool, Action: "grep", Label: "搜入口"},
	{State: petTool, Action: "verify", Label: "复核漏洞"},
	{State: petTool, Action: "compress", Label: "整理上下文"},
	{State: petTool, Action: "report", Label: "提交漏洞"},
	{State: petDone, Action: "report", Label: "确认漏洞"},
}

type petRenderContext struct {
	State   string
	Frame   int
	Variant string
	Action  string
	Label   string
}

func New(runner *agent.Agent, cfg config.Config, autoDir string) Model {
	input := textinput.New()
	input.Placeholder = "Enter path, go, or /command"
	input.Focus()
	input.CharLimit = 4000
	input.Width = 100
	return Model{runner: runner, input: input, width: 120, height: 40, autoDir: autoDir, pane: "main", sessionDir: cfg.Agent.SessionDir, modelName: cfg.OpenAI.Model, autoSaveEvery: cfg.Agent.AutoSaveInterval, phase: runner.Phase(), petState: petIdle}
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink, petTick(), terminalSizeNow(), uiRefreshTick()}
	if strings.TrimSpace(m.autoDir) != "" {
		cmds = append(cmds, func() tea.Msg { return startAuditMsg(m.autoDir) })
	}
	return tea.Batch(cmds...)
}

type startAuditMsg string

type petTickMsg time.Time

func petTick() tea.Cmd {
	return tea.Tick(petTickInterval, func(t time.Time) tea.Msg { return petTickMsg(t) })
}

func uiRefreshTick() tea.Cmd {
	return tea.Tick(uiRefreshInterval, func(t time.Time) tea.Msg { return newUIRefreshTickMsg(t) })
}

func terminalSizeNow() tea.Cmd {
	return func() tea.Msg { return newUIRefreshTickMsg(time.Now()) }
}

func newUIRefreshTickMsg(t time.Time) uiRefreshTickMsg {
	width, height := currentTerminalSize()
	return uiRefreshTickMsg{at: t, width: width, height: height}
}

func currentTerminalSize() (int, int) {
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err == nil && width > 0 && height > 0 {
		return width, height
	}
	width, height, err = term.GetSize(int(os.Stderr.Fd()))
	if err == nil && width > 0 && height > 0 {
		return width, height
	}
	return 0, 0
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case petTickMsg:
		m.petFrame++
		return m, petTick()
	case uiRefreshTickMsg:
		if msg.width > 0 && msg.height > 0 && (msg.width != m.width || msg.height != m.height) {
			return m, tea.Batch(uiRefreshTick(), func() tea.Msg {
				return tea.WindowSizeMsg{Width: msg.width, Height: msg.height}
			})
		}
		m.scroll = min(m.scroll, m.maxScroll())
		m.sideScroll = min(m.sideScroll, m.maxSidebarScroll())
		return m, uiRefreshTick()
	case tea.WindowSizeMsg:
		m.applySize(msg.Width, msg.Height)
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		case "esc", "ctrl+[":
			return m.cancelAudit()
		case "enter":
			if strings.TrimSpace(m.input.Value()) == "" {
				return m, nil
			}
			if strings.HasPrefix(strings.TrimSpace(m.input.Value()), "/") {
				if selected, ok := m.selectedCommand(); ok {
					if strings.TrimSpace(m.input.Value()) != selected {
						m.input.SetValue(selected)
						m.input.CursorEnd()
						return m, nil
					}
				}
			}
			value := strings.TrimSpace(m.input.Value())
			m.input.SetValue("")
			if strings.HasPrefix(value, "/") {
				cmd, arg := parseSlashCommand(value)
				switch cmd {
				case "go":
					return m.runGo()
				case "list":
					m.log = append(m.log, logEntry{Kind: "assistant", Content: formatFindingsList(m.findings)})
					m.scroll = 0
					return m, nil
				case "export":
					return m.exportReport()
				case "files":
					m.toggleFilesExpanded()
					return m, nil
				case "save":
					return m.saveSession(true)
				case "restore":
					if strings.TrimSpace(arg) == "" {
						m.log = append(m.log, logEntry{Kind: "info", Content: "请使用 /restore <session-file> 恢复会话。"})
						m.trimLog()
						return m, nil
					}
					return m.restoreSession(arg)
				case "sessions":
					return m.listSessions()
				case "pet-preview":
					m.toggleFindingPetPreview()
					return m, nil
				default:
					m.log = append(m.log, logEntry{Kind: "error", Content: "未知命令：" + value})
					m.trimLog()
					return m, nil
				}
			}
			switch strings.ToLower(value) {
			case "go":
				return m.runGo()
			default:
				if strings.HasPrefix(strings.ToLower(value), "restore ") {
					return m.restoreSession(strings.TrimSpace(value[len("restore "):]))
				}
				if m.busy {
					m.queuedInputs = append(m.queuedInputs, value)
					m.log = append(m.log, logEntry{Kind: "info", Content: "已排队到下一次 go：" + truncateRunes(value, 120)})
					m.trimLog()
					return m, nil
				}
				query, startAudit := m.freeTextPrompt(value)
				if !startAudit {
					return m.runQuery(query)
				}
				return m.startAudit(value, query)
			}
		case "left":
			if m.input.Value() != "" {
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(msg)
				return m, cmd
			}
			if m.pane == "side" {
				m.toggleFilesExpanded()
				return m, nil
			}
			m.pane = "side"
			return m, nil
		case "right":
			if m.input.Value() != "" {
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(msg)
				return m, cmd
			}
			m.pane = "main"
			return m, nil
		case "up":
			if m.moveCommandSelection(-1) {
				return m, nil
			}
			m.scrollFocused(1)
			return m, nil
		case "down":
			if m.moveCommandSelection(1) {
				return m, nil
			}
			m.scrollFocused(-1)
			return m, nil
		case "pgup":
			m.scrollFocused(m.pageSize())
			return m, nil
		case "pgdown":
			m.scrollFocused(-m.pageSize())
			return m, nil
		case "home":
			m.scrollFocusedToEnd()
			return m, nil
		case "end":
			m.scrollFocusedToStart()
			return m, nil
		case "tab":
			if m.moveCommandSelection(1) {
				if selected, ok := m.selectedCommand(); ok {
					m.input.SetValue(selected)
					m.input.CursorEnd()
				}
				return m, nil
			}
			if next, ok := m.completeCommand(); ok {
				m.input.SetValue(next)
				m.input.CursorEnd()
			}
			return m, nil
		}
	case tea.MouseMsg:
		sideWidth, _ := m.panelWidths()
		switch msg.Type {
		case tea.MouseLeft:
			if msg.X < sideWidth && m.handleSidebarClick(msg.Y) {
				return m, nil
			}
			return m, nil
		case tea.MouseWheelUp:
			if msg.X < sideWidth {
				m.sideScroll = max(0, m.sideScroll-3)
			} else {
				m.scroll = min(m.maxScroll(), m.scroll+3)
			}
			return m, nil
		case tea.MouseWheelDown:
			if msg.X < sideWidth {
				m.sideScroll = min(m.maxSidebarScroll(), m.sideScroll+3)
			} else {
				m.scroll = max(0, m.scroll-3)
			}
			return m, nil
		}
	case startAuditMsg:
		dir := string(msg)
		return m.startAudit(dir, auditPrompt(dir))
	case eventMsg:
		if msg.session != m.session || !m.busy {
			return m, nil
		}
		e := msg.event
		m.appendEvent(e)
		m.scroll = min(m.scroll, m.maxScroll())
		m.sideScroll = min(m.sideScroll, m.maxSidebarScroll())
		return m, waitAgent(msg.session, m.events, m.done)
	case doneMsg:
		if m.autoSaveEvery > 0 {
			m.autoSaveSession()
		}
		m.busy = false
		m.petState = petDone
		m.petAction = ""
		m.petActiveTool = ""
		m.cancel = nil
		m.events = nil
		m.done = nil
		return m, nil
	case cancelledMsg:
		m.busy = false
		m.petState = petStopped
		m.petAction = ""
		m.petActiveTool = ""
		m.cancel = nil
		m.events = nil
		m.done = nil
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func parseSlashCommand(value string) (string, string) {
	value = strings.TrimSpace(strings.TrimPrefix(value, "/"))
	if value == "" {
		return "", ""
	}
	parts := strings.Fields(value)
	cmd := strings.ToLower(parts[0])
	arg := strings.TrimSpace(strings.TrimPrefix(value, parts[0]))
	return cmd, arg
}

func (m *Model) scrollFocused(delta int) {
	if m.pane == "side" {
		m.sideScroll = min(m.maxSidebarScroll(), max(0, m.sideScroll-delta))
		return
	}
	if delta > 0 {
		m.scroll = min(m.maxScroll(), m.scroll+delta)
	} else {
		m.scroll = max(0, m.scroll+delta)
	}
}

func (m *Model) scrollFocusedToEnd() {
	if m.pane == "side" {
		m.sideScroll = 0
		return
	}
	m.scroll = m.maxScroll()
}

func (m *Model) scrollFocusedToStart() {
	if m.pane == "side" {
		m.sideScroll = m.maxSidebarScroll()
		return
	}
	m.scroll = 0
}

func (m *Model) applySize(width, height int) {
	if width > 0 {
		m.width = width
	}
	if height > 0 {
		m.height = height
	}
	m.input.Width = max(1, m.width-4)
	m.scroll = min(m.scroll, m.maxScroll())
	m.sideScroll = min(m.sideScroll, m.maxSidebarScroll())
}

func (m Model) runGo() (tea.Model, tea.Cmd) {
	if m.busy {
		m.log = append(m.log, logEntry{Kind: "info", Content: "当前正在工作，请等待完成后再输入 go。"})
		m.trimLog()
		return m, nil
	}
	if !m.workspaceReady {
		m.log = append(m.log, logEntry{Kind: "info", Content: "请先输入要审计的目录，或使用 /restore <session-file> 恢复会话。"})
		m.trimLog()
		return m, nil
	}
	if len(m.queuedInputs) > 0 {
		query := "请先回答用户刚才的补充说明，然后再继续审计。\n\n用户补充如下：\n" + strings.Join(m.queuedInputs, "\n\n")
		m.queuedInputs = nil
		if m.lastQuery != "" {
			query += "\n\n上次任务如下：\n" + m.lastQuery + "\n\n请基于现有状态继续推进，不要从头开始。"
		}
		return m.runQuery(query)
	}
	query := "继续审计。请基于当前 todo、文件排查状态、变量排查状态、跨文件 flow 和已提交漏洞，选择下一步最有价值的安全审计动作，并继续使用工具。"
	if m.audit.Ended {
		query = "用户在审计完成后要求继续审计。请恢复审计状态，继续寻找尚未覆盖的文件、变量流和跨文件链路。不要坚持已经结束；如果继续发现问题，继续使用工具审计。"
	}
	if m.lastQuery != "" {
		query += "\n\n上次任务如下：\n" + m.lastQuery + "\n\n请不要从头开始，基于现有状态继续推进。"
	}
	return m.runQuery(query)
}

func (m *Model) completeCommand() (string, bool) {
	value := strings.TrimSpace(m.input.Value())
	if !strings.HasPrefix(value, "/") {
		return "", false
	}
	prefix := strings.TrimSpace(strings.TrimPrefix(value, "/"))
	commands := m.commandNames()
	var matches []string
	for _, command := range commands {
		if strings.HasPrefix(command, strings.ToLower(prefix)) {
			matches = append(matches, command)
		}
	}
	if len(matches) == 0 {
		return "", false
	}
	if prefix != m.cmdMatchPrefix {
		m.cmdMatchPrefix = prefix
		m.cmdMatchIndex = 0
	}
	choice := matches[m.cmdMatchIndex%len(matches)]
	m.cmdMatchIndex++
	if choice == "go" {
		return "/go", true
	}
	return "/" + choice, true
}

func (m *Model) commandNames() []string {
	return []string{"go", "list", "export", "files", "save", "restore", "sessions", "pet-preview"}
}

func (m Model) commandMatches() []string {
	value := strings.TrimSpace(m.input.Value())
	if !strings.HasPrefix(value, "/") {
		return nil
	}
	prefix := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(value, "/")))
	var matches []string
	for _, command := range m.commandNames() {
		if prefix == "" || strings.HasPrefix(command, prefix) {
			matches = append(matches, "/"+command)
		}
	}
	return matches
}

func (m *Model) moveCommandSelection(delta int) bool {
	matches := m.commandMatches()
	if len(matches) == 0 {
		m.cmdSelected = 0
		return false
	}
	m.cmdSelected += delta
	if m.cmdSelected < 0 {
		m.cmdSelected = len(matches) - 1
	}
	if m.cmdSelected >= len(matches) {
		m.cmdSelected = 0
	}
	return true
}

func (m *Model) selectedCommand() (string, bool) {
	matches := m.commandMatches()
	if len(matches) == 0 {
		return "", false
	}
	if m.cmdSelected < 0 || m.cmdSelected >= len(matches) {
		m.cmdSelected = 0
	}
	return matches[m.cmdSelected], true
}

func (m Model) commandHelpLine() string {
	base := "/命令 | go继续 | session: " + m.currentSessionLabel() + " | model: " + m.currentModelLabel()
	return base
}

func renderCommandMenu(matches []string, selected, width int) string {
	return renderCommandPalette(matches, selected, width)
}

func renderCommandPalette(matches []string, selected, width int) string {
	if len(matches) == 0 {
		return ""
	}
	if selected < 0 || selected >= len(matches) {
		selected = 0
	}
	if len(matches) > 8 {
		matches = matches[:8]
	}
	rows := make([]string, 0, len(matches))
	for i, match := range matches {
		label := padToWidth(match, 14)
		desc := commandDescription(match)
		prefix := "  "
		if i == selected {
			prefix = "> "
		}
		rows = append(rows, padToWidth(prefix+label+"  "+desc, width))
	}
	return strings.Join(rows, "\n")
}

func commandDescription(command string) string {
	switch command {
	case "/go":
		return "continue queued instructions"
	case "/list":
		return "show findings"
	case "/export":
		return "export report.json"
	case "/files":
		return "toggle file review list"
	case "/save":
		return "save current session"
	case "/restore":
		return "restore session file"
	case "/sessions":
		return "list saved sessions"
	case "/pet-preview":
		return "toggle finding pet preview"
	default:
		return ""
	}
}

func (m *Model) toggleFindingPetPreview() {
	m.petPreview = !m.petPreview
	m.petFrame = 0
	state := "关闭"
	if m.petPreview {
		state = "开启"
	}
	m.log = append(m.log, logEntry{Kind: "info", Content: "发现漏洞猫猫预览已" + state + "。"})
	m.trimLog()
}

func (m *Model) toggleFilesExpanded() {
	m.filesExpanded = !m.filesExpanded
	m.sideScroll = min(m.sideScroll, m.maxSidebarScroll())
}

func (m *Model) handleSidebarClick(y int) bool {
	if y < 1 || y > sidebarContentHeight(m.topPaneHeight()) {
		return false
	}
	sideWidth, _ := m.panelWidths()
	contentWidth := sidebarInnerWidth(sideWidth)
	visibleLines := m.visibleSidebarLines(contentWidth)
	lineIdx := y - 1
	if lineIdx < 0 || lineIdx >= len(visibleLines) {
		return false
	}
	line := stripANSI(strings.TrimSpace(visibleLines[lineIdx]))
	if line == "文件排查" || strings.Contains(line, "输入 files 展开") || strings.Contains(line, "输入 files 收起") {
		m.toggleFilesExpanded()
		return true
	}
	return false
}

func (m Model) startAudit(dir, query string) (tea.Model, tea.Cmd) {
	cleanDir := cleanInputDir(dir)
	if err := m.runner.SetWorkspace(cleanDir); err != nil {
		m.log = append(m.log, logEntry{Kind: "error", Content: fmt.Sprintf("目录打开失败：%q\n%s", cleanDir, err.Error())})
		return m, nil
	}
	m.workspaceReady = true
	return m.runQuery(query)
}

func (m Model) runQuery(query string) (tea.Model, tea.Cmd) {
	m.log = append(m.log, logEntry{Kind: "audit", Content: query})
	m.lastQuery = query
	m.turnCount++
	m.trimLog()
	m.busy = true
	m.phase = m.runner.Phase()
	m.petState = petThinking
	if m.inPlanPhase() {
		m.petState = petPlanning
	}
	m.petAction = ""
	m.petActiveTool = ""
	m.session++
	session := m.session
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.events = make(chan agent.Event, 32)
	m.done = make(chan struct{})
	events := m.events
	go func() {
		defer paniclog.RecoverWithContext("agent runner")

		m.runner.Run(ctx, query, func(e agent.Event) {
			select {
			case events <- e:
			case <-ctx.Done():
			}
		})
		close(events)
		close(m.done)
	}()
	return m, waitAgent(session, m.events, m.done)
}

func (m Model) cancelAudit() (tea.Model, tea.Cmd) {
	if m.cancel == nil {
		m.log = append(m.log, logEntry{Kind: "info", Content: "当前没有正在运行的审计。"})
		m.petState = petIdle
		m.petAction = ""
		m.petActiveTool = ""
		return m, nil
	}
	m.cancel()
	m.session++
	m.log = append(m.log, logEntry{Kind: "info", Content: "已发送中断信号，正在停止当前审计请求。"})
	return m, func() tea.Msg { return cancelledMsg{} }
}

func (m Model) exportReport() (tea.Model, tea.Cmd) {
	filesReviewed, filesUnreviewed := splitFilesForReport(m.files)
	report := struct {
		GeneratedAt string                 `json:"generated_at"`
		Audit       tools.AuditState       `json:"audit"`
		Count       int                    `json:"count"`
		Findings    []tools.Finding        `json:"findings"`
		Todos       []tools.Todo           `json:"todos"`
		Files       reportFiles            `json:"files"`
		Variables   []tools.VariableReview `json:"variables"`
		Flows       []tools.FlowReview     `json:"flows"`
	}{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Audit:       m.audit,
		Count:       len(m.findings),
		Findings:    m.findings,
		Todos:       m.todos,
		Files:       reportFiles{Reviewed: filesReviewed, Unreviewed: filesUnreviewed},
		Variables:   m.variables,
		Flows:       m.flows,
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		m.log = append(m.log, logEntry{Kind: "error", Content: "导出 report.json 失败：" + err.Error()})
		return m, nil
	}
	if err := os.WriteFile("report.json", data, 0600); err != nil {
		m.log = append(m.log, logEntry{Kind: "error", Content: "导出 report.json 失败：" + err.Error()})
		return m, nil
	}
	m.log = append(m.log, logEntry{Kind: "info", Content: fmt.Sprintf("已导出 %d 个漏洞到当前目录 report.json", len(m.findings))})
	m.trimLog()
	return m, nil
}

func (m *Model) autoSaveSession() {
	path := m.ensureSessionPath()
	if err := m.runner.SaveSession(path); err != nil {
		m.log = append(m.log, logEntry{Kind: "error", Content: "自动保存会话失败：" + err.Error()})
	} else {
		m.log = append(m.log, logEntry{Kind: "info", Content: "已自动保存会话到 " + path})
	}
	m.trimLog()
}

func (m *Model) maybeAutoSaveCheckpoint() {
	if m.autoSaveEvery <= 0 || !m.busy {
		return
	}
	m.saveCheckpoint++
	if m.saveCheckpoint%m.autoSaveEvery != 0 {
		return
	}
	m.autoSaveSession()
}

func (m Model) saveSession(manual bool) (tea.Model, tea.Cmd) {
	path := m.ensureSessionPath()
	if err := m.runner.SaveSession(path); err != nil {
		m.log = append(m.log, logEntry{Kind: "error", Content: "保存会话失败：" + err.Error()})
		return m, nil
	}
	msg := "已自动保存会话到 " + path
	if manual {
		msg = "已手动保存会话到 " + path
	}
	m.log = append(m.log, logEntry{Kind: "info", Content: msg})
	m.trimLog()
	return m, nil
}

func (m Model) restoreSession(path string) (tea.Model, tea.Cmd) {
	path = strings.Trim(path, "\"'")
	if !filepath.IsAbs(path) {
		path = filepath.Join(m.sessionDir, path)
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if err := m.runner.LoadSession(path); err != nil {
		m.log = append(m.log, logEntry{Kind: "error", Content: "恢复会话失败：" + err.Error()})
		return m, nil
	}
	m.sessionPath = path
	snapshot := m.runner.Snapshot()
	m.todos = snapshot.Todos
	m.findings = snapshot.Findings
	m.projectNote = snapshot.Project
	m.files = snapshot.Files
	m.variables = snapshot.Variables
	m.flows = snapshot.Flows
	m.audit = snapshot.Audit
	m.phase = m.runner.Phase()
	m.loadedSkills = m.runner.LoadedSkills()
	m.workspaceReady = true
	m.log = append(m.log, logEntry{Kind: "info", Content: "已恢复会话：" + path})
	m.turnCount = 0
	m.trimLog()
	return m, nil
}

func (m *Model) ensureSessionPath() string {
	if m.sessionPath != "" {
		return m.sessionPath
	}
	m.sessionPath = filepath.Join(m.sessionDir, time.Now().Format("20060102-150405")+".json")
	return m.sessionPath
}

func (m Model) listSessions() (tea.Model, tea.Cmd) {
	entries, err := os.ReadDir(m.sessionDir)
	if err != nil {
		m.log = append(m.log, logEntry{Kind: "error", Content: "读取 sessions 目录失败：" + err.Error()})
		return m, nil
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	content := "## 会话文件\n\n"
	if len(names) == 0 {
		content += "暂无会话文件。"
	} else {
		for _, name := range names {
			content += "- " + name + "\n"
		}
	}
	m.log = append(m.log, logEntry{Kind: "assistant", Content: content})
	m.trimLog()
	return m, nil
}

type reportFiles struct {
	Reviewed   []tools.FileReview `json:"reviewed"`
	Unreviewed []tools.FileReview `json:"unreviewed"`
}

func splitFilesForReport(files []tools.FileReview) ([]tools.FileReview, []tools.FileReview) {
	var reviewed []tools.FileReview
	var unreviewed []tools.FileReview
	for _, file := range files {
		switch file.Status {
		case "reviewed", "skipped":
			reviewed = append(reviewed, file)
		default:
			unreviewed = append(unreviewed, file)
		}
	}
	return reviewed, unreviewed
}

func formatFindingsList(findings []tools.Finding) string {
	if len(findings) == 0 {
		return "## 当前漏洞列表\n\n暂无已提交漏洞。"
	}
	var b strings.Builder
	b.WriteString("## 当前漏洞列表\n")
	for _, finding := range findings {
		b.WriteString(fmt.Sprintf("\n%d. **[%s] %s**\n", finding.ID, finding.Severity, finding.Title))
		b.WriteString(fmt.Sprintf("路径：`%s`", finding.Path))
		if finding.Line > 0 {
			b.WriteString(fmt.Sprintf(":%d", finding.Line))
		}
		b.WriteString("\n")
		if finding.Evidence != "" {
			b.WriteString("证据：")
			b.WriteString(finding.Evidence)
			b.WriteString("\n")
		}
		if finding.Impact != "" {
			b.WriteString("影响：")
			b.WriteString(finding.Impact)
			b.WriteString("\n")
		}
		if finding.Recommendation != "" {
			b.WriteString("修复建议：")
			b.WriteString(finding.Recommendation)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func (m *Model) appendEvent(e agent.Event) {
	switch e.Kind {
	case "state":
		m.phase = e.Phase
		m.loadedSkills = e.Skills
		if e.VerifyLimit > 0 {
			m.verifyTitle = e.VerifyTitle
			m.verifyTurn = e.VerifyTurn
			m.verifyLimit = e.VerifyLimit
			m.verifyStatus = e.VerifyStatus
		}
		m.todos = e.Todos
		m.findings = e.Findings
		m.projectNote = e.Project
		m.files = e.Files
		m.variables = e.Variables
		m.flows = e.Flows
		m.audit = e.Audit
		if m.busy && m.inPlanPhase() {
			m.petState = petPlanning
			if strings.TrimSpace(m.petAction) == "" {
				m.petAction = "map"
			}
		} else if m.busy && m.petState == petPlanning {
			m.petState = petThinking
			m.petAction = ""
		}
		m.maybeAutoSaveCheckpoint()
		return
	case "assistant_delta", "think_delta", "tool_call_delta":
		kind := strings.TrimSuffix(e.Kind, "_delta")
		if m.inPlanPhase() {
			m.petState = petPlanning
			m.updatePlanPetAction(kind, e.Content)
		} else if !m.keepActiveToolPet(kind) {
			m.petState = petStateForEventKind(kind)
			isNewEntry := len(m.log) == 0 || m.log[len(m.log)-1].Kind != kind
			m.updatePetActionForDelta(kind, e.Content, isNewEntry)
		}
		if len(m.log) > 0 && m.log[len(m.log)-1].Kind == kind {
			m.log[len(m.log)-1].Content += e.Content
			return
		}
		m.log = append(m.log, logEntry{Kind: kind, Content: e.Content})
	case "assistant_done":
		return
	case "verify_progress":
		m.petState = petTool
		m.petAction = "verify"
		m.petActiveTool = "verify"
		m.verifyTitle = e.VerifyTitle
		m.verifyTurn = e.VerifyTurn
		m.verifyLimit = e.VerifyLimit
		m.verifyStatus = e.VerifyStatus
		m.verifyVisible = true
		m.upsertVerifyLog()
		m.trimLog()
		return
	case "verify_done":
		m.petState = petTool
		m.petAction = "verify"
		m.petActiveTool = "verify"
		m.verifyStatus = e.VerifyStatus
		m.verifyTurn = e.VerifyLimit
		m.verifyVisible = false
		m.upsertVerifyLog()
		m.trimLog()
		return
	case "ui_compact":
		m.petState = petTool
		m.petAction = "compress"
		m.petActiveTool = ""
		m.log = []logEntry{{Kind: "assistant", Content: e.Content}}
	default:
		if e.Kind == "tool" {
			if m.inPlanPhase() {
				m.petState = petPlanning
			} else {
				m.petState = petTool
			}
			m.petAction = m.toolActionForEvent(e.Content)
		} else if e.Kind == "error" {
			m.petState = petStopped
			m.petAction = ""
			m.petActiveTool = ""
		}
		m.log = append(m.log, logEntry{Kind: e.Kind, Content: e.Content})
	}
	m.trimLog()
}

func (m Model) inPlanPhase() bool {
	return m.phase == "plan"
}

func (m *Model) updatePlanPetAction(kind, content string) {
	switch kind {
	case "tool_call", "tool":
		if action, ok := toolActionFromContent(content); ok {
			m.petAction = action
			return
		}
	case "assistant":
		m.petAction = "map"
		return
	case "think":
		m.petAction = "plan"
		return
	}
	if strings.TrimSpace(m.petAction) == "" {
		m.petAction = "map"
	}
}

func petStateForEventKind(kind string) string {
	switch kind {
	case "think":
		return petThinking
	case "assistant":
		return petWriting
	case "tool_call", "tool":
		return petTool
	default:
		return petIdle
	}
}

func petActionForEventKind(kind, content string) string {
	switch kind {
	case "assistant":
		return "pen"
	case "tool_call", "tool":
		return toolActionLabel(content)
	default:
		return ""
	}
}

func (m *Model) updatePetActionForDelta(kind, content string, isNewEntry bool) {
	switch kind {
	case "assistant":
		m.petAction = "pen"
	case "tool_call", "tool":
		if action, ok := toolActionFromContent(content); ok {
			m.petAction = action
		} else if isNewEntry || strings.TrimSpace(m.petAction) == "" {
			m.petAction = "tool"
		}
	default:
		m.petAction = ""
	}
}

func (m *Model) keepActiveToolPet(kind string) bool {
	if strings.TrimSpace(m.petActiveTool) == "" {
		return false
	}
	if kind != "think" && kind != "assistant" {
		return false
	}
	m.petState = petTool
	m.petAction = m.petActiveTool
	return true
}

func (m *Model) toolActionForEvent(content string) string {
	trimmed := strings.TrimSpace(content)
	action, ok := toolActionFromContent(trimmed)
	if ok {
		if isToolStartEvent(trimmed) {
			m.petActiveTool = action
		} else if m.petActiveTool == action {
			m.petActiveTool = ""
		}
		return action
	}
	if m.petActiveTool != "" {
		action = m.petActiveTool
		if !isToolStartEvent(trimmed) {
			m.petActiveTool = ""
		}
		return action
	}
	return "tool"
}

func isToolStartEvent(content string) bool {
	content = strings.TrimSpace(content)
	return strings.HasPrefix(content, "calling ") || strings.HasPrefix(content, "compressing context")
}

func toolActionLabel(content string) string {
	if action, ok := toolActionFromContent(content); ok {
		return action
	}
	return "tool"
}

func toolActionFromContent(content string) (string, bool) {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "calling ")
	if content == "" {
		return "", false
	}
	if payload, ok := findToolCallJSONObject(cleanToolCallContent(content)); ok {
		var call ToolCallForPet
		if err := json.Unmarshal([]byte(payload), &call); err == nil && call.Name != "" {
			content = call.Name
		}
	}
	content = strings.TrimSpace(strings.Split(content, "\n")[0])
	content = strings.Trim(content, "{}[]()\"'")
	if idx := strings.Index(content, " "); idx >= 0 {
		content = content[:idx]
	}
	content = strings.TrimPrefix(content, "tool_call:")
	content = strings.TrimSpace(content)
	lowerContent := strings.ToLower(content)
	switch {
	case strings.Contains(lowerContent, "audit_plan_done"):
		return "done", true
	case strings.Contains(lowerContent, "list_files") || strings.Contains(lowerContent, "review_state"):
		return "map", true
	case strings.Contains(lowerContent, "read"):
		return "read", true
	case strings.Contains(lowerContent, "compress"):
		return "compress", true
	case strings.Contains(lowerContent, "search"):
		return "grep", true
	case strings.Contains(lowerContent, "git"):
		return "git", true
	case strings.Contains(lowerContent, "todo"):
		return "todo", true
	case strings.Contains(lowerContent, "review"):
		return "note", true
	case strings.Contains(lowerContent, "verify"):
		return "verify", true
	case strings.Contains(lowerContent, "verification"):
		return "verify", true
	case strings.Contains(lowerContent, "report"):
		return "report", true
	}
	return "", false
}

type ToolCallForPet struct {
	Name string `json:"name"`
}

func truncateASCII(text string, limit int) string {
	if len(text) <= limit {
		if text == "" {
			return "tool"
		}
		return text
	}
	return text[:limit]
}

const maxLogEntries = 8
const maxLogEntryChars = 6000
const maxSidebarFilesCollapsed = 30
const maxSidebarProjectNoteLines = 8

func (m *Model) trimLog() {
	if len(m.log) > maxLogEntries {
		m.log = m.log[len(m.log)-maxLogEntries:]
	}
	for i := range m.log {
		if len(m.log[i].Content) > maxLogEntryChars {
			m.log[i].Content = "...已裁剪旧 UI 文本...\n" + m.log[i].Content[len(m.log[i].Content)-maxLogEntryChars:]
		}
	}
	m.scroll = min(m.scroll, m.maxScroll())
}

func auditPrompt(input string) string {
	dir := pathForPrompt(cleanInputDir(input))
	if dir == "" {
		dir = "."
	}
	return "请自动审计这个目录：" + dir + "\n先列出文件，再检查相关代码。发现具体漏洞后必须立刻调用 report_finding。审计完成后必须调用 end_audit。全程使用中文工作。"
}

func followupPrompt(input string) string {
	return "用户补充要求：\n" + strings.TrimSpace(input) + "\n\n请基于当前工作区、当前 todo、文件排查状态、变量排查状态、跨文件 flow 和已提交漏洞继续推进。不要把这段补充要求当成新的目录路径；如果用户指出具体风险方向，优先用工具验证该方向。全程使用中文。"
}

func (m Model) freeTextPrompt(input string) (string, bool) {
	if !m.workspaceReady {
		return auditPrompt(input), true
	}
	return followupPrompt(input), false
}

func cleanInputDir(input string) string {
	dir := strings.TrimSpace(input)
	dir = strings.Trim(dir, "\"'")
	if dir == "" {
		return "."
	}
	return filepath.Clean(dir)
}

func pathForPrompt(path string) string {
	return filepath.ToSlash(path)
}

func (m Model) View() string {
	availableWidth := max(1, m.width-4)
	sideWidth, contentWidth := panelWidthsFor(availableWidth)
	topHeight := m.topPaneHeight()
	bodyLines := m.visibleLogLines(contentWidth)
	body := strings.Join(bodyLines, "\n")
	status := "ready"
	if m.busy {
		status = "running"
	}
	scrollInfo := ""
	if m.maxScroll() > 0 {
		scrollInfo = fmt.Sprintf(" scroll %d/%d", m.scroll, m.maxScroll())
	}
	mainParts := []string{m.mainHeader(contentWidth, status+scrollInfo)}
	mainParts = append(mainParts, fixedHeightBlock(body, contentWidth, m.bodyHeight()))
	main := lipgloss.JoinVertical(lipgloss.Left, mainParts...)
	if sideWidth <= 0 {
		top := fixedHeightBlock(main, contentWidth, topHeight)
		footer := m.renderFooter(m.width)
		return lipgloss.JoinVertical(lipgloss.Left, top, "", footer)
	}
	left := renderSidebar(m.loadedSkills, m.todos, m.findings, m.projectNote, m.files, m.filesExpanded, m.variables, m.flows, sideWidth, topHeight, m.sideScroll, m.pane == "side", m.petContext())
	top := renderColumns(left, main, sideWidth, contentWidth, topHeight)
	footer := m.renderFooter(m.width)
	return lipgloss.JoinVertical(lipgloss.Left, top, "", footer)
}

func (m Model) panelWidths() (int, int) {
	return panelWidthsFor(max(1, m.width-4))
}

func panelWidthsFor(width int) (int, int) {
	if width <= 0 {
		return 0, 1
	}
	if width < 60 {
		return 0, width
	}
	const gap = 2
	mainMin := 44
	sideMin := 24
	side := width * 46 / 100
	if width < 90 {
		mainMin = 32
		sideMin = 20
	}
	if side < sideMin {
		side = sideMin
	}
	maxSide := width - mainMin - gap
	if maxSide < sideMin {
		maxSide = sideMin
	}
	if maxSide > 120 {
		maxSide = 120
	}
	if side > maxSide {
		side = maxSide
	}
	main := width - side - gap
	if main < 1 {
		main = 1
		side = max(0, width-gap-main)
	}
	if side+main+gap > width {
		main = max(1, width-side-gap)
	}
	return side, main
}

func (m Model) topPaneHeight() int {
	return max(1, m.height-m.footerHeight()-1)
}

func (m Model) visibleLogLines(width int) []string {
	lines := m.renderLogLines(width)
	if len(lines) == 0 {
		return nil
	}
	page := m.bodyHeight()
	if len(lines) <= page {
		return padLines(lines, page)
	}
	end := len(lines) - m.scroll
	if end > len(lines) {
		end = len(lines)
	}
	if end < 0 {
		end = 0
	}
	start := end - page
	if start < 0 {
		start = 0
	}
	return padLines(lines[start:end], page)
}

func (m Model) renderLogLines(width int) []string {
	var rendered []string
	for _, entry := range m.log {
		rendered = append(rendered, renderEntry(entry, width))
	}
	body := strings.Join(rendered, "\n\n")
	if body == "" {
		return nil
	}
	return strings.Split(body, "\n")
}

func (m Model) mainHeader(width int, status string) string {
	header := styleTitle.Render("Code Review Agent") + " " + styleStatus.Render(status)
	if m.workspaceReady || len(m.log) > 0 {
		return header
	}
	used := runewidth.StringWidth(stripANSI(header))
	remaining := width - used - 2
	if remaining <= 0 {
		return header
	}
	return header + "  " + fitLine(welcomeHint, remaining)
}

func (m Model) bodyHeight() int {
	return max(1, m.topPaneHeight()-1)
}

func (m Model) commandMenuHeight() int {
	return m.footerCommandHeight()
}

func (m Model) footerHeight() int {
	return m.footerCommandHeight() + 2
}

func (m Model) footerCommandHeight() int {
	matches := m.commandMatches()
	if len(matches) == 0 {
		return 0
	}
	if len(matches) > 8 {
		return 8
	}
	return len(matches)
}

func (m Model) renderFooter(width int) string {
	parts := []string{}
	if menu := renderCommandPalette(m.commandMatches(), m.cmdSelected, width); menu != "" {
		parts = append(parts, menu)
	}
	inputLine := lipgloss.NewStyle().Background(lipgloss.Color("235")).Render(padStyledToWidth("> "+m.input.View(), width))
	statusLine := styleHelp.Render(padToWidth(m.commandHelpLine(), width))
	parts = append(parts, inputLine, statusLine)
	return strings.Join(parts, "\n")
}

func (m Model) currentSessionLabel() string {
	if m.sessionPath == "" {
		return "未保存"
	}
	if rel, err := filepath.Rel(".", m.sessionPath); err == nil && rel != "" && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(m.sessionPath)
}

func (m Model) currentModelLabel() string {
	if strings.TrimSpace(m.modelName) == "" {
		return "unknown"
	}
	return m.modelName
}

func (m *Model) upsertVerifyLog() {
	content := m.verifyProgressText()
	for i := len(m.log) - 1; i >= 0; i-- {
		if m.log[i].Kind == "verify" {
			m.log[i].Content = content
			if !m.verifyVisible && i == len(m.log)-1 {
				m.log = append(m.log[:i], m.log[i+1:]...)
			}
			return
		}
	}
	if m.verifyVisible {
		m.log = append(m.log, logEntry{Kind: "verify", Content: content})
	}
}

func (m Model) verifyProgressText() string {
	if m.verifyLimit <= 0 || m.verifyTitle == "" {
		return ""
	}
	progress := m.verifyTurn
	if progress > m.verifyLimit {
		progress = m.verifyLimit
	}
	barWidth := 10
	filled := 0
	if m.verifyLimit > 0 {
		filled = progress * barWidth / m.verifyLimit
	}
	if filled > barWidth {
		filled = barWidth
	}
	bar := "[" + strings.Repeat("#", filled) + strings.Repeat(".", barWidth-filled) + "]"
	status := m.verifyStatus
	if status == "" {
		status = "验证中"
	}
	status = truncateRunes(status, 28)
	return fmt.Sprintf("%s %d/%d %s", bar, progress, m.verifyLimit, status)
}

func truncateRunes(text string, maxLen int) string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	return string(runes[:maxLen]) + "..."
}

func projectNotePreviewLines(note string, width, maxLines int) []string {
	note = strings.TrimSpace(stripANSI(note))
	if note == "" || maxLines <= 0 {
		return nil
	}
	var lines []string
	for _, raw := range strings.Split(note, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		for _, wrapped := range wrapLine(raw, width) {
			lines = append(lines, fitLine(wrapped, width))
			if len(lines) >= maxLines {
				lines[len(lines)-1] = fitLine(lines[len(lines)-1]+" ...", width)
				return lines
			}
		}
	}
	return lines
}

func (m Model) pageSize() int {
	return m.bodyHeight()
}

func (m Model) maxScroll() int {
	_, contentWidth := m.panelWidths()
	maxScroll := len(m.renderLogLines(contentWidth)) - m.pageSize()
	if maxScroll < 0 {
		return 0
	}
	return maxScroll
}

func (m Model) maxSidebarScroll() int {
	sideWidth, _ := m.panelWidths()
	contentWidth := sidebarInnerWidth(sideWidth)
	lines := sidebarLines(m.loadedSkills, m.todos, m.findings, m.projectNote, m.files, m.filesExpanded, m.variables, m.flows, contentWidth)
	listHeight := sidebarContentHeight(m.topPaneHeight()) - len(renderPetAreaLines(m.petContext(), contentWidth))
	maxScroll := len(lines) - max(1, listHeight)
	if maxScroll < 0 {
		return 0
	}
	return maxScroll
}

func (m Model) visibleSidebarLines(contentWidth int) []string {
	lines := sidebarLines(m.loadedSkills, m.todos, m.findings, m.projectNote, m.files, m.filesExpanded, m.variables, m.flows, contentWidth)
	contentHeight := max(1, sidebarContentHeight(m.topPaneHeight())-len(renderPetAreaLines(m.petContext(), contentWidth)))
	if len(lines) > contentHeight {
		start := min(m.sideScroll, len(lines)-contentHeight)
		end := start + contentHeight
		lines = lines[start:end]
	}
	return padLines(lines, contentHeight)
}

func (m Model) petStateForRender() string {
	if strings.TrimSpace(m.petState) == "" {
		return petIdle
	}
	return m.petState
}

func (m Model) petContext() petRenderContext {
	if m.petPreview && len(findingPetPreviewActions) > 0 {
		preview := findingPetPreviewActions[(m.petFrame/petPreviewFramesPerAction)%len(findingPetPreviewActions)]
		return petRenderContext{State: preview.State, Frame: m.petFrame, Variant: petVariantAlert, Action: preview.Action, Label: preview.Label}
	}
	state := m.petStateForRender()
	action := m.petAction
	return petRenderContext{State: state, Frame: m.petFrame, Variant: petVariantForFindings(m.findings), Action: action}
}

func petVariantForFindings(findings []tools.Finding) string {
	variant := petVariantNormal
	for _, finding := range findings {
		switch strings.ToLower(finding.Severity) {
		case "critical", "high":
			return petVariantAlert
		case "medium", "low", "info":
			variant = petVariantDetective
		default:
			if variant == petVariantNormal {
				variant = petVariantDetective
			}
		}
	}
	return variant
}

func waitAgent(session int, events <-chan agent.Event, done <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-events
		if !ok {
			return doneMsg{}
		}
		return eventMsg{session: session, event: e}
	}
}

func renderEntry(e logEntry, width int) string {
	switch e.Kind {
	case "audit":
		return sectionHeader("audit", styleUser, width) + "\n" + renderMarkdown(e.Content, width)
	case "assistant":
		return sectionHeader("assistant", styleAssistant, width) + "\n" + renderMarkdown(e.Content, width)
	case "think":
		return sectionHeader("think", styleThink, width) + "\n" + renderMarkdown(e.Content, width)
	case "tool":
		return renderTool(e.Content, width)
	case "tool_call":
		return renderToolCall(e.Content, width)
	case "verify":
		return sectionHeader("verify", styleTool, width) + "\n" + wrapBlock(e.Content, width)
	case "error":
		return sectionHeader("error", styleError, width) + "\n" + wrapBlock(e.Content, width)
	case "hint":
		return renderHint(e.Content, width)
	case "info":
		if isWelcomeHint(e.Content) {
			return renderHint(e.Content, width)
		}
		return styleMuted.Render(wrapBlock(e.Content, width))
	default:
		return fmt.Sprintf("%s: %s", e.Kind, e.Content)
	}
}

func renderHint(content string, width int) string {
	return fitLine(content, width)
}

func isWelcomeHint(content string) bool {
	return strings.TrimSpace(content) == welcomeHint
}

func sectionHeader(label string, style lipgloss.Style, width int) string {
	text := " " + label + " "
	return style.Render(text)
}

func renderMarkdown(content string, width int) string {
	if strings.TrimSpace(content) == "" {
		return ""
	}
	return renderSimpleMarkdown(content, width)
}

func renderSimpleMarkdown(content string, width int) string {
	lines := strings.Split(stripANSI(content), "\n")
	var out []string
	inCode := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inCode = !inCode
			out = append(out, repeatToWidth("-", width))
			continue
		}
		if inCode {
			out = append(out, wrapLine("  "+line, width)...)
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "### "):
			out = append(out, "", "### "+cleanInlineMarkdown(strings.TrimSpace(trimmed[4:])))
		case strings.HasPrefix(trimmed, "## "):
			out = append(out, "", "## "+cleanInlineMarkdown(strings.TrimSpace(trimmed[3:])))
		case strings.HasPrefix(trimmed, "# "):
			out = append(out, "", "# "+cleanInlineMarkdown(strings.TrimSpace(trimmed[2:])))
		case strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* "):
			out = append(out, wrapLine("  • "+cleanInlineMarkdown(strings.TrimSpace(trimmed[2:])), width)...)
		case isNumberedList(trimmed):
			out = append(out, wrapLine("  "+cleanInlineMarkdown(trimmed), width)...)
		case strings.HasPrefix(trimmed, ">"):
			out = append(out, wrapLine("  │ "+cleanInlineMarkdown(strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))), width)...)
		case trimmed == "---" || trimmed == "***":
			out = append(out, repeatToWidth("-", width))
		default:
			out = append(out, wrapLine(cleanInlineMarkdown(line), width)...)
		}
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}

func cleanInlineMarkdown(text string) string {
	text = strings.ReplaceAll(text, "**", "")
	text = strings.ReplaceAll(text, "__", "")
	text = strings.ReplaceAll(text, "`", "'")
	return text
}

func isNumberedList(text string) bool {
	idx := strings.Index(text, ". ")
	if idx <= 0 || idx > 3 {
		return false
	}
	for _, r := range text[:idx] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func renderTool(content string, width int) string {
	trimmed := strings.TrimSpace(content)
	if strings.Contains(trimmed, "end_audit") || strings.Contains(trimmed, "审计完成工具") {
		return sectionHeader("end_audit", styleSuccess, width) + "\n" + wrapBlock(trimmed, width)
	}
	if strings.HasPrefix(trimmed, "calling ") {
		return sectionHeader("tool", styleTool, width) + "\n" + styleToolName.Render(strings.TrimPrefix(trimmed, "calling "))
	}
	return sectionHeader("tool", styleTool, width) + "\n" + wrapBlock(trimmed, width)
}

func renderToolCall(content string, width int) string {
	trimmed := normalizeRenderedToolCallContent(content)
	var pretty strings.Builder
	var payload struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(trimmed), &payload); err == nil && payload.Name != "" {
		pretty.WriteString("tool_call: ")
		pretty.WriteString(payload.Name)
		if len(payload.Arguments) > 0 {
			var args interface{}
			if err := json.Unmarshal(payload.Arguments, &args); err == nil {
				data, _ := json.MarshalIndent(args, "", "  ")
				pretty.WriteString("\n")
				pretty.WriteString(string(data))
			}
		}
		return sectionHeader("tool_call", styleTool, width) + "\n" + wrapBlock(pretty.String(), width)
	}
	return sectionHeader("tool_call", styleTool, width) + "\n" + wrapBlock(trimmed, width)
}

func cleanToolCallContent(content string) string {
	trimmed := strings.TrimSpace(content)
	trimmed = strings.TrimPrefix(trimmed, "<tool_call>")
	trimmed = strings.TrimSuffix(trimmed, "</tool_call>")
	return strings.TrimSpace(trimmed)
}

func normalizeRenderedToolCallContent(content string) string {
	trimmed := cleanToolCallContent(content)
	if nested, ok := extractNestedToolCallContent(trimmed); ok {
		trimmed = nested
	}
	if payload, ok := findToolCallJSONObject(trimmed); ok {
		return payload
	}
	return trimmed
}

func extractNestedToolCallContent(text string) (string, bool) {
	open := "<tool_call>"
	close := "</tool_call>"
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

func findToolCallJSONObject(text string) (string, bool) {
	var payload struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	trimmed := strings.TrimSpace(text)
	if err := json.Unmarshal([]byte(trimmed), &payload); err == nil && payload.Name != "" {
		return trimmed, true
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != '{' {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(trimmed[i:]))
		if err := decoder.Decode(&payload); err == nil && payload.Name != "" {
			data, _ := json.Marshal(payload)
			return string(data), true
		}
	}
	return "", false
}

func renderSidebar(skills []string, todos []tools.Todo, findings []tools.Finding, projectNote tools.ProjectNote, files []tools.FileReview, filesExpanded bool, variables []tools.VariableReview, flows []tools.FlowReview, width, height, scroll int, focused bool, pet petRenderContext) string {
	if width <= 0 {
		return ""
	}
	contentWidth := sidebarInnerWidth(width)
	contentHeight := sidebarContentHeight(height)
	petLines := renderPetAreaLines(pet, contentWidth)
	listHeight := max(1, contentHeight-len(petLines))
	lines := sidebarLines(skills, todos, findings, projectNote, files, filesExpanded, variables, flows, contentWidth)
	if len(lines) > listHeight {
		start := min(max(0, scroll), len(lines)-listHeight)
		end := start + listHeight
		lines = lines[start:end]
	}
	contentLines := append(padSidebarLines(lines, listHeight, contentWidth), petLines...)
	content := strings.Join(padSidebarLines(contentLines, contentHeight, contentWidth), "\n")
	borderColor := sidebarBackground
	if focused {
		borderColor = lipgloss.Color("86")
	}
	return renderSidebarFrame(content, width, borderColor)
}

func sidebarInnerWidth(width int) int {
	return max(1, width-6)
}

func padSidebarLines(lines []string, height, width int) []string {
	for len(lines) < height {
		lines = append(lines, renderSidebarLine(lipgloss.NewStyle(), "", width))
	}
	return lines
}

func renderSidebarFrame(content string, width int, borderColor lipgloss.Color) string {
	innerWidth := max(1, width-2)
	borderStyle := lipgloss.NewStyle().Foreground(borderColor)
	backgroundStyle := lipgloss.NewStyle().Background(sidebarBackground)
	top := borderStyle.Render("+" + repeatToWidth("-", innerWidth) + "+")
	bottom := borderStyle.Render("+" + repeatToWidth("-", innerWidth) + "+")
	blank := borderStyle.Render("|") + backgroundStyle.Render(strings.Repeat(" ", innerWidth)) + borderStyle.Render("|")
	var out []string
	out = append(out, top, blank)
	for _, line := range strings.Split(content, "\n") {
		out = append(out, borderStyle.Render("|")+backgroundStyle.Render("  ")+line+backgroundStyle.Render("  ")+borderStyle.Render("|"))
	}
	out = append(out, blank, bottom)
	return strings.Join(out, "\n")
}

func renderSidebarLine(style lipgloss.Style, text string, width int) string {
	return style.Background(sidebarBackground).Render(padToWidth(fitLine(text, width), width))
}

func renderPetAreaLines(pet petRenderContext, width int) []string {
	petLines := renderPetLines(pet, width)
	separator := repeatToWidth("-", width)
	lines := []string{renderSidebarLine(styleMuted, separator, width)}
	lines = append(lines, petLines...)
	return lines
}

func renderPetLines(pet petRenderContext, width int) []string {
	art, label := petArt(pet)
	lines := make([]string, 0, len(art)+2)
	lines = append(lines, renderSidebarLine(stylePetTitle, "你的宠物", width))
	for _, line := range centerPetBlock(art, width) {
		lines = append(lines, renderSidebarLine(stylePet, line, width))
	}
	lines = append(lines, renderSidebarLine(stylePetStatus, centerLine(label, width), width))
	return lines
}

func petArt(pet petRenderContext) ([]string, string) {
	phase := pet.Frame % 8
	state := pet.State
	variant := pet.Variant
	action := pet.Action
	labelOverride := strings.TrimSpace(pet.Label)
	sprite := func(face, bubble, badge string) []string { return catSprite(face, bubble, badge, variant) }
	writeBadge := action
	if writeBadge == "" {
		writeBadge = "pen"
	}
	toolBadge := action
	if toolBadge == "" {
		toolBadge = "tool"
	}
	switch state {
	case petThinking:
		frames := [][]string{
			sprite("'-'", "?", ""), sprite("'-'", "??", ""), sprite("'-'", "...", ""), sprite("'-'", "*?", ""),
			sprite("'-'", "?", ""), sprite("'-'", "??", ""), sprite("- -", "...", ""), sprite("'-'", "*?", ""),
		}
		return frames[phase], firstNonEmptyString(labelOverride, "思考中")
	case petPlanning:
		return catPlanSprite(action, variant, phase), firstNonEmptyString(labelOverride, "规划中")
	case petWriting:
		frames := [][]string{
			sprite("o.o", "", " "+writeBadge), sprite("o.o", "", " /"+writeBadge), sprite("o.o", "", " "+writeBadge), sprite("- -", "", " /"+writeBadge),
			sprite("o.o", "", " "+writeBadge), sprite("o.o", "", " /"+writeBadge), sprite("o.o", "", " "+writeBadge), sprite("- -", "", " /"+writeBadge),
		}
		return frames[phase], firstNonEmptyString(labelOverride, "写字中")
	case petTool:
		art, label := petToolArt(toolBadge, variant, phase)
		return art, firstNonEmptyString(labelOverride, label)
	case petDone:
		frames := [][]string{
			sprite("^o^", "", " ~"), sprite("^.^", "", "  ~"), sprite("^o^", "", " ~~"), sprite("^.^", "", "  ~"),
			sprite("^o^", "", " ~"), sprite("^.^", "", "  ~"), sprite("- -", "", " ~~"), sprite("^.^", "", "  ~"),
		}
		return frames[phase], firstNonEmptyString(labelOverride, "完成啦")
	case petStopped:
		frames := [][]string{
			sprite("- -", "z", ""), sprite("- -", "zz", ""), sprite("- -", "zzz", ""), sprite("- -", "zz", ""),
			sprite("- -", "z", ""), sprite("- -", "zz", ""), sprite("- -", "zzz", ""), sprite("- -", "zz", ""),
		}
		return frames[phase], firstNonEmptyString(labelOverride, "打盹中")
	default:
		if phase == 6 {
			return sprite("- -", "", ""), firstNonEmptyString(labelOverride, "待命中")
		}
		return sprite("'-'", "", ""), firstNonEmptyString(labelOverride, "待命中")
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func catSprite(face, bubble, badge, variant string) []string {
	suffix := ""
	if bubble != "" {
		suffix = "  " + bubble
	}
	if strings.TrimSpace(badge) != "" {
		suffix += badge
	}
	suffix = truncateASCII(suffix, 14)
	head := catHeadLines(face, suffix, variant)
	switch variant {
	case petVariantAlert:
		return append(head, ` /| \\  /`, ` V-V(_m`)
	case petVariantDetective:
		return append(head, ` /|  \ /`, ` U-U(_/`)
	}
	return append(head, ` |   \ /`, ` U-U(_/`)
}

func petToolArt(action, variant string, phase int) ([]string, string) {
	switch action {
	case "read":
		return catReadFileSprite(variant, phase), "读文件"
	case "compress":
		return catCompressSprite(variant, phase), "压缩中"
	case "grep":
		return catSearchSprite(variant, phase), "搜索中"
	case "verify", "check":
		return catVerifyFindingSprite(variant, phase), "复核漏洞"
	case "report", "bug":
		return catReportFindingSprite(variant, phase), "提交漏洞"
	}
	return catGenericToolSprite(action, variant, phase), "调工具"
}

func catPlanSprite(action, variant string, phase int) []string {
	prop := "map"
	switch action {
	case "todo":
		prop = "todo"
	case "read":
		prop = "cfg"
	case "grep":
		prop = "idx"
	case "done":
		prop = "go!"
	case "plan":
		prop = "plan"
	}
	frames := [][]string{
		append(catHeadLines("o.o", "", variant), ` /| [map]`, ` U-U(_/`),
		append(catHeadLines("o.o", "", variant), ` [ ]-[ ]`, ` U-U(_/`),
		append(catHeadLines("- -", "", variant), ` /| {`+prop+`}`, ` U-U(_/`),
		append(catHeadLines("o.o", "", variant), ` [x]->[ ]`, ` U-U(_/`),
	}
	return frames[phase%len(frames)]
}

func catHeadLines(face, suffix, variant string) []string {
	switch variant {
	case petVariantAlert:
		return []string{` /^^\` + suffix, fmt.Sprintf(`(%s )!`, face)}
	case petVariantDetective:
		return []string{`  ___` + suffix, ` /-/\`, fmt.Sprintf(`(%s )o`, face)}
	default:
		return []string{` /-/\` + suffix, fmt.Sprintf(`(%s )`, face)}
	}
}

func catReadFileSprite(variant string, phase int) []string {
	frames := [][]string{
		append(catHeadLines("o.o", "", variant), ` /| [file]`, ` U-U(_/`),
		append(catHeadLines("o.o", "", variant), `[file] |\`, ` U-U(_/`),
		append(catHeadLines("- -", "", variant), ` /| [file]`, ` U-U(_/`),
		append(catHeadLines("o.o", "", variant), `[file] |\`, ` U-U(_/`),
	}
	return frames[phase%len(frames)]
}

func catCompressSprite(variant string, phase int) []string {
	frames := [][]string{
		append(catHeadLines(">.<", "", variant), ` /| {ctx}`, ` U-U(_/`),
		append(catHeadLines(">.<", "", variant), `{ctx} |\`, ` U-U(_/`),
		append(catHeadLines("- -", "", variant), ` /| [zip]`, ` U-U(_/`),
		append(catHeadLines(">.<", "", variant), `[zip] |\`, ` U-U(_/`),
	}
	return frames[phase%len(frames)]
}

func catSearchSprite(variant string, phase int) []string {
	frames := [][]string{
		append(catHeadLines("o.o", "", variant), ` /|  o-`, ` U-U(_/`),
		append(catHeadLines("o.o", "", variant), ` o-  |\`, ` U-U(_/`),
		append(catHeadLines("- -", "", variant), ` /|  o-`, ` U-U(_/`),
		append(catHeadLines("o.o", "", variant), ` o-  |\`, ` U-U(_/`),
	}
	return frames[phase%len(frames)]
}

func catVerifyFindingSprite(variant string, phase int) []string {
	frames := [][]string{
		append(catHeadLines("o.o", "", variant), ` /| [ok?]`, ` U-U(_/`),
		append(catHeadLines("o.o", "", variant), `[ok?] |\`, ` U-U(_/`),
		append(catHeadLines("- -", "", variant), ` /| [yes]`, ` U-U(_/`),
		append(catHeadLines("o.o", "", variant), `[yes] |\`, ` U-U(_/`),
	}
	return frames[phase%len(frames)]
}

func catReportFindingSprite(variant string, phase int) []string {
	frames := [][]string{
		append(catHeadLines("^o^", "", variant), ` /| [BUG]`, ` U-U(_/`),
		append(catHeadLines("^.^", "", variant), `[BUG] |\`, ` U-U(_/`),
		append(catHeadLines("- -", "", variant), ` /| [!!!]`, ` U-U(_/`),
		append(catHeadLines("^.^", "", variant), `[!!!] |\`, ` U-U(_/`),
	}
	return frames[phase%len(frames)]
}

func catGenericToolSprite(action, variant string, phase int) []string {
	if action == "" {
		action = "tool"
	}
	prop := truncateASCII(action, 6)
	frames := [][]string{
		append(catHeadLines(">.<", "", variant), ` /| [`+prop+`]`, ` U-U(_/`),
		append(catHeadLines(">.<", "", variant), `[`+prop+`] |\`, ` U-U(_/`),
		append(catHeadLines("- -", "", variant), ` /| {`+prop+`}`, ` U-U(_/`),
		append(catHeadLines(">.<", "", variant), `{`+prop+`} |\`, ` U-U(_/`),
	}
	return frames[phase%len(frames)]
}

func centerLine(text string, width int) string {
	text = fitLine(text, width)
	padding := width - runewidth.StringWidth(text)
	if padding <= 0 {
		return text
	}
	left := padding / 2
	return strings.Repeat(" ", left) + text
}

func centerPetBlock(lines []string, width int) []string {
	minIndent := -1
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " ")
		if trimmed == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if minIndent < 0 || indent < minIndent {
			minIndent = indent
		}
	}
	if minIndent < 0 {
		minIndent = 0
	}
	blockWidth := 0
	for _, line := range lines {
		if len(line) >= minIndent {
			line = line[minIndent:]
		}
		line = strings.TrimRight(line, " ")
		lineWidth := runewidth.StringWidth(line)
		if lineWidth > blockWidth {
			blockWidth = lineWidth
		}
	}
	left := max(0, (width-blockWidth)/2)
	out := make([]string, len(lines))
	for i, line := range lines {
		if len(line) >= minIndent {
			line = line[minIndent:]
		}
		line = strings.TrimRight(line, " ")
		out[i] = fitLine(strings.Repeat(" ", left)+line, width)
	}
	return out
}

func sidebarLines(skills []string, todos []tools.Todo, findings []tools.Finding, projectNote tools.ProjectNote, files []tools.FileReview, filesExpanded bool, variables []tools.VariableReview, flows []tools.FlowReview, width int) []string {
	var lines []string
	appendLine := func(text string) {
		lines = append(lines, renderSidebarLine(lipgloss.NewStyle(), text, width))
	}
	appendStyledLine := func(style lipgloss.Style, text string) {
		lines = append(lines, renderSidebarLine(style, text, width))
	}
	appendWrapped := func(style lipgloss.Style, text string) {
		for _, line := range wrapLine(text, width) {
			appendStyledLine(style, line)
		}
	}
	appendStyledLine(styleSidebarTitle, "已加载技能")
	if len(skills) == 0 {
		appendStyledLine(styleMuted, "暂无已加载 skill")
	} else {
		for _, skill := range skills {
			appendWrapped(styleSuccess, skill)
		}
	}
	appendLine("")

	appendStyledLine(styleSidebarTitle, "项目笔记")
	if strings.TrimSpace(projectNote.Note) == "" {
		appendStyledLine(styleMuted, "暂无 project note")
	} else {
		for _, line := range projectNotePreviewLines(projectNote.Note, width, maxSidebarProjectNoteLines) {
			appendStyledLine(styleMuted, line)
		}
	}
	appendLine("")

	appendStyledLine(styleSidebarTitle, "Todo")
	if len(todos) == 0 {
		appendStyledLine(styleMuted, "暂无 todo")
	} else {
		for _, todo := range todos {
			mark := "[ ]"
			if todo.Status == "completed" || todo.Status == "done" {
				mark = "[x]"
			}
			line := fmt.Sprintf("%s #%d %s", mark, todo.ID, todo.Title)
			appendWrapped(statusStyle(todo.Status), line)
			appendStyledLine(styleMuted, todo.Status+" / "+todo.Priority)
		}
	}
	appendLine("")
	appendStyledLine(styleSidebarTitle, "文件排查")
	fileCounts := map[string]int{}
	for _, file := range files {
		fileCounts[file.Status]++
	}
	appendStyledLine(styleMuted, fmt.Sprintf("未看%d 正在%d 已看%d 跳过%d", fileCounts["unseen"], fileCounts["reviewing"], fileCounts["reviewed"], fileCounts["skipped"]))
	visibleFiles := files
	if !filesExpanded && len(files) > maxSidebarFilesCollapsed {
		visibleFiles = files[:maxSidebarFilesCollapsed]
	}
	for _, file := range visibleFiles {
		appendWrapped(statusStyle(file.Status), fmt.Sprintf("[%s] %s", file.Status, file.Path))
		if file.Note != "" {
			appendWrapped(styleMuted, file.Note)
		}
	}
	if !filesExpanded && len(files) > len(visibleFiles) {
		appendStyledLine(styleMuted, fmt.Sprintf("...还有 %d 个文件，输入 files 展开", len(files)-len(visibleFiles)))
	} else if filesExpanded && len(files) > maxSidebarFilesCollapsed {
		appendStyledLine(styleMuted, "已展开全部文件，输入 files 收起")
	}
	appendLine("")
	appendStyledLine(styleSidebarTitle, "变量排查")
	if len(variables) == 0 {
		appendStyledLine(styleMuted, "暂无变量记录")
	} else {
		for i, variable := range variables {
			if i >= 8 {
				break
			}
			appendWrapped(statusStyle(variable.Status), fmt.Sprintf("[%s] %s", variable.Status, variable.Name))
			if variable.Note != "" {
				appendWrapped(styleMuted, variable.Note)
			}
		}
	}
	appendLine("")
	appendStyledLine(styleSidebarTitle, "跨文件链路")
	if len(flows) == 0 {
		appendStyledLine(styleMuted, "暂无 flow 记录")
	} else {
		for _, flow := range flows {
			appendWrapped(statusStyle(flow.Status), fmt.Sprintf("[%s] %s", flow.Status, flow.Name))
			if flow.Entry != "" {
				appendWrapped(styleMuted, "入口: "+flow.Entry)
			}
			if len(flow.Files) > 0 {
				appendWrapped(styleMuted, "文件链: "+strings.Join(flow.Files, " -> "))
			}
			if flow.NextStep != "" {
				appendWrapped(styleMuted, "下一步: "+flow.NextStep)
			}
		}
	}
	appendLine("")
	appendStyledLine(styleSidebarTitle, "漏洞")
	if len(findings) == 0 {
		appendStyledLine(styleMuted, "暂无已提交漏洞")
	} else {
		for _, finding := range findings {
			appendWrapped(severityStyle(finding.Severity), fmt.Sprintf("#%d [%s] %s", finding.ID, finding.Severity, finding.Title))
			appendWrapped(styleMuted, finding.Path)
		}
	}
	return lines
}

func sidebarContentHeight(height int) int {
	if height < 10 {
		return 7
	}
	return height - 4
}

func boxStyle(width int, color lipgloss.Color) lipgloss.Style {
	contentWidth := max(1, width-4)
	return lipgloss.NewStyle().Width(contentWidth).MaxWidth(contentWidth).Border(lipgloss.RoundedBorder()).BorderForeground(color).Padding(0, 1)
}

func renderBox(content string, width int) string {
	innerWidth := max(8, width-4)
	lines := strings.Split(wrapBlock(content, innerWidth), "\n")
	var b strings.Builder
	b.WriteString("+")
	b.WriteString(strings.Repeat("-", innerWidth+2))
	b.WriteString("+\n")
	for _, line := range lines {
		b.WriteString("| ")
		b.WriteString(padToWidth(line, innerWidth))
		b.WriteString(" |\n")
	}
	b.WriteString("+")
	b.WriteString(strings.Repeat("-", innerWidth+2))
	b.WriteString("+")
	return b.String()
}

func repeatToWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	var b strings.Builder
	for runewidth.StringWidth(b.String()) < width {
		b.WriteString(s)
	}
	return fitLine(b.String(), width)
}

func padToWidth(text string, width int) string {
	text = fitLine(text, width)
	padding := width - runewidth.StringWidth(text)
	if padding <= 0 {
		return text
	}
	return text + strings.Repeat(" ", padding)
}

func padStyledToWidth(text string, width int) string {
	if width <= 0 {
		return text
	}
	visible := stripANSI(text)
	if runewidth.StringWidth(visible) > width {
		return fitLine(visible, width)
	}
	padding := width - runewidth.StringWidth(visible)
	if padding <= 0 {
		return text
	}
	return text + strings.Repeat(" ", padding)
}

func statusStyle(status string) lipgloss.Style {
	switch strings.ToLower(status) {
	case "reviewing", "tracking", "suspicious", "pending":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	case "reviewed", "completed", "done", "benign":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("120"))
	case "skipped", "unseen":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	case "confirmed", "critical", "high":
		if strings.ToLower(status) == "critical" {
			return lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
		}
		return lipgloss.NewStyle().Foreground(lipgloss.Color("167")).Bold(true)
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	}
}

func severityStyle(severity string) lipgloss.Style {
	switch strings.ToLower(severity) {
	case "critical":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	case "high":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("167")).Bold(true)
	case "medium":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	case "low", "info":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	}
}

func wrapBlock(text string, width int) string {
	if width <= 0 {
		return stripANSI(text)
	}
	text = stripANSI(text)
	lines := strings.Split(text, "\n")
	var wrapped []string
	for _, line := range lines {
		wrapped = append(wrapped, wrapLine(line, width)...)
	}
	return strings.Join(wrapped, "\n")
}

func cropBlock(text string, height int) string {
	if height <= 0 {
		return ""
	}
	lines := strings.Split(text, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(padLines(lines, height), "\n")
}

func fixedHeightBlock(text string, width, height int) string {
	return strings.Join(fixedWidthLines(text, width, height), "\n")
}

func fixedWidthLines(text string, width, height int) []string {
	lines := strings.Split(text, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	lines = padLines(lines, height)
	for i := range lines {
		lines[i] = padStyledToWidth(lines[i], width)
	}
	return lines
}

func renderColumns(left, right string, leftWidth, rightWidth, height int) string {
	leftLines := fixedRenderedLines(left, leftWidth, height)
	rightLines := fixedWidthLines(right, rightWidth, height)
	if len(leftLines) < height {
		leftLines = padLines(leftLines, height)
	}
	if len(rightLines) < height {
		rightLines = padLines(rightLines, height)
	}
	var out []string
	for i := 0; i < height; i++ {
		out = append(out, leftLines[i]+"  "+rightLines[i])
	}
	return strings.Join(out, "\n")
}

func fixedRenderedLines(text string, width, height int) []string {
	lines := strings.Split(text, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	lines = padLines(lines, height)
	for i := range lines {
		lines[i] = fitStyledLine(lines[i], width)
	}
	return lines
}

func fitStyledLine(text string, width int) string {
	if width <= 0 {
		return ""
	}
	if runewidth.StringWidth(stripANSI(text)) <= width {
		return text
	}
	var b strings.Builder
	used := 0
	for i := 0; i < len(text); {
		if text[i] == '\x1b' {
			end := ansiSequenceEnd(text, i)
			if end <= i {
				break
			}
			b.WriteString(text[i:end])
			i = end
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == utf8.RuneError && size == 0 {
			break
		}
		runeWidth := runewidth.RuneWidth(r)
		if used+runeWidth > width {
			break
		}
		b.WriteRune(r)
		used += runeWidth
		i += size
	}
	b.WriteString("\x1b[0m")
	return b.String()
}

func ansiSequenceEnd(text string, start int) int {
	if start+1 >= len(text) || text[start] != '\x1b' || text[start+1] != '[' {
		return start + 1
	}
	for i := start + 2; i < len(text); i++ {
		if text[i] >= '@' && text[i] <= '~' {
			return i + 1
		}
	}
	return len(text)
}

func padLines(lines []string, height int) []string {
	for len(lines) < height {
		lines = append(lines, "")
	}
	return lines
}

func fitLine(text string, width int) string {
	if width <= 0 {
		return stripANSI(text)
	}
	text = stripANSI(text)
	if runewidth.StringWidth(text) <= width {
		return text
	}
	var b strings.Builder
	used := 0
	for _, r := range text {
		rw := runewidth.RuneWidth(r)
		if used+rw > width {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String()
}

func wrapLine(text string, width int) []string {
	if width <= 0 {
		return []string{stripANSI(text)}
	}
	text = stripANSI(text)
	if text == "" {
		return []string{""}
	}
	var lines []string
	var b strings.Builder
	used := 0
	for _, r := range text {
		rw := runewidth.RuneWidth(r)
		if used > 0 && used+rw > width {
			lines = append(lines, b.String())
			b.Reset()
			used = 0
		}
		b.WriteRune(r)
		used += rw
	}
	if b.Len() > 0 {
		lines = append(lines, b.String())
	}
	return lines
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func stripANSI(text string) string {
	return ansiRe.ReplaceAllString(text, "")
}

var (
	sidebarBackground = lipgloss.Color("#141414")

	styleTitle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86"))
	styleStatus       = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleUser         = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	styleAssistant    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("120"))
	styleThink        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("244"))
	styleTool         = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleToolName     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	styleError        = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styleSuccess      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("120"))
	styleInput        = lipgloss.NewStyle().MarginTop(1)
	styleHelp         = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleSidebarTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86"))
	stylePetTitle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("229"))
	stylePet          = lipgloss.NewStyle().Foreground(lipgloss.Color("229"))
	stylePetStatus    = lipgloss.NewStyle().Foreground(lipgloss.Color("180"))
	styleMuted        = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
