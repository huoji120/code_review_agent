package tui

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"code-review-agent/internal/forum"
	"code-review-agent/internal/llm"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

var frameColor = lipgloss.Color("240")
var focusColor = lipgloss.Color("73")
var ansiEscape = regexp.MustCompile("\\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\\x07\\x1b]*(?:\\x07|\\x1b\\\\))")

func boundedText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + " …（显示截断）"
}
func safeText(text string) string {
	text = ansiEscape.ReplaceAllString(text, "")
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' || (r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069') {
			return -1
		}
		return r
	}, text)
}
func fitLine(text string, width int) string {
	if width <= 0 {
		return ""
	}
	return runewidth.Truncate(strings.ReplaceAll(safeText(text), "\n", " "), width, "")
}
func wrapLines(text string, width int) []string {
	width = max(1, width)
	text = safeText(text)
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		var b strings.Builder
		used := 0
		for _, r := range line {
			rw := runewidth.RuneWidth(r)
			if rw > width {
				r = '?'
				rw = 1
			}
			if used+rw > width {
				lines = append(lines, b.String())
				b.Reset()
				used = 0
			}
			b.WriteRune(r)
			used += rw
		}
		lines = append(lines, b.String())
	}
	return lines
}
func (m *Model) resize(width, height int) {
	if width > 0 {
		m.width = width
	}
	if height > 0 {
		m.height = height
	}
	m.input.Width = max(1, m.width-4)
	for pane := 0; pane < 2; pane++ {
		_, limit := m.scrollContent(pane)
		m.scroll[pane] = min(m.scroll[pane], limit)
	}
	m.resizeModal()
}
func (m Model) layout() (left, right, height int, narrow bool) {
	height = max(3, m.height-3)
	narrow = m.width < 90
	if narrow {
		return max(4, m.width), max(4, m.width), height, true
	}
	left = max(32, m.width*36/100)
	return left, m.width - left - 1, height, false
}
func phaseLabel(phase string) string {
	switch phase {
	case "recon":
		return "侦察"
	case "audit":
		return "审计"
	case "completed":
		return "完成"
	case "failed":
		return "失败"
	case "idle", "":
		return "待命"
	}
	return phase
}
func statusLabel(status string) string {
	switch status {
	case "running":
		return "运行"
	case "waiting":
		return "等待"
	case "completed":
		return "完成"
	case "failed":
		return "失败"
	case "cancelled":
		return "停止"
	case "pending":
		return "待开始"
	}
	return status
}
func (m Model) summary() string {
	done, reviewed := 0, 0
	for _, t := range m.snapshot.Todos {
		if t.Status == "done" || t.Status == "completed" {
			done++
		}
	}
	for _, f := range m.snapshot.Files {
		if f.Status == "reviewed" {
			reviewed++
		}
	}
	return fmt.Sprintf("覆盖 %d/%d · Todo %d/%d · 漏洞 %d", reviewed, len(m.snapshot.Files), done, len(m.snapshot.Todos), len(m.snapshot.Findings))
}

func generationLabel(progress *llm.GenerationProgress) string {
	if progress == nil {
		return ""
	}
	if progress.Estimated {
		return fmt.Sprintf("generation≈%d tok", progress.OutputTokens)
	}
	return fmt.Sprintf("generation %d tok", progress.OutputTokens)
}

func activityClock(value string) string {
	if stamp, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return stamp.Local().Format("15:04:05")
	}
	return value
}

// Pin current team progress above the scrollable operations area. Excess workers
// remain accessible in that area's scrollback on small screens or large teams.
func (m Model) leftContent(width, height int) (pinned, lines []string) {
	pinned = append(pinned, fitLine(m.summary(), width))
	maxPinned := max(1, (height-5)/2)
	for i, w := range m.workers {
		activity := w.Activity
		generation := generationLabel(w.Generation)
		if w.Generation != nil && w.Generation.ReceivedAt == "" {
			activity = "等待首段输出 · " + activity
		} else if w.LastModelActivity != "" {
			activity += " · 输出 " + activityClock(w.LastModelActivity)
		}
		row := fmt.Sprintf("%s %s [%s] #%d %s", m.agentLabel(w.ID, w.Name), generation, statusLabel(w.Status), w.Turn, boundedText(activity, 220))
		if i < maxPinned {
			pinned = append(pinned, fitLine(row, width))
		} else {
			lines = append(lines, wrapLines(row, width)...)
		}
	}
	if len(m.workers) == 0 {
		pinned = append(pinned, "团队尚未启动")
	}
	if len(m.workers) > maxPinned {
		pinned = append(pinned, fitLine("其余 worker 见下方滚动区", width))
	}
	pinned = append(pinned, strings.Repeat("─", max(1, width)))
	lines = append(lines, "最近操作 · /help 查看命令")
	for _, e := range m.events {
		text := e.at + " " + strings.TrimSpace(m.agentLabel(e.agentID, "")+" "+e.content)
		lines = append(lines, wrapLines(text, width)...)
	}
	return pinned, lines
}

type forumHeader struct {
	id                  int64
	start, end, bodyEnd int
}

type forumRenderCache struct {
	width    int
	revision uint64
	lines    []string
	headers  []forumHeader
	threadID int64
}

func (m Model) agentLabel(id, name string) string {
	if name != "" {
		return name
	}
	for _, w := range m.workers {
		if w.ID == id && w.Name != "" {
			return w.Name
		}
	}
	switch id {
	case "":
		return ""
	case "user":
		return "用户"
	case "coordinator":
		return "协调器"
	case "system":
		return "系统"
	default:
		return "正在命名"
	}
}

func (m Model) forumContent(width int) []string {
	if cache := m.forumCache; cache != nil && cache.width == width && cache.revision == m.forumRevision && cache.threadID == m.threadID {
		return cache.lines
	}
	var lines []string
	var headers []forumHeader
	if m.forumPage.Gap {
		lines = append(lines, wrapLines("保留缺口：较早消息已过期，正文与回复仅含当前 Board 保留内容。", width)...)
	}
	if m.forumSearch != "" && m.threadID == 0 {
		lines = append(lines, wrapLines("搜索："+m.forumSearch+" · /search 清除", width)...)
	}
	for _, post := range m.forumPage.Posts {
		expanded := m.expandedPosts[post.ID]
		topic := post.Root.Topic
		if topic == "" {
			topic = "讨论"
		}
		if post.RootMissing {
			topic = "原帖不在当前保留窗口"
		}
		marker, state := " ", "[+]"
		if post.ID == m.selectedPost {
			marker = ">"
		}
		if expanded {
			state = "[-]"
		}
		start := len(lines)
		lines = append(lines, wrapLines(fmt.Sprintf("%s %s 帖子 #%d  %s", marker, state, post.ID, topic), width)...)
		headers = append(headers, forumHeader{id: post.ID, start: start, end: len(lines)})
		lines = append(lines, wrapLines(fmt.Sprintf("%s [%s] · 更新 %s · %d 保留回复", m.agentLabel(post.Root.AgentID, post.Root.AgentName), phaseLabel(post.Root.Stage), post.UpdatedAt.Local().Format("15:04:05"), len(post.Replies)), width)...)
		if expanded {
			note := "展开：Board 当前保留正文与回复（不是完整历史保证）。"
			if post.RootMissing {
				note = "保留缺口：原帖已过期；以下为当前保留回复。"
			}
			lines = append(lines, wrapLines(note, width)...)
		}
		if !post.RootMissing {
			body := post.Root.Content
			if !expanded {
				body = boundedText(body, 240)
			}
			lines = append(lines, wrapLines(body, width)...)
		}
		if expanded {
			for _, reply := range post.Replies {
				lines = append(lines, "")
				lines = append(lines, wrapLines(fmt.Sprintf("回复 #%d · %s → #%d · %s", reply.ID, m.agentLabel(reply.AgentID, reply.AgentName), reply.ReplyTo, reply.CreatedAt.Local().Format("15:04:05")), width)...)
				lines = append(lines, wrapLines(reply.Content, width)...)
			}
		} else if len(post.Replies) > 0 {
			reply := post.Replies[len(post.Replies)-1]
			lines = append(lines, wrapLines("最新回复 · "+m.agentLabel(reply.AgentID, reply.AgentName)+"："+boundedText(reply.Content, 180), width)...)
		}
		headers[len(headers)-1].bodyEnd = len(lines)
		lines = append(lines, strings.Repeat("─", max(1, width)), "")
	}
	if len(m.forumPage.Posts) == 0 {
		lines = append(lines, wrapLines("当前页没有帖子；/search 清除搜索，/say 发帖，/forum 返回列表。", width)...)
	}
	if m.forumCache != nil {
		*m.forumCache = forumRenderCache{width: width, revision: m.forumRevision, threadID: m.threadID, lines: lines, headers: headers}
	}
	return lines
}

// Refresh only on board changes or explicit navigation, never on timer redraws.
func (m *Model) refreshForumPage() {
	anchorID, anchorOffset := int64(0), 0
	if !m.follow[1] && m.forumCache != nil {
		_, right, height, _ := m.layout()
		lines := m.forumContent(max(1, right-4))
		start := min(max(0, m.scroll[1]), max(0, len(lines)-max(1, height-3)))
		for _, header := range m.forumCache.headers {
			if header.start > start {
				break
			}
			anchorID, anchorOffset = header.id, start-header.start
		}
	}
	if m.threadID == 0 {
		m.forumPage = m.runner.ForumPosts(max(1, m.forumPage.Page), forum.DefaultPageSize, m.forumSearch)
	} else {
		messages := m.runner.ForumMessages()
		page := forum.PostPage{Page: 1, PageSize: forum.DefaultPageSize, TotalPages: 1}
		page.Gap = len(messages) > 0 && messages[0].ID > 1
		for _, post := range forum.GroupPosts(messages) {
			if post.ID == m.threadID {
				page.Posts = []forum.Post{post}
				page.TotalPosts = 1
				break
			}
		}
		m.forumPage = page
	}
	m.forumDirty = false
	m.reconcileForum()
	if anchorID != 0 {
		_, right, _, _ := m.layout()
		m.forumContent(max(1, right-4))
		for _, header := range m.forumCache.headers {
			if header.id == anchorID {
				m.scroll[1] = header.start + min(anchorOffset, max(0, header.bodyEnd-header.start-1))
				break
			}
		}
		_, limit := m.scrollContent(1)
		m.scroll[1] = min(m.scroll[1], limit)
	}
}

func (m *Model) reconcileForum() {
	retained := make(map[int64]bool, len(m.forumPage.Posts))
	for _, post := range m.forumPage.Posts {
		retained[post.ID] = true
	}
	for id := range m.expandedPosts {
		if !retained[id] {
			delete(m.expandedPosts, id)
		}
	}
	if m.selectedPost == 0 && len(m.forumPage.Posts) > 0 {
		m.selectedPost = m.forumPage.Posts[0].ID
	}
	// A selected ID that leaves this page stays selected but cannot be toggled.
	// Choosing a replacement here would make a later Enter affect the wrong post.
	m.forumRevision++
}

func (m *Model) selectForumPost(delta int) {
	posts := m.forumPage.Posts
	if len(posts) == 0 {
		return
	}
	index := -1
	for i, post := range posts {
		if post.ID == m.selectedPost {
			index = i
			break
		}
	}
	if index >= 0 && m.expandedPosts[m.selectedPost] {
		_, right, height, _ := m.layout()
		lines := m.forumContent(max(1, right-4))
		available := max(1, height-3)
		start := min(m.scroll[1], max(0, len(lines)-available))
		if m.follow[1] {
			start = 0
		}
		for _, header := range m.forumCache.headers {
			if header.id == m.selectedPost && ((delta > 0 && header.bodyEnd > start+available) || (delta < 0 && header.start < start)) {
				m.moveScroll(delta * 3)
				return
			}
		}
	}
	if index < 0 {
		index = 0
	} else {
		index = min(len(posts)-1, max(0, index+delta))
		if posts[index].ID == m.selectedPost {
			m.moveScroll(delta * 3)
			return
		}
	}
	m.selectedPost = posts[index].ID
	m.forumRevision++
	m.revealSelectedPost()
}

func (m *Model) toggleForumPost(id int64) {
	for _, post := range m.forumPage.Posts {
		if post.ID != id {
			continue
		}
		m.selectedPost = id
		m.showForumPost(post)
		return
	}
	m.addEvent("所选帖子已移出当前页；↑↓ 重新选择，或 /thread <ID> 打开。")
}

func (m *Model) showForumPost(post forum.Post) {
	topic := post.Root.Topic
	if topic == "" {
		topic = "讨论"
	}
	lines := []string{fmt.Sprintf("作者：%s · 阶段：%s · 帖子 ID：%d", m.agentLabel(post.Root.AgentID, post.Root.AgentName), post.Root.Stage, post.ID), "", post.Root.Content}
	for _, reply := range post.Replies {
		lines = append(lines, "", fmt.Sprintf("回复 #%d · %s", reply.ID, m.agentLabel(reply.AgentID, reply.AgentName)), reply.Content)
	}
	m.showDetail(fmt.Sprintf("论坛帖子 #%d · %s", post.ID, topic), lines)
}

func (m *Model) revealSelectedPost() {
	_, right, height, _ := m.layout()
	if m.forumCache == nil {
		m.forumCache = &forumRenderCache{}
	}
	lines := m.forumContent(max(1, right-4))
	available := max(1, height-3)
	start := min(m.scroll[1], max(0, len(lines)-available))
	if m.follow[1] {
		start = 0
	}
	for _, header := range m.forumCache.headers {
		if header.id != m.selectedPost {
			continue
		}
		end := min(header.bodyEnd, header.end+3)
		if m.expandedPosts[header.id] || header.start < start || end-header.start >= available {
			start = header.start
		} else if end > start+available {
			start = end - available
		}
		m.scroll[1] = min(start, max(0, len(lines)-available))
		m.follow[1] = false
		return
	}
}

func (m *Model) clickForum(x, y int) {
	left, right, height, narrow := m.layout()
	if m.width < 20 || m.height < 7 || (narrow && m.focus != 1) {
		return
	}
	origin := 0
	if !narrow {
		origin = left + 1
	}
	// Global row 0 is the app header; pane border/title consume rows 1 and 2.
	if x < origin+2 || x >= origin+right-2 || y < 3 || y >= height {
		return
	}
	if m.forumCache == nil {
		m.forumCache = &forumRenderCache{}
	}
	lines := m.forumContent(max(1, right-4))
	start := min(max(0, m.scroll[1]), max(0, len(lines)-max(1, height-3)))
	if m.follow[1] {
		start = 0
	}
	row := start + y - 3
	for _, header := range m.forumCache.headers {
		if row >= header.start && row < header.end {
			m.focus = 1
			m.toggleForumPost(header.id)
			return
		}
	}
}

func (m *Model) changeForumPage(delta int) {
	if m.threadID != 0 {
		return
	}
	page := min(max(1, m.forumPage.TotalPages), max(1, m.forumPage.Page+delta))
	if page == m.forumPage.Page {
		return
	}
	m.forumPage.Page = page
	m.selectedPost = 0
	m.scroll[1] = 0
	m.follow[1] = true
	m.refreshForumPage()
}
func (m Model) scrollContent(pane int) ([]string, int) {
	left, right, height, _ := m.layout()
	available := max(1, height-3)
	if pane == 0 {
		pinned, lines := m.leftContent(max(1, left-4), height)
		available = max(1, available-len(pinned))
		return lines, max(0, len(lines)-available)
	}
	lines := m.forumContent(max(1, right-4))
	return lines, max(0, len(lines)-available)
}
func (m *Model) moveScroll(delta int) {
	_, limit := m.scrollContent(m.focus)
	position := m.scroll[m.focus]
	if m.follow[m.focus] {
		if m.focus == 0 {
			position = limit
		} else {
			position = 0
		}
	}
	m.scroll[m.focus] = min(limit, max(0, position+delta))
	if m.focus == 1 {
		m.follow[1] = m.scroll[1] == 0
	} else {
		m.follow[0] = delta > 0 && m.scroll[0] >= limit
	}
}
func viewport(lines []string, start, height int, follow bool) []string {
	height = max(0, height)
	if follow {
		start = max(0, len(lines)-height)
	}
	start = min(max(0, start), max(0, len(lines)-height))
	end := min(len(lines), start+height)
	out := make([]string, height)
	copy(out, lines[start:end])
	return out
}
func renderPane(title string, lines []string, width, height int, focused bool) string {
	inner := max(1, width-4)
	color := frameColor
	if focused {
		color = focusColor
	}
	border := lipgloss.NewStyle().Foreground(color)
	rows := make([]string, height)
	rows[0] = border.Render("╭" + strings.Repeat("─", width-2) + "╮")
	rows[height-1] = border.Render("╰" + strings.Repeat("─", width-2) + "╯")
	for i := 1; i < height-1; i++ {
		text := ""
		if i == 1 {
			text = title
		} else if i-2 < len(lines) {
			text = lines[i-2]
		}
		text = fitLine(text, inner)
		rows[i] = border.Render("│") + " " + text + strings.Repeat(" ", max(0, inner-runewidth.StringWidth(text))) + " " + border.Render("│")
	}
	return strings.Join(rows, "\n")
}
func (m Model) renderLeft(width, height int) string {
	pinned, lines := m.leftContent(max(1, width-4), height)
	available := max(0, height-3-len(pinned))
	content := append(pinned, viewport(lines, m.scroll[0], available, m.follow[0])...)
	return renderPane("团队进度 / 操作", content, width, height, m.focus == 0)
}
func (m Model) renderForum(width, height int) string {
	mode := "历史"
	if m.follow[1] {
		mode = "最新在前"
	}
	title := fmt.Sprintf("论坛 %d/%d页 · %d帖 · %s", max(1, m.forumPage.Page), max(1, m.forumPage.TotalPages), m.forumPage.TotalPosts, mode)
	if m.threadID != 0 {
		title = fmt.Sprintf("帖子 #%d · /forum 返回", m.threadID)
	}
	start := m.scroll[1]
	if m.follow[1] {
		start = 0
	}
	content := viewport(m.forumContent(max(1, width-4)), start, max(1, height-3), false)
	return renderPane(title, content, width, height, m.focus == 1)
}
func (m Model) View() string {
	state := phaseLabel(m.phase)
	if m.stopping {
		state += " / 正在停止"
	} else if m.busy {
		state += " / 运行中"
	}
	if m.saving {
		state += " / 保存中"
	}
	header := fitLine("代码审计 · "+state+" · "+m.workspace, m.width)
	if m.width < 20 || m.height < 7 {
		lines := []string{header, fitLine("请扩大终端；Esc 停止", m.width), fitLine(m.input.View(), m.width)}
		return m.overlayModal(strings.Join(lines[:min(len(lines), max(1, m.height))], "\n"))
	}
	left, right, height, narrow := m.layout()
	var panels string
	if narrow {
		if m.focus == 0 {
			panels = m.renderLeft(left, height)
		} else {
			panels = m.renderForum(right, height)
		}
		header = fitLine("审计 · "+state+" · Tab: 状态/论坛", m.width)
	} else {
		panels = lipgloss.JoinHorizontal(lipgloss.Top, m.renderLeft(left, height), " ", m.renderForum(right, height))
	}
	help := "Tab 面板 · ↑↓选帖/读正文 Enter展收 ←→翻页 · 点击标题 · PgUp/PgDn/滚轮滚动 · Esc暂停 · /list /help"
	if narrow {
		help = "↑↓选帖 Enter展收 ←→翻页 · Tab · Esc暂停 · /help"
	}
	return m.overlayModal(header + "\n" + panels + "\n" + fitLine(help, m.width) + "\n" + m.input.View())
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
