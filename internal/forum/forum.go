package forum

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxContent       = 16 * 1024
	maxTopic         = 512
	maxMessages      = 4096
	maxRetainedBytes = 8 * 1024 * 1024
	maxPage          = 64
	maxPageBytes     = 4096
	maxWait          = 120 * time.Second
)

type Message struct {
	ID        int64     `json:"id"`
	ThreadID  int64     `json:"thread_id"`
	AgentID   string    `json:"agent_id"`
	AgentName string    `json:"agent_name,omitempty"`
	Stage     string    `json:"stage"`
	To        string    `json:"to"`
	ReplyTo   int64     `json:"reply_to"`
	Topic     string    `json:"topic"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type participant struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Stage  string `json:"stage"`
	Status string `json:"status"`
}

type Board struct {
	mu           sync.Mutex
	defaultWait  time.Duration
	nextID       int64
	msgs         []Message
	bytes        int
	agents       map[string]participant
	participants map[int64]map[string]int64
	threadCounts map[int64]int
	notify       chan struct{}
	hook         func(Message)
}

func New(defaultWait time.Duration) *Board {
	if defaultWait <= 0 {
		defaultWait = 30 * time.Second
	}
	if defaultWait > maxWait {
		defaultWait = maxWait
	}
	return &Board{defaultWait: defaultWait, agents: make(map[string]participant), participants: make(map[int64]map[string]int64), threadCounts: make(map[int64]int), notify: make(chan struct{})}
}
func (b *Board) Register(id, stage string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id == "" {
		return
	}
	if _, exists := b.agents[id]; exists {
		return
	}
	b.agents[id] = participant{ID: id, Stage: stage, Status: "running"}
	b.signalLocked()
}
func (b *Board) SetStatus(id, status string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if a, ok := b.agents[id]; ok {
		a.Status = status
		b.agents[id] = a
		b.signalLocked()
	}
}
func (b *Board) SetOnPost(fn func(Message)) { b.mu.Lock(); b.hook = fn; b.mu.Unlock() }
func (b *Board) signalLocked()              { close(b.notify); b.notify = make(chan struct{}) }
func IsTool(name string) bool {
	switch name {
	case "forum_post", "forum_threads", "forum_read", "forum_wait", "forum_roster":
		return true
	}
	return false
}
func ToolPrompt() string {
	return `# 交流论坛工具
- forum_post {to,reply_to,topic,content}：发帖/回复。to 为空或 "*" 表示广播，或填 forum_roster 中的 Agent ID；reply_to 可选，为已有帖子 ID。content 必填、最多16KiB；topic 最多512字节。返回 message.id，可作为等待该帖后续回复的 after_id。发送者由服务端确定，禁止传 agent_id 伪造身份。
- forum_threads {page,limit,query}：按最新活动倒序的编号分页帖子列表。page 默认1；limit 默认60、最大64。query 可选，对原标题/正文及所有回复标题/正文进行忽略大小写的字面子串搜索。返回原文摘录、page、total_pages、has_more、gap；用 forum_read 的 thread_id 打开帖子，用 forum_post 的 reply_to 回复。自动通知只推送其他作者的新帖，以及你发帖/回复参与过的线程的新回复；to 不影响自动通知参与规则。
- forum_read {after_id,limit,all,thread_id,from}：按ID升序发现帖子，仅返回每条正文的有界原文摘录（最多512字节）。after_id 默认0；limit 默认16、最大64；整个 JSON 页最多4096字节，可能少于指定条数。顶层 next_id / has_more 是消息列表游标；消息内 total_bytes / next_offset / has_more 是该条正文的字节进度，不要混用。默认只读自己发出的、发给自己的和广播消息；all:true 可读整个公开论坛。thread_id 只读该线程及递归回复，from 限制发帖人；gap 表示早期帖子已淘汰，不是完整历史。
- forum_read {message_id,offset,max_bytes,all,thread_id,from}：按固定消息 ID 分页读取需要的长正文。offset 是 UTF-8 字节位置，默认0，必须位于字符边界；max_bytes 默认1024、最大4096，实际内容可能为满足 JSON 字节上限而更短。返回 message 中该页原文，以及顶层 offset / next_offset / has_more / total_bytes。只对相关正文沿同一 message_id 的 next_offset 继续读取，不要因为无关内容很长就读完整个 buffer。过滤规则不变；已淘汰或不可见的消息明确失败。不要把消息 next_id 用作正文 offset，也不要把正文偏移套到另一条消息。
- forum_wait {after_id,limit,all,thread_id,from,timeout_seconds}：与消息发现模式相同，只返回正文摘录；已有匹配立即返回，否则事件唤醒等待。timeout_seconds 默认配置值、最大120秒；返回 timed_out、cancelled、peers_done 和 roster，roster_has_more 表示名册因页上限被截短（完整名册按需 forum_roster）。先发问，再把该帖id用作after_id等待回复；超时或对方已完成时独立推进，不要无限等待。正文分页使用 forum_read 的 message_id 模式。
- forum_roster {}：查看 Agent ID、阶段和 running/waiting/completed/failed/cancelled/pending 状态。
论坛是公开交流而非私信；to 只影响默认收件过滤。保留最近4096条且最多8MiB，完整源码请用文件工具而不是粘贴到论坛。`
}

func messageBytes(m Message) int {
	return len(m.Content) + len(m.Topic) + len(m.AgentID) + len(m.AgentName) + len(m.Stage) + len(m.To) + 128
}
func (b *Board) trimLocked() {
	for len(b.msgs) > maxMessages || b.bytes > maxRetainedBytes {
		thread := b.msgs[0].ThreadID
		b.threadCounts[thread]--
		if b.threadCounts[thread] == 0 {
			delete(b.threadCounts, thread)
			delete(b.participants, thread)
		}
		b.bytes -= messageBytes(b.msgs[0])
		b.msgs[0] = Message{}
		b.msgs = b.msgs[1:]
	}
}
func (b *Board) hasIDLocked(id int64) bool {
	i := sort.Search(len(b.msgs), func(i int) bool { return b.msgs[i].ID >= id })
	return i < len(b.msgs) && b.msgs[i].ID == id
}
func (b *Board) Post(agentID, stage, to string, replyTo int64, topic, content string) (Message, error) {
	if strings.TrimSpace(content) == "" || len(content) > maxContent {
		return Message{}, fmt.Errorf("content 必须非空且不超过 %d 字节", maxContent)
	}
	if len(topic) > maxTopic {
		return Message{}, fmt.Errorf("topic 超过 %d 字节", maxTopic)
	}
	b.mu.Lock()
	a, ok := b.agents[agentID]
	if !ok || (stage != "" && stage != a.Stage) {
		b.mu.Unlock()
		return Message{}, fmt.Errorf("未注册的 Agent 或阶段不匹配")
	}
	if to != "" && to != "*" {
		if _, ok := b.agents[to]; !ok {
			b.mu.Unlock()
			return Message{}, fmt.Errorf("未知接收 Agent")
		}
	}
	if replyTo < 0 || (replyTo > 0 && !b.hasIDLocked(replyTo)) {
		b.mu.Unlock()
		return Message{}, fmt.Errorf("回复目标不存在或已淘汰")
	}
	b.nextID++
	threadID := b.nextID
	if replyTo > 0 {
		index := sort.Search(len(b.msgs), func(i int) bool { return b.msgs[i].ID >= replyTo })
		threadID = b.msgs[index].ThreadID
	}
	m := Message{ID: b.nextID, ThreadID: threadID, AgentID: agentID, AgentName: a.Name, Stage: a.Stage, To: to, ReplyTo: replyTo, Topic: topic, Content: content, CreatedAt: time.Now().UTC()}
	b.msgs = append(b.msgs, m)
	b.joinThreadLocked(m)
	b.threadCounts[m.ThreadID]++
	b.bytes += messageBytes(m)
	b.trimLocked()
	hook := b.hook
	b.signalLocked()
	b.mu.Unlock()
	// Concurrent hooks may finish out of order; consumers order by immutable ID.
	if hook != nil {
		hook(m)
	}
	return m, nil
}
func (b *Board) Messages() []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Message(nil), b.msgs...)
}
func (b *Board) Restore(messages []Message) error {
	var previous int64
	for _, m := range messages {
		if m.ID <= previous || m.AgentID == "" || m.Stage == "" || strings.TrimSpace(m.Content) == "" || len(m.Content) > maxContent || len(m.Topic) > maxTopic || m.ReplyTo < 0 || m.ReplyTo >= m.ID || m.ThreadID < 0 || m.ThreadID > m.ID {
			return fmt.Errorf("无效的论坛会话记录")
		}
		if m.AgentName != "" && !validName(m.AgentName) {
			return fmt.Errorf("无效的论坛作者名字")
		}
		previous = m.ID
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = nil
	b.bytes = 0
	b.participants = make(map[int64]map[string]int64)
	b.threadCounts = make(map[int64]int)
	threadIDs := make(map[int64]int64)
	for _, m := range messages {
		if m.ThreadID == 0 {
			m.ThreadID = m.ID
			if m.ReplyTo > 0 {
				m.ThreadID = threadIDs[m.ReplyTo]
				if m.ThreadID == 0 {
					m.ThreadID = m.ReplyTo
				}
			}
		}
		threadIDs[m.ID] = m.ThreadID
		b.msgs = append(b.msgs, m)
		b.joinThreadLocked(m)
		b.threadCounts[m.ThreadID]++
		b.bytes += messageBytes(m)
		b.trimLocked()
	}
	if previous > b.nextID {
		b.nextID = previous
	}
	b.signalLocked()
	return nil
}

type query struct {
	AfterID        int64   `json:"after_id"`
	Limit          int     `json:"limit"`
	All            bool    `json:"all"`
	ThreadID       int64   `json:"thread_id"`
	From           string  `json:"from"`
	TimeoutSeconds float64 `json:"timeout_seconds"`
	MessageID      *int64  `json:"message_id"`
	Offset         int     `json:"offset"`
	MaxBytes       int     `json:"max_bytes"`
}
type page struct {
	OK            bool          `json:"ok"`
	Error         string        `json:"error,omitempty"`
	Messages      []readMessage `json:"messages"`
	NextID        int64         `json:"next_id"`
	LatestID      int64         `json:"latest_id"`
	OldestID      int64         `json:"oldest_id"`
	Gap           bool          `json:"gap"`
	HasMore       bool          `json:"has_more"`
	TimedOut      bool          `json:"timed_out"`
	Cancelled     bool          `json:"cancelled"`
	PeersDone     bool          `json:"peers_done"`
	Roster        []participant `json:"roster,omitempty"`
	RosterHasMore bool          `json:"roster_has_more,omitempty"`
}

// Read projections never mutate the canonical message retained by the board/UI.
type readMessage struct {
	Message
	TotalBytes int  `json:"total_bytes"`
	NextOffset int  `json:"next_offset"`
	HasMore    bool `json:"has_more"`
}

type bodyPage struct {
	OK         bool    `json:"ok"`
	Message    Message `json:"message"`
	Offset     int     `json:"offset"`
	NextOffset int     `json:"next_offset"`
	TotalBytes int     `json:"total_bytes"`
	HasMore    bool    `json:"has_more"`
}

func encodeReadPage(p page) string {
	for {
		encoded := marshal(p)
		if len(encoded) <= maxPageBytes {
			return encoded
		}
		if len(p.Roster) > 0 {
			p.Roster = p.Roster[:len(p.Roster)/2]
			p.RosterHasMore = true
		} else if len(p.Messages) > 1 {
			p.Messages = p.Messages[:len(p.Messages)-1]
			p.NextID = p.Messages[len(p.Messages)-1].ID
			p.HasMore = true
		} else if len(p.Messages) == 1 && len(p.Messages[0].Content) > 0 {
			m := &p.Messages[0]
			m.Content = excerpt(m.Content, len(m.Content)/2)
			m.NextOffset, m.HasMore = len(m.Content), len(m.Content) < m.TotalBytes
		} else if len(p.Error) > 128 {
			p.Error = excerpt(p.Error, 128)
		} else {
			return failure("forum message metadata exceeds read page budget")
		}
	}
}

func matchesRead(m Message, id string, q query, thread map[int64]bool) bool {
	inThread := q.ThreadID == 0 || m.ThreadID == q.ThreadID || thread[m.ID] || thread[m.ReplyTo]
	if q.ThreadID > 0 && inThread {
		thread[m.ID] = true
	}
	return inThread && (q.From == "" || m.AgentID == q.From) &&
		(q.All || m.AgentID == id || m.To == "" || m.To == "*" || m.To == id)
}

func (b *Board) readBodyLocked(id string, q query) string {
	thread := map[int64]bool{q.ThreadID: true}
	for _, m := range b.msgs {
		visible := matchesRead(m, id, q, thread)
		if m.ID != *q.MessageID || !visible {
			continue
		}
		if !utf8.ValidString(m.Content) || q.Offset > len(m.Content) || (q.Offset < len(m.Content) && !utf8.RuneStart(m.Content[q.Offset])) {
			return failure("offset must be a valid UTF-8 byte boundary within the retained message")
		}
		p := bodyPage{OK: true, Message: m, Offset: q.Offset, TotalBytes: len(m.Content)}
		p.Message.Content = excerpt(m.Content[q.Offset:], q.MaxBytes)
		for {
			p.NextOffset = q.Offset + len(p.Message.Content)
			p.HasMore = p.NextOffset < p.TotalBytes
			if p.HasMore && p.NextOffset == q.Offset {
				return failure("max_bytes or JSON page budget is too small for the next UTF-8 character")
			}
			encoded := marshal(p)
			if len(encoded) <= maxPageBytes {
				return encoded
			}
			if p.Message.Content == "" {
				return failure("forum message metadata exceeds read page budget")
			}
			p.Message.Content = excerpt(p.Message.Content, len(p.Message.Content)/2)
		}
	}
	return failure("message_id is missing, expired, or excluded by visibility filters")
}

func marshal(v any) string { data, _ := json.Marshal(v); return string(data) }
func failure(message string) string {
	return marshal(struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}{false, message})
}
func (b *Board) rosterLocked() []participant {
	out := make([]participant, 0, len(b.agents))
	for _, a := range b.agents {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func terminal(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}
func (b *Board) readLocked(id string, q query) page {
	p := page{OK: true, Messages: []readMessage{}, NextID: q.AfterID, LatestID: b.nextID}
	if len(b.msgs) > 0 {
		p.OldestID = b.msgs[0].ID
		p.Gap = q.AfterID < p.OldestID-1
	}
	thread := map[int64]bool{}
	if q.ThreadID > 0 {
		thread[q.ThreadID] = true
		if !b.hasIDLocked(q.ThreadID) {
			p.Gap = true
		}
	}
	for _, m := range b.msgs {
		if !matchesRead(m, id, q, thread) || m.ID <= q.AfterID {
			continue
		}
		projection := readMessage{Message: m, TotalBytes: len(m.Content)}
		projection.Content = excerpt(m.Content, 512)
		projection.NextOffset = len(projection.Content)
		projection.HasMore = projection.NextOffset < projection.TotalBytes
		if len(p.Messages) >= q.Limit {
			p.HasMore = true
			break
		}
		p.Messages = append(p.Messages, projection)
		if len(p.Messages) > 1 && len(marshal(p)) > maxPageBytes-64 {
			p.Messages = p.Messages[:len(p.Messages)-1]
			p.HasMore = true
			break
		}
		p.NextID = m.ID
	}
	// Cursor advances through filtered-out messages only if all matches delivered.
	if !p.HasMore && q.AfterID <= b.nextID {
		p.NextID = b.nextID
	}
	return p
}
func (b *Board) peersDoneLocked(id string, q query) bool {
	if q.From != "" {
		a, ok := b.agents[q.From]
		return ok && terminal(a.Status)
	}
	stage := b.agents[id].Stage
	for other, a := range b.agents {
		if other != id && a.Stage == stage && !terminal(a.Status) {
			return false
		}
	}
	return true
}
func (b *Board) Call(ctx context.Context, id, stage, name string, raw json.RawMessage) string {
	if !IsTool(name) {
		return failure("未知论坛工具")
	}
	b.mu.Lock()
	a, known := b.agents[id]
	b.mu.Unlock()
	if !known || a.Stage != stage {
		return failure("未注册的 Agent 或阶段不匹配")
	}
	if name == "forum_threads" {
		return b.threadPage(raw)
	}
	if name == "forum_post" {
		var args struct {
			To      string `json:"to"`
			ReplyTo int64  `json:"reply_to"`
			Topic   string `json:"topic"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return failure("无效帖子参数")
		}
		m, err := b.Post(id, stage, args.To, args.ReplyTo, args.Topic, args.Content)
		if err != nil {
			return failure(err.Error())
		}
		return marshal(struct {
			OK      bool    `json:"ok"`
			Message Message `json:"message"`
		}{true, m})
	}
	if name == "forum_roster" {
		b.mu.Lock()
		roster := b.rosterLocked()
		b.mu.Unlock()
		return marshal(struct {
			OK     bool          `json:"ok"`
			Agents []participant `json:"agents"`
		}{true, roster})
	}
	var q query
	if err := json.Unmarshal(raw, &q); err != nil || q.AfterID < 0 || q.ThreadID < 0 || q.Limit < 0 || q.TimeoutSeconds < 0 || q.Offset < 0 || q.MaxBytes < 0 || (q.MessageID != nil && *q.MessageID <= 0) {
		return failure("无效游标、条数、正文偏移或等待时间")
	}
	if q.MessageID != nil && (name != "forum_read" || q.AfterID != 0) {
		return failure("message_id body paging requires forum_read and cannot use after_id")
	}
	if q.MessageID == nil && (q.Offset != 0 || q.MaxBytes != 0) {
		return failure("正文 offset / max_bytes 必须同时指定 message_id")
	}
	if q.MaxBytes == 0 {
		q.MaxBytes = 1024
	}
	if q.MaxBytes > 4096 {
		q.MaxBytes = 4096
	}
	if q.Limit == 0 {
		q.Limit = 16
	}
	if q.Limit > maxPage {
		q.Limit = maxPage
	}
	b.mu.Lock()
	_, known = b.agents[q.From]
	latest := b.nextID
	b.mu.Unlock()
	if q.From != "" && !known {
		return failure("未知发帖人过滤条件")
	}
	if q.AfterID > latest {
		return failure("after_id 超过当前论坛游标")
	}
	if name == "forum_read" {
		b.mu.Lock()
		if q.MessageID != nil {
			body := b.readBodyLocked(id, q)
			b.mu.Unlock()
			return body
		}
		p := b.readLocked(id, q)
		b.mu.Unlock()
		return encodeReadPage(p)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	duration := b.defaultWait
	if q.TimeoutSeconds > 120 {
		q.TimeoutSeconds = 120
	}
	if q.TimeoutSeconds > 0 {
		duration = time.Duration(q.TimeoutSeconds * float64(time.Second))
		if duration < time.Millisecond {
			duration = time.Millisecond
		}
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		b.mu.Lock()
		p := b.readLocked(id, q)
		p.PeersDone = b.peersDoneLocked(id, q)
		wake := b.notify
		if p.PeersDone {
			p.Roster = b.rosterLocked()
		}
		b.mu.Unlock()
		if ctx.Err() != nil {
			p.OK = false
			p.Cancelled = true
			p.Error = ctx.Err().Error()
			return encodeReadPage(p)
		}
		if len(p.Messages) > 0 || p.Gap || p.PeersDone {
			return encodeReadPage(p)
		}
		select {
		case <-ctx.Done():
			p.OK = false
			p.Cancelled = true
			p.Error = ctx.Err().Error()
			return encodeReadPage(p)
		case <-timer.C:
			b.mu.Lock()
			p = b.readLocked(id, q)
			p.Roster = b.rosterLocked()
			p.PeersDone = b.peersDoneLocked(id, q)
			b.mu.Unlock()
			p.TimedOut = len(p.Messages) == 0
			return encodeReadPage(p)
		case <-wake:
		}
	}
}
