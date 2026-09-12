package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestBottomPostRevealsPreviewAndExpandedBody(t *testing.T) {
	m := forumTestModel(t)
	m.resize(124, 22)
	postForumFixture(t, &m, "BOTTOM-PREVIEW\n"+strings.Repeat("long retained evidence line\n", 60)+"BOTTOM-END")
	for i := 0; i < 14; i++ {
		postForumFixture(t, &m, fmt.Sprintf("other thread %d", i))
	}
	m.selectedPost = m.forumPage.Posts[0].ID
	for i := 0; i < 14; i++ {
		m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	}
	if !strings.Contains(m.View(), "BOTTOM-PREVIEW") {
		t.Fatal("bottom selection only reveals its title, leaving content outside viewport")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.View(), "BOTTOM-PREVIEW") {
		t.Fatal("expansion failed to reveal beginning of post")
	}
	for i := 0; i < 30 && !strings.Contains(m.View(), "BOTTOM-END"); i++ {
		m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	}
	if !strings.Contains(m.View(), "BOTTOM-END") || m.selectedPost != 1 {
		t.Fatal("arrow navigation cannot read the final expanded post to its end")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyHome})
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnd})
	if !strings.Contains(m.View(), "BOTTOM-END") {
		t.Fatal("End did not expose expanded retained end")
	}
}

func TestForumRefreshKeepsScrolledReadingAnchor(t *testing.T) {
	m := forumTestModel(t)
	m.resize(124, 22)
	var body strings.Builder
	for i := 0; i < 90; i++ {
		fmt.Fprintf(&body, "evidence-line-%03d\n", i)
	}
	postForumFixture(t, &m, body.String())
	m.toggleForumPost(1)
	m.moveScroll(35)
	_, right, height, _ := m.layout()
	before := strings.Split(m.renderForum(right, height), "\n")[2]
	postForumFixture(t, &m, "new incoming thread should not move the current body")
	after := strings.Split(m.renderForum(right, height), "\n")[2]
	if before != after || !strings.Contains(before, "evidence-line-") {
		t.Fatalf("new thread displaced the visible evidence: %q -> %q", before, after)
	}
	if m.selectedPost != 1 || !m.expandedPosts[1] {
		t.Fatal("refresh changed selected expanded thread")
	}
}
