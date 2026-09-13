package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/reflow/truncate"
)

const maxBroadcastBytes = 16 * 1024

type modalState struct {
	title        string
	lines        []string
	wrapped      []string
	wrapWidth    int
	scroll       int
	compose      bool
	editor       textarea.Model
	err          string
	findings     *findingsModal
	sessions     *sessionsModal
	agents       bool
	budget       bool
	budgetForm   *budgetForm
	agentIndex   int
	agentDetails bool
	agentStarts  []int
	agentState   *agentsModal
}

type sessionsModal struct {
	items    []string
	selected int
	starts   []int
}

func (m *Model) openBudgetPrompt() tea.Cmd {
	if m.busy || m.saving {
		m.pendingDir = ""
		m.addEvent("请先停止审计并等待保存完成，再修改预算。")
		return nil
	}
	m.budgetCfg = m.runner.BudgetStatus()
	m.modal = &modalState{title: "审计预算 · /budget", budget: true, budgetForm: newBudgetForm(m.budgetCfg)}
	m.input.Blur()
	m.resizeModal()
	return nil
}

func (m *Model) openBroadcast(value string) tea.Cmd {
	editor := textarea.New()
	editor.Prompt = ""
	editor.Placeholder = "Message to all agents..."
	editor.ShowLineNumbers = false
	editor.EndOfBufferCharacter = ' '
	editor.CharLimit = maxBroadcastBytes
	editor.MaxHeight = 0
	state := &modalState{title: "广播草稿 /say", compose: true, editor: editor}
	if len(value) > maxBroadcastBytes {
		end := maxBroadcastBytes
		for end > 0 && !utf8.RuneStart(value[end]) {
			end--
		}
		value = value[:end]
		state.err = "草稿超过 16 KiB；已截至上限，请检查后再发送。"
	}
	state.editor.SetValue(value)
	m.modal = state
	m.input.Blur()
	m.resizeModal()
	return m.modal.editor.Focus()
}

func (m *Model) closeModal() tea.Cmd {
	m.modal = nil
	return m.input.Focus()
}

func (m Model) modalSize() (width, height, bodyHeight int) {
	width = min(96, max(4, m.width-4))
	height = min(24, max(4, m.height-2))
	return width, height, max(1, height-5)
}

func (m *Model) resizeModal() {
	if m.modal == nil {
		return
	}
	width, _, bodyHeight := m.modalSize()
	inner := max(1, width-4)
	if m.modal.budget {
		for i := range m.modal.budgetForm.fields {
			m.modal.budgetForm.fields[i].Width = max(1, inner-4)
		}
		return
	}
	if m.modal.compose {
		m.modal.editor.SetWidth(inner)
		m.modal.editor.SetHeight(bodyHeight)
		return
	}
	if s := m.modal.sessions; s != nil {
		m.modal.wrapped = nil
		s.starts = s.starts[:0]
		for i, item := range s.items {
			s.starts = append(s.starts, len(m.modal.wrapped))
			marker := "  "
			if i == s.selected {
				marker = "> "
			}
			m.modal.wrapped = append(m.modal.wrapped, wrapLines(marker+item, inner)...)
		}
		if len(s.items) == 0 {
			m.modal.wrapped = wrapLines("没有已保存的会话", inner)
		} else {
			row := s.starts[s.selected]
			if row < m.modal.scroll || row >= m.modal.scroll+bodyHeight {
				m.modal.scroll = row
			}
		}
		m.modal.wrapWidth = inner
		return
	}
	m.wrapModal(inner)
	m.modal.scroll = min(m.modal.scroll, max(0, len(m.modal.wrapped)-bodyHeight))
	if m.modal.findings != nil && !m.modal.findings.detail {
		m.modal.keepFindingVisible(bodyHeight)
	}
	if m.modal.agents && !m.modal.agentDetails {
		m.modal.keepAgentVisible(bodyHeight)
	}
}

func (m Model) wrapModal(width int) {
	if m.modal.agents {
		m.modal.wrapAgents(width)
		return
	}
	if m.modal.findings != nil && !m.modal.findings.detail {
		m.modal.wrapFindings(width)
		return
	}
	if m.modal.wrapWidth == width {
		return
	}
	m.modal.wrapped = nil
	for _, line := range m.modal.lines {
		m.modal.wrapped = append(m.modal.wrapped, wrapLines(line, width)...)
	}
	m.modal.wrapWidth = width
}

func (m *Model) updateModal(msg tea.Msg) tea.Cmd {
	state := m.modal
	if state.budget {
		return m.updateBudget(msg)
	}
	if key, ok := msg.(tea.KeyMsg); ok && !state.compose {
		if converted, consumed := m.bottomShortcut(key, true); consumed {
			if converted.Type != tea.KeyEnd {
				return nil
			}
			msg = converted
		}
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "esc", "ctrl+[":
			if state.agents && state.agentDetails {
				m.returnToAgents()
				return nil
			}
			if state.findings != nil && state.findings.detail {
				m.returnToFindings()
				return nil
			}
			m.pendingDir = ""
			return m.closeModal()
		case "ctrl+c":
			cmd, _ := m.handleKey(key)
			return cmd
		case "ctrl+s":
			if state.compose {
				value := state.editor.Value()
				if len(value) > maxBroadcastBytes || strings.TrimSpace(value) == "" {
					state.err = "消息必须非空且不超过16 KiB"
					return nil
				}
				if err := m.runner.PostMessage(value); err != nil {
					state.err = err.Error()
					return nil
				}
				m.addAgentEvent("user", "用户广播了："+value+"（完整正文将在各 Agent 下一轮优先读取）")
				m.refreshForumPage()
				return m.closeModal()
			}
		}
	}
	if state.compose {
		if _, mouse := msg.(tea.MouseMsg); mouse {
			return nil
		}
		before := state.editor.Value()
		var cmd tea.Cmd
		state.editor, cmd = state.editor.Update(msg)
		if len(state.editor.Value()) > maxBroadcastBytes {
			state.editor.SetValue(before)
			state.err = "输入超过16 KiB"
		} else if state.editor.Value() != before {
			state.err = ""
		}
		return cmd
	}
	if state.agents {
		return m.updateAgents(msg)
	}
	if state.sessions != nil {
		return m.updateSessions(msg)
	}
	if state.findings != nil && !state.findings.detail {
		return m.updateFindings(msg)
	}
	_, _, available := m.modalSize()
	delta := 0
	switch event := msg.(type) {
	case tea.KeyMsg:
		switch event.String() {
		case "up":
			delta = -1
		case "down":
			delta = 1
		case "pgup":
			delta = -available
		case "pgdown":
			delta = available
		case "home":
			state.scroll = 0
		case "end":
			state.scroll = max(0, len(state.wrapped)-available)
		}
	case tea.MouseMsg:
		switch event.Type {
		case tea.MouseWheelUp:
			delta = -3

		case tea.MouseWheelDown:
			delta = 3
		}
	}
	state.scroll = min(max(0, state.scroll+delta), max(0, len(state.wrapped)-available))
	return nil
}

func (m *Model) updateSessions(msg tea.Msg) tea.Cmd {
	s := m.modal.sessions
	if len(s.items) == 0 {
		return nil
	}
	_, _, height := m.modalSize()
	switch key := msg.(type) {
	case tea.KeyMsg:
		switch key.String() {
		case "up":
			s.selected--
		case "down":
			s.selected++
		case "pgup":
			s.selected -= height
		case "pgdown":
			s.selected += height
		case "home":
			s.selected = 0
		case "end":
			s.selected = len(s.items) - 1
		case "enter":
			if m.busy || m.saving {
				m.modal.err = "请先暂停审计并等待保存完成"
				return nil
			}
			m.restoreSession(s.items[s.selected])
			if m.modal == nil {
				return m.input.Focus()
			}
			return nil
		}
	case tea.MouseMsg:
		if key.Type == tea.MouseWheelUp {
			s.selected--
		}
		if key.Type == tea.MouseWheelDown {
			s.selected++
		}
	}
	s.selected = min(max(0, s.selected), len(s.items)-1)
	m.modal.wrapWidth = 0
	m.resizeModal()
	return nil
}

// Slice terminal cells, replacing a wide glyph intersected by an edge with spaces.
func modalCells(text string, start, width int) string {
	if width <= 0 {
		return ""
	}
	var out strings.Builder
	position, used := 0, 0
	for _, r := range text {
		size := runewidth.RuneWidth(r)
		end := position + size
		if position >= start+width {
			break
		}
		if end > start {
			if position < start || end > start+width {
				n := min(end, start+width) - max(position, start)
				out.WriteString(strings.Repeat(" ", n))
				used += n
			} else {
				out.WriteRune(r)
				used += size
			}
		}
		position = end
	}
	out.WriteString(strings.Repeat(" ", max(0, width-used)))
	return out.String()
}

func (m Model) overlayModal(frame string) string {
	if m.modal == nil {
		return frame
	}
	width, height, bodyHeight := m.modalSize()
	if m.width < 20 || m.height < 8 {
		dismiss := "请扩大终端；Esc 关闭"
		if m.modal.findings != nil && m.modal.findings.detail {
			dismiss = "请扩大终端；Esc 返回列表"
		}
		rows := []string{fitLine(m.modal.title, m.width), fitLine(dismiss, m.width)}
		if m.modal.compose {
			rows = append(rows, fitLine("Ctrl+S 发送 / Esc 取消", m.width))
		}
		return strings.Join(rows[:min(len(rows), max(1, m.height))], "\n")
	}
	inner := width - 4
	var content []string
	status := ""
	footer := "↑↓ 滚动 · PgUp/PgDn 翻页 · gg / End 到底 · Esc 关闭"
	if m.modal.budget {
		content = m.budgetLines(inner, bodyHeight)
		status = "时间与 token 任一用尽即停；修改预算不重置已用量"
		if !m.modal.budgetForm.infinite {
			status = "普通模式不限额；下列预算仅在无限审计模式生效"
		}
		if m.modal.err != "" {
			status = "! " + m.modal.err
		}
		footer = "Tab 切换 · ←→ 模式 · Ctrl+S 确认 · Esc 取消"
	} else if m.modal.compose {
		content = strings.Split(m.modal.editor.View(), "\n")
		status = fmt.Sprintf("%d / %d bytes", len(m.modal.editor.Value()), maxBroadcastBytes)
		if m.modal.err != "" {
			status = m.modal.err
		}
		footer = "Ctrl+S 发送 · Enter 换行 · Esc 取消"
	} else {
		m.wrapModal(inner)
		content = viewport(m.modal.wrapped, m.modal.scroll, bodyHeight, false)
		status = fmt.Sprintf("行 %d–%d / %d", min(len(m.modal.wrapped), m.modal.scroll+1), min(len(m.modal.wrapped), m.modal.scroll+bodyHeight), len(m.modal.wrapped))
		if f := m.modal.findings; f != nil {
			if f.detail {
				footer = "↑↓ 滚动 · PgUp/PgDn 翻页 · gg / End 到底 · Esc 返回列表"
			} else {
				status = fmt.Sprintf("选中 %d / %d · 本次快照", min(len(f.items), f.selected+1), len(f.items))
				footer = "↑↓ 选择 · gg / End 末项 · Enter 详情 · Esc 关闭"
			}
		}
		if m.modal.agents {
			footer = "↑↓ 选择 · gg / End 末项 · Enter 诊断 · Esc 关闭"
			if m.modal.agentDetails {
				footer = "↑↓ 滚动 · gg / End 到底 · Esc 返回列表"
			}
		}
	}
	if inner < 60 {
		switch {
		case m.modal.budget:
			footer = "Tab切换 · ^S保存 · Esc取消"
		case m.modal.compose:
			footer = "^S发送 · Esc取消"
		case m.modal.agents && !m.modal.agentDetails, m.modal.findings != nil && !m.modal.findings.detail, m.modal.sessions != nil:
			footer = "↑↓选 · Enter打开 · Esc关闭"
		default:
			footer = "↑↓滚动 · gg到底 · Esc返回"
		}
	}
	rows := make([]string, height)
	rows[0] = paintBorder("╭" + strings.Repeat("─", width-2) + "╮")
	rows[height-1] = paintBorder("╰" + strings.Repeat("─", width-2) + "╯")
	for row := 1; row < height-1; row++ {
		text := ""
		switch {
		case row == 1:
			text = fitLine(m.modal.title, inner)
		case row == height-3:
			text = fitLine(status, inner)
		case row == height-2:
			text = fitLine(footer, inner)
		case row-2 < len(content):
			text = content[row-2]
		}
		// Preserve the textarea cursor's ANSI styling; all external body text
		// has already passed through wrapLines/safeText.
		text = truncate.String(text, uint(inner))
		switch {
		case row == 1:
			text = paintAccent(text)
		case row == height-3 && m.modal.err != "":
			text = paintLine("! " + fitLine(m.modal.err, max(1, inner-2)))
		case row == height-3 || row == height-2:
			text = paintMuted(text)
		case !m.modal.compose && !m.modal.budget:
			text = paintLine(text)
		}
		padding := strings.Repeat(" ", max(0, inner-runewidth.StringWidth(ansiEscape.ReplaceAllString(text, ""))))
		rows[row] = paintBorder("│") + " " + text + padding + " " + paintBorder("│")
	}
	background := strings.Split(safeText(frame), "\n")
	for len(background) < m.height {
		background = append(background, "")
	}
	background = background[:m.height]
	x, y := (m.width-width)/2, (m.height-height)/2
	for row, line := range rows {
		base := background[y+row]
		background[y+row] = paintMuted(modalCells(base, 0, x)) + line + paintMuted(modalCells(base, x+width, m.width-x-width))
	}
	return strings.Join(background, "\n")
}
