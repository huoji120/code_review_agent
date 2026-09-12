package tui

import (
	"fmt"
	"strings"

	"code-review-agent/internal/tools"
	tea "github.com/charmbracelet/bubbletea"
)

type findingsModal struct {
	items       []tools.Finding
	selected    int
	detail      bool
	starts      []int
	listWrapped []string
	listWidth   int
	listScroll  int
}

func (m *Model) openFindings(items []tools.Finding, selected int) {
	// Own the snapshot: live aggregation must never retarget an open row.
	items = append([]tools.Finding(nil), items...)
	m.modal = &modalState{
		title: fmt.Sprintf("漏洞列表 · %d 个", len(items)),
		findings: &findingsModal{
			items:    items,
			selected: min(max(0, selected), max(0, len(items)-1)),
		},
	}
	m.input.Blur()
	m.resizeModal()
}

func (state *modalState) wrapFindings(width int) {
	f := state.findings
	if f.listWidth != width {
		f.listWrapped = nil
		f.starts = f.starts[:0]
		indent := min(2, max(0, width-1))
		prefix := strings.Repeat(" ", indent)
		for _, item := range f.items {
			f.starts = append(f.starts, len(f.listWrapped))
			for _, text := range []string{
				fmt.Sprintf("#%d [%s] %s", item.ID, item.Severity, item.Title),
				fmt.Sprintf("%s:%d", item.Path, item.Line),
			} {
				for _, line := range wrapLines(text, max(1, width-indent)) {
					f.listWrapped = append(f.listWrapped, prefix+line)
				}
			}
		}
		if len(f.items) == 0 {
			f.listWrapped = wrapLines("当前尚无漏洞。后台审计继续运行；关闭后重新 /list 查看最新结果。", width)
		}
		f.listWidth = width
		state.markFinding(f.selected, true)
	}
	state.wrapped = f.listWrapped
	state.wrapWidth = width
}

func (state *modalState) markFinding(index int, selected bool) {
	f := state.findings
	if index < 0 || index >= len(f.starts) || f.listWidth <= 1 {
		return
	}
	row := f.starts[index]
	marker := " "
	if selected {
		marker = ">"
	}
	f.listWrapped[row] = marker + f.listWrapped[row][1:]
}

func (state *modalState) keepFindingVisible(height int) {
	f := state.findings
	if len(f.starts) == 0 {
		state.scroll = 0
		return
	}
	start, end := f.starts[f.selected], len(f.listWrapped)
	if f.selected+1 < len(f.starts) {
		end = f.starts[f.selected+1]
	}
	if start < state.scroll || end-start > height {
		state.scroll = start
	} else if end > state.scroll+height {
		state.scroll = end - height
	}
	state.scroll = min(max(0, state.scroll), max(0, len(f.listWrapped)-height))
}

func (m *Model) updateFindings(msg tea.Msg) tea.Cmd {
	state := m.modal
	f := state.findings
	if len(f.items) == 0 {
		return nil
	}
	_, _, height := m.modalSize()
	selected := f.selected
	switch event := msg.(type) {
	case tea.KeyMsg:
		switch event.String() {
		case "up":
			selected--
		case "down":
			selected++
		case "home":
			selected = 0
		case "end":
			selected = len(f.items) - 1
		case "pgup":
			target := f.starts[selected] - height
			for selected > 0 && f.starts[selected] > target {
				selected--
			}
		case "pgdown":
			target := f.starts[selected] + height
			for selected < len(f.items)-1 && f.starts[selected] < target {
				selected++
			}
		case "enter":
			f.listScroll = state.scroll
			f.detail = true
			item := f.items[selected]
			state.title = fmt.Sprintf("漏洞详情 · %d / %d · #%d", selected+1, len(f.items), item.ID)
			state.lines = []string{
				"标题：" + item.Title,
				"严重程度：" + item.Severity,
				fmt.Sprintf("位置：%s:%d", item.Path, item.Line),
				"CWE：" + item.CWE,
				"", "证据：", item.Evidence,
				"", "影响：", item.Impact,
				"", "建议：", item.Recommendation,
			}
			state.wrapWidth = 0
			state.scroll = 0
			m.resizeModal()
			return nil
		}
	case tea.MouseMsg:
		switch event.Type {
		case tea.MouseWheelUp:
			selected--
		case tea.MouseWheelDown:
			selected++
		}
	}
	selected = min(max(0, selected), len(f.items)-1)
	if selected != f.selected {
		state.markFinding(f.selected, false)
		f.selected = selected
		state.markFinding(f.selected, true)
		state.keepFindingVisible(height)
	}
	return nil
}

func (m *Model) returnToFindings() {
	state := m.modal
	f := state.findings
	f.detail = false
	state.title = fmt.Sprintf("漏洞列表 · %d 个", len(f.items))
	state.lines = nil
	state.scroll = f.listScroll
	state.wrapped = f.listWrapped
	state.wrapWidth = f.listWidth
	m.resizeModal()
}
