package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"strings"
	"testing"
)

func TestBudgetCancelDoesNotStartAndInvalidModeCannotDisableLimits(t *testing.T) {
	m := forumTestModel(t)
	if err := m.runner.ConfigureBudget(true, 8, 0, 0); err != nil {
		t.Fatal(err)
	}
	m.syncTeam()
	m.submit("/dir never-open-this")
	if m.modal == nil || !m.modal.budget || m.busy {
		t.Fatal("infinite startup did not wait for budget")
	}
	m.modal.editor.SetValue("typo 0 0 0")
	m.updateModal(tea.KeyMsg{Type: tea.KeyCtrlS})
	if m.modal == nil || m.modal.err == "" || !m.runner.BudgetStatus().InfiniteMode || m.busy {
		t.Fatal("invalid mode silently disabled limits")
	}
	m.updateModal(tea.KeyMsg{Type: tea.KeyEsc})
	if m.pendingDir != "" || m.modal != nil || m.busy {
		t.Fatal("cancel retained deferred startup")
	}
	m.submit("/budget")
	m.modal.editor.SetValue("infinite 0 0 12345")
	m.updateModal(tea.KeyMsg{Type: tea.KeyCtrlS})
	if m.busy || m.runner.BudgetStatus().TokenLimit != 12345 {
		t.Fatal("budget edit launched cancelled directory")
	}
	m.resize(140, 40)
	if text := m.View(); !strings.Contains(text, "12345") || !strings.Contains(text, "不限") {
		t.Fatal("budget bounds missing from actual view")
	}
}
