package tui

import (
	"fmt"
	"strings"

	"code-review-agent/internal/agent"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

type agentsModal struct {
	selectedID string
	listScroll int
	lineStarts []int
}

func agentName(status agent.WorkerStatus) string {
	if status.Name != "" {
		return status.Name
	}
	return "未命名（" + status.ID + "）"
}

func agentValue(value string) string {
	if value == "" {
		return "暂无记录"
	}
	return value
}

func agentSummary(text string, width int) string {
	return runewidth.Truncate(strings.ReplaceAll(safeText(text), "\n", " "), width, "…")
}

func (m *Model) refreshAgentsModal() {
	state := m.modal
	if state == nil || !state.agents {
		return
	}
	if state.agentState == nil {
		state.agentState = &agentsModal{}
	}
	a := state.agentState
	found := false
	for i, status := range m.workers {
		if status.ID == a.selectedID {
			state.agentIndex = i
			found = true
			break
		}
	}
	if !found {
		if state.agentDetails {
			state.agentDetails = false
			state.scroll = a.listScroll
		}
		state.agentIndex = min(max(0, state.agentIndex), max(0, len(m.workers)-1))
	}
	a.selectedID = ""
	if len(m.workers) > 0 {
		a.selectedID = m.workers[state.agentIndex].ID
	}
	state.title = fmt.Sprintf("Agent 列表 · %d 个", len(m.workers))
	if state.agentDetails {
		state.title = "Agent 详情 · " + agentSummary(agentName(m.workers[state.agentIndex]), 48)
	}
	state.lines = m.agentDetailLines()
	state.wrapWidth = 0
	m.resizeModal()
}

func (state *modalState) wrapAgents(width int) {
	if state.wrapWidth == width {
		return
	}
	state.wrapped = nil
	state.agentStarts = state.agentStarts[:0]
	next := 0
	for i, line := range state.lines {
		if !state.agentDetails && state.agentState != nil && next < len(state.agentState.lineStarts) && i == state.agentState.lineStarts[next] {
			state.agentStarts = append(state.agentStarts, len(state.wrapped))
			next++
		}
		state.wrapped = append(state.wrapped, wrapLines(line, width)...)
	}
	state.wrapWidth = width
}

func (state *modalState) keepAgentVisible(height int) {
	if state.agentDetails {
		return
	}
	if state.agentIndex < 0 || state.agentIndex >= len(state.agentStarts) {
		state.scroll = 0
		return
	}
	start, end := state.agentStarts[state.agentIndex], len(state.wrapped)
	if state.agentIndex+1 < len(state.agentStarts) {
		end = state.agentStarts[state.agentIndex+1]
	}
	// The blank separator is not part of the selected worker's content.
	if end > start && state.wrapped[end-1] == "" {
		end--
	}
	if start < state.scroll || end-start > height {
		state.scroll = start
	} else if end > state.scroll+height {
		state.scroll = end - height
	}
	state.scroll = min(max(0, state.scroll), max(0, len(state.wrapped)-height))
}

func (m *Model) updateAgents(msg tea.Msg) tea.Cmd {
	state := m.modal
	width, modalHeight, height := m.modalSize()
	if state.agentDetails {
		delta := 0
		switch event := msg.(type) {
		case tea.KeyMsg:
			switch event.String() {
			case "up":
				delta = -1
			case "down":
				delta = 1
			case "pgup":
				delta = -height
			case "pgdown":
				delta = height
			case "home":
				state.scroll = 0
			case "end":
				state.scroll = len(state.wrapped)
			}
		case tea.MouseMsg:
			switch event.Type {
			case tea.MouseWheelUp:
				delta = -3
			case tea.MouseWheelDown:
				delta = 3
			}
		}
		state.scroll = min(max(0, state.scroll+delta), max(0, len(state.wrapped)-height))
		return nil
	}
	if len(m.workers) == 0 || len(state.agentStarts) != len(m.workers) {
		return nil
	}
	selected := state.agentIndex
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
			selected = len(m.workers) - 1
		case "pgup":
			target := state.agentStarts[selected] - height
			for selected > 0 && state.agentStarts[selected] > target {
				selected--
			}
		case "pgdown":
			target := state.agentStarts[selected] + height
			for selected < len(m.workers)-1 && state.agentStarts[selected] < target {
				selected++
			}
		case "enter":
			state.agentState.listScroll = state.scroll
			state.agentDetails = true
			state.scroll = 0
			m.refreshAgentsModal()
			return nil
		}
	case tea.MouseMsg:
		switch event.Type {
		case tea.MouseWheelUp:
			selected--
		case tea.MouseWheelDown:
			selected++
		case tea.MouseLeft:
			if m.width < 20 || m.height < 8 {
				return nil
			}
			x, y := (m.width-width)/2, (m.height-modalHeight)/2
			if event.X < x+2 || event.X >= x+width-2 || event.Y < y+2 || event.Y >= y+2+height {
				return nil
			}
			row := state.scroll + event.Y - y - 2
			if row < state.agentStarts[0] || row >= len(state.wrapped) {
				return nil
			}
			for i, start := range state.agentStarts {
				if start > row {
					break
				}
				selected = i
			}
		}
	}
	selected = min(max(0, selected), len(m.workers)-1)
	if selected != state.agentIndex {
		state.agentIndex = selected
		state.agentState.selectedID = m.workers[selected].ID
		m.refreshAgentsModal()
	}
	return nil
}

func (m *Model) returnToAgents() {
	state := m.modal
	state.agentDetails = false
	state.scroll = state.agentState.listScroll
	m.refreshAgentsModal()
}
