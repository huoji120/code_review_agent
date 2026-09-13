package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestBottomShortcutDoesNotStealInputOrCrossReadingContexts(t *testing.T) {
	m := forumTestModel(t)
	send := func(key tea.KeyMsg) { updated, _ := m.Update(key); m = updated.(Model) }
	g := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")}
	send(g)
	send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if m.input.Value() != "go" {
		t.Fatal("navigation stole command input")
	}
	m.showDetail("reading", []string{strings.Repeat("line\n", 100) + "READ_BOTTOM"})
	send(g)
	if m.modal.scroll != 0 {
		t.Fatal("single g jumped")
	}
	// A different reading surface cannot consume the first surface's prefix.
	m.showDetail("another", []string{strings.Repeat("line\n", 100) + "READ_BOTTOM"})
	send(g)
	if m.modal.scroll != 0 {
		t.Fatal("shortcut crossed modal identity")
	}
	send(g)
	if !strings.Contains(m.overlayModal(""), "READ_BOTTOM") {
		t.Fatal("gg did not reveal final text")
	}
	send(tea.KeyMsg{Type: tea.KeyHome})
	send(g)
	m.bottomAt = time.Now().Add(-time.Second)
	send(g)
	if m.modal.scroll != 0 {
		t.Fatal("expired shortcut triggered")
	}
	send(tea.KeyMsg{Type: tea.KeyEsc})
	m.openBroadcast("")
	send(g)
	send(g)
	if m.modal.editor.Value() != "gg" {
		t.Fatal("shortcut stole broadcast text")
	}
}
