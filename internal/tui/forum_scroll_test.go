package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestForumPostOpensScrollableModal(t *testing.T) {
	m := forumTestModel(t)
	m.resize(124, 22)
	postForumFixture(t, &m, "MODAL-BEGIN\n"+strings.Repeat("long retained evidence line\n", 60)+"MODAL-END")
	m.selectedPost = m.forumPage.Posts[0].ID
	m.toggleForumPost(m.selectedPost)
	if m.modal == nil || !strings.Contains(m.View(), "MODAL-BEGIN") {
		t.Fatal("selected forum post did not open readable modal")
	}
	initial := m.modal.scroll
	for i := 0; i < 30 && !strings.Contains(m.View(), "MODAL-END"); i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	}
	if m.modal.scroll <= initial || !strings.Contains(m.View(), "MODAL-END") {
		t.Fatal("forum modal did not scroll to retained body end")
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.modal != nil {
		t.Fatal("Escape did not close forum modal")
	}
}
