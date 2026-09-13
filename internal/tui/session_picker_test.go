package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionPickerDisambiguatesRestoresAndOwnsKeys(t *testing.T) {
	m := forumTestModel(t)
	m.sessionDir = t.TempDir()
	for _, name := range []string{"20260913-100000.123.json", "20260913-110000.456.json"} {
		if err := m.runner.SaveSession(filepath.Join(m.sessionDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	m.submit("/session")
	if m.modal == nil || len(m.modal.sessions.items) != 2 {
		t.Fatal("session list missing or duplicated")
	}
	first := m.modal.sessions.items[0]
	m.updateModal(tea.KeyMsg{Type: tea.KeyDown})
	selected := m.modal.sessions.items[1]
	if selected == first || !strings.Contains(m.View(), "> ") {
		t.Fatal("selection not visible")
	}
	m.busy = true
	m.updateModal(tea.KeyMsg{Type: tea.KeyEnter})
	if m.sessionPath != "" || m.modal == nil {
		t.Fatal("restored while busy")
	}
	m.updateModal(tea.KeyMsg{Type: tea.KeyEsc})
	if m.modal != nil || !m.busy || m.stopping {
		t.Fatal("Escape paused audit")
	}
	m.busy = false
	m.restoreSession("20260913")
	if m.modal == nil || m.sessionPath != "" {
		t.Fatal("ambiguous restore silently chose")
	}
	m.updateModal(tea.KeyMsg{Type: tea.KeyDown})
	m.updateModal(tea.KeyMsg{Type: tea.KeyEnter})
	if m.sessionPath != selected || m.modal != nil || !m.input.Focused() {
		t.Fatalf("selection did not restore: %s", m.sessionPath)
	}
	m.input.SetValue("/restore 110000.456")
	m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	if !strings.Contains(m.input.Value(), "110000.456.json") {
		t.Fatal("Tab lost dotted timestamp suffix")
	}
	m.restoreSession(strings.TrimSuffix(first, ".json"))
	if m.sessionPath != first {
		t.Fatal("absolute path without extension failed")
	}
	before := m.sessionPath
	m.restoreSession("not-a-session")
	if m.sessionPath != before {
		t.Fatal("missing match changed session")
	}
	if err := os.WriteFile(filepath.Join(m.sessionDir, "broken.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	m.restoreSession("broken")
	if m.sessionPath != before {
		t.Fatal("invalid session replaced current session")
	}
}
