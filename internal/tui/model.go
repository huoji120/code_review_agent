package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"code-review-agent/internal/agent"
	"code-review-agent/internal/config"
	"code-review-agent/internal/forum"
	"code-review-agent/internal/tools"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

const maxUIEvents = 120

var terminalWidthOnce sync.Once

// Model retains worker status and recent operations, never model streams.
type Model struct {
	runner                                                        *agent.Team
	input                                                         textinput.Model
	width, height                                                 int
	autoDir, sessionDir, sessionPath, workspace, lastQuery        string
	workspaceReady, busy, stopping, quitting, saving, escapeArmed bool
	cancel                                                        context.CancelFunc
	session                                                       int
	mailbox                                                       *eventMailbox
	phase                                                         string
	workers                                                       []agent.WorkerStatus
	forumPage                                                     forum.PostPage
	forumSearch                                                   string
	forumDirty                                                    bool
	forumCache                                                    *forumRenderCache
	forumRevision                                                 uint64
	threadID                                                      int64
	selectedPost                                                  int64
	expandedPosts                                                 map[int64]bool
	snapshot                                                      tools.Snapshot
	events                                                        []operationEvent
	modal                                                         *modalState
	focus                                                         int
	scroll                                                        [2]int
	follow                                                        [2]bool
	finalSavePending                                              bool
}

type operationEvent struct {
	at, agentID, content string
}

type startAuditMsg string
type refreshMsg struct{ width, height int }
type eventBatchMsg struct {
	session int
	events  []agent.Event
	done    bool
	failure string
}
type saveDoneMsg struct {
	session int
	path    string
	err     error
}

// The coordinator must never wait for terminal rendering or disk I/O. Coalesce
// snapshots and keep a bounded recent queue; final state is read after Run returns.
type eventMailbox struct {
	mu      sync.Mutex
	queue   []agent.Event
	wake    chan struct{}
	done    bool
	failure string
}

func (b *eventMailbox) post(e agent.Event) {
	if !operationalEvent(e.Kind) {
		return
	}
	e.Content = boundedText(e.Content, 1000)
	b.mu.Lock()
	if b.done {
		b.mu.Unlock()
		return
	}
	if e.Kind == "state" || e.Kind == "model_progress" {
		for i := len(b.queue) - 1; i >= 0; i-- {
			if b.queue[i].Kind == e.Kind {
				copy(b.queue[i:], b.queue[i+1:])
				b.queue[len(b.queue)-1] = agent.Event{}
				b.queue = b.queue[:len(b.queue)-1]
				break
			}
		}
	}
	if len(b.queue) == 256 {
		copy(b.queue, b.queue[1:])
		b.queue = b.queue[:255]
	}
	b.queue = append(b.queue, e)
	select {
	case b.wake <- struct{}{}:
	default:
	}
	b.mu.Unlock()
}
func (b *eventMailbox) finish(failure string) {
	b.mu.Lock()
	if !b.done {
		b.done, b.failure = true, failure
		close(b.wake)
	}
	b.mu.Unlock()
}
func waitAgent(session int, b *eventMailbox) tea.Cmd {
	return func() tea.Msg {
		for {
			b.mu.Lock()
			if len(b.queue) > 0 || b.done {
				msg := eventBatchMsg{session: session, events: b.queue, done: b.done, failure: b.failure}
				b.queue = nil
				b.mu.Unlock()
				return msg
			}
			b.mu.Unlock()
			<-b.wake
		}
	}
}
func operationalEvent(kind string) bool {
	switch kind {
	case "state", "model_progress", "worker", "name", "forum", "tool", "waiting", "info", "error", "verify_progress", "verify_done":
		return true
	}
	return false
}

func New(runner *agent.Team, cfg config.Config, autoDir string) Model {
	terminalWidthOnce.Do(func() {
		// Modern terminals render box drawing and punctuation as single cells,
		// even when the Windows locale is CJK. Keep explicit user overrides.
		if os.Getenv("RUNEWIDTH_EASTASIAN") == "" {
			runewidth.EastAsianWidth = false
			runewidth.DefaultCondition = runewidth.NewCondition()
		}
	})
	input := textinput.New()
	input.Placeholder = "audit directory / go /say /help"
	input.Focus()
	input.CharLimit = 4000
	input.Width = 116
	// Cached wraps are used only by the serialized Bubble Tea update/render loop.
	m := Model{runner: runner, input: input, width: 120, height: 40, autoDir: autoDir, sessionDir: cfg.Agent.SessionDir, focus: 1, follow: [2]bool{true, true}}
	m.forumCache = &forumRenderCache{}
	m.syncTeam()
	m.addEvent("请输入要审计的目录；论坛只显示真实通信。")
	return m
}
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink, refreshTerminal()}
	if strings.TrimSpace(m.autoDir) != "" {
		cmds = append(cmds, func() tea.Msg { return startAuditMsg(m.autoDir) })
	}
	return tea.Batch(cmds...)
}
func refreshTerminal() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg {
		w, h, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			w, h, _ = term.GetSize(int(os.Stderr.Fd()))
		}
		return refreshMsg{w, h}
	})
}
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch x := msg.(type) {
	case refreshMsg:
		if x.width > 0 && x.height > 0 && (x.width != m.width || x.height != m.height) {
			return m, tea.Batch(refreshTerminal(), func() tea.Msg {
				return tea.WindowSizeMsg{Width: x.width, Height: x.height}
			})
		}
		return m, refreshTerminal()
	case tea.WindowSizeMsg:
		if x.Width <= 0 || x.Height <= 0 {
			return m, nil
		}
		changed := x.Width != m.width || x.Height != m.height
		m.resize(x.Width, x.Height)
		if changed {
			return m, tea.ClearScreen
		}
		return m, nil
	case startAuditMsg:
		cmd := m.startDirectory(string(x))
		return m, cmd
	case eventBatchMsg:
		if x.session != m.session || !m.busy {
			return m, nil
		}
		for _, e := range x.events {
			m.applyEvent(e)
		}
		if m.forumDirty && !x.done {
			m.refreshForumPage()
		}
		if x.done {
			// Drain the entire last batch before making resume/workspace changes legal.
			m.busy = false
			m.stopping = false
			m.mailbox = nil
			if m.cancel != nil {
				m.cancel()
				m.cancel = nil
			}
			m.syncTeam()
			if x.failure != "" {
				m.addEvent("运行异常：" + x.failure)
			} else if m.phase == "completed" {
				m.addEvent("全部审计 Agent 已完成；/report 查看摘要，/export 导出报告。")
			} else {
				m.addEvent("本轮已停止；输入 go 继续，或 /export 导出报告。")
			}
			if m.saving {
				m.finalSavePending = true
				return m, nil
			}
			cmd := m.saveSession("")
			return m, cmd
		}
		return m, waitAgent(m.session, m.mailbox)
	case saveDoneMsg:
		if x.session != m.session {
			return m, nil
		}
		m.saving = false
		if x.err != nil {
			m.addEvent("保存会话失败：" + x.err.Error())
		} else {
			m.addEvent("已保存会话：" + x.path)
		}
		if m.finalSavePending {
			m.finalSavePending = false
			cmd := m.saveSession("")
			return m, cmd
		}
		if m.quitting && !m.busy {
			return m, tea.Quit
		}
		return m, nil
	case tea.MouseMsg:
		if m.modal != nil {
			cmd := m.updateModal(x)
			return m, cmd
		}
		if x.Y >= m.height-2 {
			return m, nil
		}
		left, _, _, narrow := m.layout()
		if !narrow {
			if x.X < left {
				m.focus = 0
			} else {
				m.focus = 1
			}
		}
		switch x.Type {
		case tea.MouseWheelUp:
			m.moveScroll(-3)
		case tea.MouseWheelDown:
			m.moveScroll(3)
		case tea.MouseLeft:
			m.clickForum(x.X, x.Y)
		}
		return m, nil
	case tea.KeyMsg:
		if m.modal != nil {
			cmd := m.updateModal(x)
			return m, cmd
		}
		cmd, handled := m.handleKey(x)
		if handled {
			return m, cmd
		}
	}
	if m.modal != nil {
		cmd := m.updateModal(msg)
		return m, cmd
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}
func (m *Model) handleKey(key tea.KeyMsg) (tea.Cmd, bool) {
	if key.String() != "esc" && key.String() != "ctrl+[" {
		m.escapeArmed = false
	}
	switch key.String() {
	case "ctrl+c":
		if m.busy {
			m.quitting = true
			m.stop()
			return nil, true
		}
		if m.saving {
			m.quitting = true
			m.addEvent("等待保存完成后退出。")
			return nil, true
		}
		return tea.Quit, true
	case "esc", "ctrl+[":
		if !m.busy || m.stopping {
			m.escapeArmed = false
			return nil, true
		}
		if !m.escapeArmed {
			m.escapeArmed = true
			m.addEvent("再按一次 Esc 暂停审计；其他按键取消确认。")
			return nil, true
		}
		m.escapeArmed = false
		m.stop()
		return nil, true
	case "tab":
		m.focus = 1 - m.focus
		return nil, true
	case "up", "down":
		if m.input.Value() != "" {
			return nil, false
		}
		delta := 1
		if key.String() == "up" {
			delta = -1
		}
		if m.focus == 1 {
			m.selectForumPost(delta)
		} else {
			m.moveScroll(delta)
		}
		return nil, true
	case "left", "right":
		if m.input.Value() != "" || m.focus != 1 {
			return nil, false
		}
		delta := 1
		if key.String() == "left" {
			delta = -1
		}
		m.changeForumPage(delta)
		return nil, true
	case "pgup":
		_, _, h, _ := m.layout()
		m.moveScroll(-max(1, h-4))
		return nil, true
	case "pgdown":
		_, _, h, _ := m.layout()
		m.moveScroll(max(1, h-4))
		return nil, true
	case "home":
		if m.input.Value() != "" {
			return nil, false
		}
		m.scroll[m.focus] = 0
		m.follow[m.focus] = m.focus == 1
		return nil, true
	case "end":
		if m.input.Value() != "" {
			return nil, false
		}
		if m.focus == 1 {
			_, m.scroll[1] = m.scrollContent(1)
			m.follow[1] = false
		} else {
			m.follow[0] = true
		}
		return nil, true
	case "enter":
		if m.input.Value() == "" && m.focus == 1 {
			m.toggleForumPost(m.selectedPost)
			return nil, true
		}
		value := strings.TrimSpace(m.input.Value())
		m.input.Reset()
		if value == "" {
			return nil, true
		}
		return m.submit(value), true
	}
	return nil, false
}
func (m *Model) stop() {
	if !m.busy {
		m.addEvent("当前没有运行中的审计。")
		return
	}
	if m.stopping {
		return
	}
	m.stopping = true
	if m.cancel != nil {
		m.cancel()
	}
	m.addEvent("正在停止；等待所有 worker 退出后才能继续。")
}
func (m *Model) runQuery(query string) tea.Cmd {
	if m.busy || m.saving {
		m.addEvent("请等待当前运行或保存完成后再开始。")
		return nil
	}
	m.busy = true
	m.stopping = false
	m.session++
	m.lastQuery = query
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	box := &eventMailbox{wake: make(chan struct{}, 1)}
	m.mailbox = box
	runner := m.runner
	go func() {
		defer func() {
			failure := ""
			if r := recover(); r != nil {
				failure = fmt.Sprint(r)
			}
			box.finish(failure)
		}()
		runner.Run(ctx, query, box.post)
	}()
	m.addEvent("启动团队审计；Esc 暂停，/say 打开广播草稿，Ctrl+S 发送。")
	return waitAgent(m.session, box)
}
func (m *Model) applyEvent(e agent.Event) {
	if e.Kind == "state" && e.Phase != "" {
		m.phase = e.Phase
	}
	if e.Workers != nil {
		m.workers = e.Workers
		if m.modal != nil && m.modal.agents {
			m.modal.lines = m.agentDetailLines()
			m.modal.wrapWidth = 0
			m.resizeModal()
		}
	}
	switch e.Kind {
	case "state":
		m.snapshot = tools.Snapshot{Todos: e.Todos, Findings: e.Findings, Project: e.Project, Files: e.Files, Variables: e.Variables, Flows: e.Flows, Audit: e.Audit}
	case "forum":
		if e.Forum != nil {
			m.forumDirty = true
		}
	case "name":
		m.forumDirty = true
	case "verify_progress":
		m.addAgentEvent(e.AgentID, fmt.Sprintf("复核 %s · %d/%d", boundedText(e.VerifyTitle, 120), e.VerifyTurn, e.VerifyLimit))
	case "verify_done":
		m.addAgentEvent(e.AgentID, "复核结束")
	case "worker", "waiting", "tool", "info", "error":
		if e.Content != "" {
			m.addAgentEvent(e.AgentID, e.Content)
		}
	}
}
func (m *Model) syncTeam() {
	m.snapshot = m.runner.Snapshot()
	m.workers = m.runner.Statuses()
	m.phase = m.runner.Phase()
	m.refreshForumPage()
}
func (m *Model) addEvent(text string) {
	m.addAgentEvent("", text)
}
func (m *Model) addAgentEvent(id, text string) {
	entry := operationEvent{at: time.Now().Format("15:04:05"), agentID: id, content: boundedText(text, 1000)}
	if len(m.events) == maxUIEvents {
		copy(m.events, m.events[1:])
		m.events = m.events[:maxUIEvents-1]
	}
	m.events = append(m.events, entry)
}
