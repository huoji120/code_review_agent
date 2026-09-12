package tui

import (
	"fmt"
	"strings"
	"testing"

	"code-review-agent/internal/tools"
	tea "github.com/charmbracelet/bubbletea"
)

func TestFindingsSelectionSurvivesWrappedResize(t *testing.T) {
	m := forumTestModel(t)
	items := make([]tools.Finding, 30)
	for i := range items {
		items[i] = tools.Finding{ID: i + 1, Severity: "high", Title: strings.Repeat("wrapped title ", 8), Path: fmt.Sprintf("src/item%d.go", i+1), Line: i + 10}
	}
	m.openFindings(items, 0)
	for _, key := range []tea.KeyType{tea.KeyPgDown, tea.KeyPgDown, tea.KeyEnd} {
		updated, _ := m.Update(tea.KeyMsg{Type: key})
		m = updated.(Model)
	}
	for _, size := range [][2]int{{70, 25}, {38, 12}, {18, 6}, {132, 38}} {
		updated, _ := m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m = updated.(Model)
		if m.modal.findings.selected != len(items)-1 {
			t.Fatal("resizing changed the selected finding")
		}
		if size[0] >= 20 && !strings.Contains(m.View(), "> #30") {
			t.Fatal("selected wrapped finding is outside the visible list")
		}
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m = updated.(Model)
	if m.modal.findings.selected >= len(items)-1 {
		t.Fatal("page up cannot leave the bottom finding")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m = updated.(Model)
	if !strings.Contains(m.View(), "> #1 ") {
		t.Fatal("Home failed to reveal the first finding")
	}
}

func TestFindingDetailFullSnapshotAndNestedEscape(t *testing.T) {
	m := forumTestModel(t)
	m.busy = true
	stopped := false
	m.cancel = func() { stopped = true }
	m.input.SetValue("preserved input")
	items := []tools.Finding{
		{ID: 1, Title: "first"},
		{ID: 2, Severity: "critical", Title: strings.Repeat("title ", 60) + "\nTITLE_TAIL", Path: "src/second.go", Line: 77, CWE: "CWE-79",
			Evidence:       strings.Repeat("evidence line\n", 400) + "EVIDENCE_TAIL",
			Impact:         strings.Repeat("impact line\n", 200) + "IMPACT_TAIL",
			Recommendation: strings.Repeat("recommendation line\n", 200) + "RECOMMENDATION_TAIL"},
	}
	m.openFindings(items, 0)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	listScroll := m.modal.scroll
	items[1] = tools.Finding{ID: 99, Title: "replacement after open"}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	var rendered strings.Builder
	for {
		rendered.WriteString(m.View())
		previous := m.modal.scroll
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
		m = updated.(Model)
		if m.modal.scroll == previous {
			break
		}
	}
	for _, value := range []string{"TITLE_TAIL", "critical", "src/second.go:77", "CWE-79", "EVIDENCE_TAIL", "IMPACT_TAIL", "RECOMMENDATION_TAIL"} {
		if !strings.Contains(rendered.String(), value) {
			t.Fatalf("full snapshot detail is not reachable by paging: %s", value)
		}
	}
	if strings.Contains(rendered.String(), "replacement after open") {
		t.Fatal("live source mutation changed the open snapshot")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.modal == nil || m.modal.findings.detail || m.modal.findings.selected != 1 || m.modal.scroll != listScroll {
		t.Fatal("detail Escape did not restore the prior list position")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.modal != nil || stopped || m.stopping || !m.busy || !m.input.Focused() || m.input.Value() != "preserved input" {
		t.Fatal("nested Escape paused the audit or failed to restore input")
	}
}

func TestEmptyFindingsNavigationClosesWithoutPausing(t *testing.T) {
	m := forumTestModel(t)
	m.busy = true
	stopped := false
	m.cancel = func() { stopped = true }
	m.openFindings(nil, 10)
	for _, key := range []tea.KeyType{tea.KeyDown, tea.KeyUp, tea.KeyPgDown, tea.KeyPgUp, tea.KeyHome, tea.KeyEnd, tea.KeyEnter} {
		updated, _ := m.Update(tea.KeyMsg{Type: key})
		m = updated.(Model)
	}
	updated, _ := m.Update(tea.MouseMsg{Type: tea.MouseWheelDown})
	m = updated.(Model)
	if m.modal == nil || m.modal.findings.detail || m.modal.scroll != 0 {
		t.Fatal("empty findings navigation entered a nonexistent detail")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.modal != nil || stopped || !m.busy || !m.input.Focused() {
		t.Fatal("empty findings Escape paused the audit or trapped input")
	}
}
