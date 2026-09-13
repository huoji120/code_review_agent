package tui

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/reflow/truncate"
)

const maxBroadcastBytes = 16 * 1024

type modalState struct {
	title     string
	lines     []string
	wrapped   []string
	wrapWidth int
	scroll    int
	compose   bool
	editor    textarea.Model
	err       string
	findings  *findingsModal
	agents    bool
	budget    bool
}

func (m *Model) openBudgetPrompt() tea.Cmd {
	if m.busy || m.saving {
		m.pendingDir = ""
		m.addEvent("请先停止审计并等待保存完成，再修改预算。")
		return nil
	}
	m.budgetCfg = m.runner.BudgetStatus()
	editor := textarea.New()
	editor.Prompt = ""
	editor.Placeholder = "normal|infinite hours minutes tokens"
	editor.ShowLineNumbers = false
	editor.EndOfBufferCharacter = ' '
	editor.MaxHeight = 0
	editor.CharLimit = 256
	mode := "normal"
	if m.budgetCfg.InfiniteMode {
		mode = "infinite"
	}
	tokens := "0"
	if m.budgetCfg.TokenLimit > 0 {
		tokens = fmt.Sprint(m.budgetCfg.TokenLimit)
	}
	editor.SetValue(fmt.Sprintf("%s %d %d %s", mode, m.budgetCfg.Hours, m.budgetCfg.Minutes, tokens))
	m.modal = &modalState{title: "审计预算", compose: true, budget: true, editor: editor}
	m.input.Blur()
	m.resizeModal()
	return m.modal.editor.Focus()
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
	if m.modal.compose {
		m.modal.editor.SetWidth(inner)
		m.modal.editor.SetHeight(bodyHeight)
		return
	}
	m.wrapModal(inner)
	m.modal.scroll = min(m.modal.scroll, max(0, len(m.modal.wrapped)-bodyHeight))
	if m.modal.findings != nil && !m.modal.findings.detail {
		m.modal.keepFindingVisible(bodyHeight)
	}
}

func (m Model) wrapModal(width int) {
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
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "esc", "ctrl+[":
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
				if state.budget {
					p := strings.Fields(value)
					if len(p) != 4 || (p[0] != "normal" && p[0] != "infinite") {
						state.err = "格式：normal|infinite 小时 分钟 tokens"
						return nil
					}
					h, e1 := strconv.Atoi(p[1])
					mi, e2 := strconv.Atoi(p[2])
					tok, e3 := strconv.ParseInt(p[3], 10, 64)
					if e1 != nil || e2 != nil || e3 != nil || h < 0 || mi < 0 || tok < 0 {
						state.err = "预算必须是非负整数，0表示不限"
						return nil
					}
					if err := m.runner.ConfigureBudget(p[0] == "infinite", h, mi, tok); err != nil {
						state.err = err.Error()
						return nil
					}
					m.budgetCfg = m.runner.BudgetStatus()
					dir := m.pendingDir
					m.pendingDir = ""
					cmd := m.closeModal()
					if dir != "" {
						return m.startDirectory(dir)
					}
					return cmd
				}
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
	footer := "↑↓/PgUp/PgDn/滚轮 滚动 · Esc 关闭"
	if m.modal.compose {
		content = strings.Split(m.modal.editor.View(), "\n")
		status = fmt.Sprintf("%d / %d bytes", len(m.modal.editor.Value()), maxBroadcastBytes)
		if m.modal.budget {
			status = "normal|infinite 小时 分钟 tokens；0不限；时间/token 任一耗尽即停"
		}
		if m.modal.err != "" {
			status = m.modal.err
		}
		footer = "Ctrl+S 发送 · Enter 换行 · Esc 取消"
		if m.modal.budget {
			footer = "Ctrl+S 确认预算 · Esc 取消（不启动）"
		}
	} else {
		m.wrapModal(inner)
		content = viewport(m.modal.wrapped, m.modal.scroll, bodyHeight, false)
		status = fmt.Sprintf("行 %d–%d / %d", min(len(m.modal.wrapped), m.modal.scroll+1), min(len(m.modal.wrapped), m.modal.scroll+bodyHeight), len(m.modal.wrapped))
		if f := m.modal.findings; f != nil {
			if f.detail {
				footer = "↑↓/PgUp/PgDn/滚轮 滚动 · Esc 返回列表"
			} else {
				status = fmt.Sprintf("选中 %d / %d · 打开时快照；重新 /list 刷新", min(len(f.items), f.selected+1), len(f.items))
				footer = "↑↓/滚轮 选择 · PgUp/PgDn/Home/End · Enter 详情 · Esc 关闭"
			}
		}
	}
	rows := make([]string, height)
	rows[0] = "╭" + strings.Repeat("─", width-2) + "╮"
	rows[height-1] = "╰" + strings.Repeat("─", width-2) + "╯"
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
		rows[row] = "│ " + text + strings.Repeat(" ", max(0, inner-runewidth.StringWidth(ansiEscape.ReplaceAllString(text, "")))) + " │"
	}
	background := strings.Split(safeText(frame), "\n")
	for len(background) < m.height {
		background = append(background, "")
	}
	background = background[:m.height]
	x, y := (m.width-width)/2, (m.height-height)/2
	for row, line := range rows {
		base := background[y+row]
		background[y+row] = modalCells(base, 0, x) + line + modalCells(base, x+width, m.width-x-width)
	}
	return strings.Join(background, "\n")
}
