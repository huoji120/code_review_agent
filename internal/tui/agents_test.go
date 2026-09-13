package tui

import (
	"strings"
	"testing"

	"code-review-agent/internal/agent"
	tea "github.com/charmbracelet/bubbletea"
)

func TestAgentDiagnosticTracksIdentityAcrossLiveRosterChanges(t *testing.T) {
	m := forumTestModel(t)
	first := agent.WorkerStatus{ID: "audit-1", Name: "first", Status: "running", OutputExcerpt: "FIRST_ONLY"}
	selected := agent.WorkerStatus{ID: "audit-2", Name: "selected", Status: "failed", OutputExcerpt: strings.Repeat("evidence\n", 60) + "SELECTED_END"}
	m.applyEvent(agent.Event{Kind: "worker", Workers: []agent.WorkerStatus{first, selected}})
	m.showAgents()
	m.updateModal(tea.KeyMsg{Type: tea.KeyEnd})
	m.updateModal(tea.KeyMsg{Type: tea.KeyEnter})
	m.updateModal(tea.KeyMsg{Type: tea.KeyEnd})
	before := m.modal.scroll
	if !strings.Contains(m.overlayModal(""), "SELECTED_END") {
		t.Fatal("selected diagnostic tail unreachable")
	}
	selected.OutputExcerpt = strings.ReplaceAll(selected.OutputExcerpt, "SELECTED_END", "UPDATED_END")
	m.applyEvent(agent.Event{Kind: "worker", Workers: []agent.WorkerStatus{selected, first}})
	view := m.overlayModal("")
	if m.modal.scroll != before || !strings.Contains(view, "UPDATED_END") || strings.Contains(view, "FIRST_ONLY") {
		t.Fatal("live reorder retargeted or reset the open diagnostic")
	}
	m.applyEvent(agent.Event{Kind: "worker", Workers: []agent.WorkerStatus{first}})
	if m.modal.agentDetails || strings.Contains(m.overlayModal(""), "UPDATED_END") {
		t.Fatal("removed worker silently replaced an open diagnostic")
	}
	m.applyEvent(agent.Event{Kind: "worker", Workers: []agent.WorkerStatus{}})
	m.updateModal(tea.KeyMsg{Type: tea.KeyEnter})
	if m.modal.agentDetails {
		t.Fatal("empty roster opened a stale worker")
	}
}
