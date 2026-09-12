package tui

import (
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"code-review-agent/internal/agent"
	"code-review-agent/internal/config"
	"code-review-agent/internal/forum"
	"code-review-agent/internal/prompt"
	"code-review-agent/internal/tools"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

func TestChineseLocaleFrameFitsTerminal(t *testing.T) {
	t.Setenv("RUNEWIDTH_EASTASIAN", "")
	terminalWidthOnce = sync.Once{}
	oldMode, oldCondition := runewidth.EastAsianWidth, runewidth.DefaultCondition
	defer func() {
		runewidth.EastAsianWidth, runewidth.DefaultCondition = oldMode, oldCondition
		terminalWidthOnce = sync.Once{}
	}()
	// Reproduce a CJK host locale; physical terminal ambiguous glyphs are narrow.
	runewidth.EastAsianWidth = true
	runewidth.DefaultCondition = runewidth.NewCondition()
	physicalWidth := &runewidth.Condition{EastAsianWidth: false, StrictEmojiNeutral: true}
	r, err := tools.NewRegistry(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	team := agent.NewTeam(cfg, prompt.Prompts{}, nil, nil, r)
	defer team.Close()
	if err := team.PostMessage("中文论坛内容与单宽边框"); err != nil {
		t.Fatal(err)
	}
	model := New(team, cfg, "")
	for _, size := range [][2]int{{132, 38}, {70, 25}, {18, 6}} {
		updated, _ := model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := updated.View()
		if !utf8.ValidString(view) {
			t.Fatal("Chinese placeholder split a UTF-8 character")
		}
		lines := strings.Split(view, "\n")
		if len(lines) > size[1] {
			t.Fatalf("frame exceeds terminal rows: %d > %d", len(lines), size[1])
		}
		for _, line := range lines {
			if width := physicalWidth.StringWidth(ansiEscape.ReplaceAllString(line, "")); width > size[0] {
				t.Fatalf("frame exceeds physical columns: %d > %d", width, size[0])
			}
		}
	}
}

func forumTestModel(t *testing.T) Model {
	t.Helper()
	r, err := tools.NewRegistry(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	team := agent.NewTeam(cfg, prompt.Prompts{}, nil, nil, r)
	t.Cleanup(func() { _ = team.Close() })
	return New(team, cfg, "")
}

func TestModelIgnoresStreamDeltasAndKeepsTurnToolOperations(t *testing.T) {
	m := forumTestModel(t)
	m.applyEvent(agent.Event{Kind: "worker", AgentID: "recon-1", Content: "", Workers: []agent.WorkerStatus{{ID: "recon-1", Name: "桃子", Turn: 1, Status: "running", Activity: "思考中"}}})
	m.applyEvent(agent.Event{Kind: "think_delta", AgentID: "recon-1", Content: "PRIVATE_REASONING_SENTINEL"})
	m.applyEvent(agent.Event{Kind: "tool", AgentID: "recon-1", Content: "calling tool", Workers: []agent.WorkerStatus{{ID: "recon-1", Name: "桃子", Turn: 1, Status: "running", Activity: "calling tool"}}})
	m.applyEvent(agent.Event{Kind: "worker", AgentID: "recon-1", Content: "", Workers: []agent.WorkerStatus{{ID: "recon-1", Name: "桃子", Turn: 2, Status: "running", Activity: "思考中"}}})
	view := m.View()
	if strings.Contains(view, "PRIVATE_REASONING_SENTINEL") || strings.Contains(view, "思考摘录") || strings.Contains(view, "摘要") || !strings.Contains(view, "calling tool") || !strings.Contains(view, "思考中") {
		t.Fatalf("unexpected stream/status rendering: %q", view)
	}
}

func postForumFixture(t *testing.T, m *Model, body string) {
	t.Helper()
	if err := m.runner.PostMessage(body); err != nil {
		t.Fatal(err)
	}
	m.refreshForumPage()
}

func TestForumSelectionSurvivesReplyBump(t *testing.T) {
	m := forumTestModel(t)
	postForumFixture(t, &m, "older")
	postForumFixture(t, &m, "newer")
	m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	selected := m.selectedPost
	m.submit("/reply 1 bump older thread")
	if m.forumPage.Posts[0].ID == selected {
		t.Fatal("fixture did not move the selected post")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.selectedPost != selected || !m.expandedPosts[selected] || m.expandedPosts[1] {
		t.Fatal("Enter toggled the bumped index instead of the selected ID")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.expandedPosts[selected] {
		t.Fatal("second Enter did not collapse selected post")
	}
	// Eviction must not silently transfer an imminent Enter to another post.
	m.forumPage.Posts = []forum.Post{{ID: 99, Root: forum.Message{ID: 99, Content: "replacement"}}}
	m.reconcileForum()
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.expandedPosts) != 0 {
		t.Fatal("expired selection toggled a replacement post")
	}
}

func TestForumClickWrappedHeaderAfterScrolling(t *testing.T) {
	for _, width := range []int{120, 40} {
		m := forumTestModel(t)
		m.width, m.height = width, 16
		m.forumPage = forum.PostPage{Posts: []forum.Post{
			{ID: 2, Root: forum.Message{ID: 2, Topic: "newer", Content: "preview"}},
			{ID: 1, Root: forum.Message{ID: 1, Topic: strings.Repeat("wrapped-title ", 12), Content: strings.Repeat("body\n", 20)}},
		}}
		m.reconcileForum()
		left, right, _, narrow := m.layout()
		m.forumContent(right - 4)
		header := m.forumCache.headers[1]
		if header.end-header.start < 2 {
			t.Fatal("fixture title does not wrap")
		}
		m.scroll[1], m.follow[1] = header.start+1, false
		x := 2
		if !narrow {
			x += left + 1
		}
		updated, _ := m.Update(tea.MouseMsg{X: x, Y: 3, Type: tea.MouseLeft})
		m = updated.(Model)
		if m.selectedPost != 1 || !m.expandedPosts[1] || m.expandedPosts[2] {
			t.Fatalf("width %d: scrolled wrapped-title click toggled wrong post", width)
		}
		// Border/title rows are not content hit targets.
		m.clickForum(x, 2)
		m.clickForum(x-2, 3)
		if !m.expandedPosts[1] {
			t.Fatal("pane border toggled the post")
		}
		for _, h := range m.forumCache.headers {
			if h.id == 1 {
				m.scroll[1], m.follow[1] = h.start, false
				break
			}
		}
		m.clickForum(x, 3)
		if m.expandedPosts[1] {
			t.Fatal("second header click did not collapse")
		}
	}
}

func TestForumDoesNotHijackCommandInput(t *testing.T) {
	m := forumTestModel(t)
	postForumFixture(t, &m, "existing")
	m.input.SetValue("/reply 1 typed command")
	selected := m.selectedPost
	for _, key := range []tea.KeyType{tea.KeyUp, tea.KeyDown, tea.KeyLeft, tea.KeyRight, tea.KeyHome, tea.KeyEnd} {
		if _, handled := m.handleKey(tea.KeyMsg{Type: key}); handled {
			t.Fatalf("command editing key %v was hijacked", key)
		}
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	posts := m.runner.ForumPosts(1, forum.DefaultPageSize, "typed command")
	if posts.TotalPosts != 1 || len(posts.Posts[0].Replies) != 1 || posts.Posts[0].Replies[0].Content != "typed command" || m.input.Value() != "" {
		t.Fatal("command Enter did not submit the typed post")
	}
	if m.selectedPost != selected || len(m.expandedPosts) != 0 {
		t.Fatal("command Enter changed expansion/selection")
	}
}

func TestForumExpansionShowsRetainedBodyBeyondPreview(t *testing.T) {
	m := forumTestModel(t)
	body := strings.Repeat("a", 9000) + " retained-body-end"
	if err := m.runner.PostMessage(body); err != nil {
		t.Fatal(err)
	}
	m.refreshForumPage()
	if strings.Contains(strings.Join(m.forumContent(100), ""), "retained-body-end") {
		t.Fatal("collapsed post exposed full body")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(strings.Join(m.forumContent(100), ""), "retained-body-end") {
		t.Fatal("expanded post retained UI preview truncation")
	}
}

func TestDetailModalIsolatesPanelInputAndEscape(t *testing.T) {
	m := forumTestModel(t)
	postForumFixture(t, &m, "post")
	m.busy = true
	stopped := false
	m.cancel = func() { stopped = true }
	m.focus, m.scroll, m.follow = 1, [2]int{2, 3}, [2]bool{false, false}
	m.input.SetValue("preserved command")
	selected, scroll := m.selectedPost, m.scroll
	m.showDetail("Details", strings.Split(strings.Repeat("line\n", 100), "\n"))
	for _, key := range []tea.KeyMsg{{Type: tea.KeyPgDown}, {Type: tea.KeyDown}, {Type: tea.KeyEnter}, {Type: tea.KeyTab}, {Type: tea.KeyRunes, Runes: []rune("typing")}} {
		updated, _ := m.Update(key)
		m = updated.(Model)
	}
	if m.modal.scroll == 0 || m.focus != 1 || m.selectedPost != selected || m.scroll != scroll || len(m.expandedPosts) != 0 || m.input.Value() != "preserved command" {
		t.Fatal("detail dialog input leaked into underlying panels or command input")
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.modal != nil || stopped || m.stopping || !m.busy || !m.input.Focused() {
		t.Fatal("dialog Escape paused audit or failed to restore command input")
	}
}

func TestBroadcastDialogRequiresConfirmationAndPreservesDraft(t *testing.T) {
	m := forumTestModel(t)
	m.submit("/say cancel this")
	if len(m.runner.ForumMessages()) != 0 || m.modal == nil || !m.modal.compose {
		t.Fatal("say submitted before explicit confirmation")
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.modal != nil || len(m.runner.ForumMessages()) != 0 {
		t.Fatal("cancelled broadcast was sent")
	}
	m.submit("/say")
	m.modal.editor.SetValue(strings.Repeat("界", 6000))
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = updated.(Model)
	if m.modal == nil || m.modal.err == "" || len(m.runner.ForumMessages()) != 0 {
		t.Fatal("over-byte-limit draft was sent or discarded")
	}
	m.modal.editor.SetValue("hello")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("world")})
	m = updated.(Model)
	if len(m.runner.ForumMessages()) != 0 || m.modal.editor.Value() != "hello\nworld" {
		t.Fatal("composer Enter submitted rather than inserting a newline")
	}
	for _, size := range [][2]int{{70, 25}, {18, 6}, {140, 42}} {
		updated, _ = m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m = updated.(Model)
		if m.modal.editor.Value() != "hello\nworld" {
			t.Fatal("resize lost the draft")
		}
		lines := strings.Split(m.View(), "\n")
		if len(lines) > size[1] {
			t.Fatal("modal exceeds viewport height")
		}
		for _, line := range lines {
			if runewidth.StringWidth(ansiEscape.ReplaceAllString(line, "")) > size[0] {
				t.Fatalf("modal exceeds viewport %dx%d: width=%d line=%q", size[0], size[1], runewidth.StringWidth(ansiEscape.ReplaceAllString(line, "")), line)
			}
		}
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = updated.(Model)
	messages := m.runner.ForumMessages()
	if m.modal != nil || len(messages) != 1 || messages[0].Content != "hello\nworld" || !m.input.Focused() {
		t.Fatal("confirmed broadcast was not sent exactly once")
	}
}

func TestChosenNameReplacesRoutingIDInStatusPostsAndHistory(t *testing.T) {
	m := forumTestModel(t)
	m.resize(160, 42)
	m.applyEvent(agent.Event{Kind: "worker", AgentID: "recon-1", Content: "请求模型", Workers: []agent.WorkerStatus{{ID: "recon-1", Status: "running"}}})
	if view := m.View(); strings.Contains(view, "recon-1") || !strings.Contains(view, "正在命名") {
		t.Fatal("unnamed worker leaked its routing ID")
	}
	m.applyEvent(agent.Event{Kind: "tool", AgentID: "recon-1", Content: "calling read_file", Workers: []agent.WorkerStatus{{ID: "recon-1", Name: "追光者", Status: "running"}, {ID: "audit-1", Name: "守望者", Status: "running"}}})
	m.forumPage = forum.PostPage{Posts: []forum.Post{{ID: 1, Root: forum.Message{ID: 1, AgentID: "recon-1", AgentName: "追光者", Topic: "入口证据", Content: "root evidence"}, Replies: []forum.Message{{ID: 2, AgentID: "audit-1", AgentName: "守望者", ReplyTo: 1, Content: "reply evidence"}}}}, Page: 1, TotalPages: 1, TotalPosts: 1}
	m.reconcileForum()
	m.toggleForumPost(1)
	view := m.View()
	for _, id := range []string{"recon-1", "audit-1", "正在命名"} {
		if strings.Contains(view, id) {
			t.Fatalf("routing ID or stale naming label remains: %q", id)
		}
	}
	for _, text := range []string{"追光者 请求模型", "追光者 calling read_file", "追光者 [", "守望者", "reply evidence"} {
		if !strings.Contains(view, text) {
			t.Fatalf("chosen name missing from rendered activity/forum: %q", text)
		}
	}
}
