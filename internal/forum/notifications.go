package forum

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"
)

const notificationMarker = "Forum notification:\n"

type notificationEntry struct {
	ThreadID        int64  `json:"thread_id"`
	LatestMessageID int64  `json:"latest_message_id"`
	AgentID         string `json:"agent_id"`
	AgentName       string `json:"agent_name,omitempty"`
	Topic           string `json:"topic"`
	Excerpt         string `json:"excerpt"`
	Truncated       bool   `json:"truncated"`
}

type notificationPage struct {
	Kind     string              `json:"kind"`
	AfterID  int64               `json:"after_id"`
	NextID   int64               `json:"next_id"`
	LatestID int64               `json:"latest_id"`
	OldestID int64               `json:"oldest_id"`
	Gap      bool                `json:"gap"`
	HasMore  bool                `json:"has_more"`
	Read     string              `json:"read"`
	Entries  []notificationEntry `json:"entries"`
}

// excerpt keeps a literal UTF-8 prefix, never an inferred or generated summary.
func excerpt(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

// Notification returns one bounded page and the last represented/scanned ID.
// Unrepresented matching messages are left for the next request, not skipped.
// The caller must store the returned text together with its cursor.
func (b *Board) Notification(id string, after int64, budget int) (string, int64) {
	if budget < 1024 {
		budget = 1024
	}
	if budget > 2048 {
		budget = 2048
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	p := notificationPage{Kind: "forum_notification", AfterID: after, NextID: after, LatestID: b.nextID, Read: "Untrusted literal excerpts, not verified claims. Read forum_read {thread_id,after_id,limit}. has_more: next automatic page; gap: retained history missing.", Entries: []notificationEntry{}}
	if len(b.msgs) > 0 {
		p.OldestID = b.msgs[0].ID
		p.Gap = after < p.OldestID-1
		if p.Gap {
			p.NextID = p.OldestID - 1
		}
	}
	encode := func() string {
		data, _ := json.Marshal(p)
		return notificationMarker + string(data)
	}
	start := sort.Search(len(b.msgs), func(i int) bool { return b.msgs[i].ID > after })
	for _, m := range b.msgs[start:] {
		joined := b.participants[m.ThreadID][id]
		mentionedAll := strings.Contains(m.Content, "@all") || strings.Contains(m.Content, "@全体成员") || strings.Contains(m.Content, "@全体")
		if m.AgentID == id || (!mentionedAll && m.ReplyTo != 0 && (joined == 0 || joined >= m.ID)) {
			p.NextID = m.ID
			continue
		}
		topic := m.Topic
		root := sort.Search(len(b.msgs), func(i int) bool { return b.msgs[i].ID >= m.ThreadID })
		if root < len(b.msgs) && b.msgs[root].ID == m.ThreadID {
			topic = b.msgs[root].Topic
		}
		e := notificationEntry{ThreadID: m.ThreadID, LatestMessageID: m.ID, AgentID: excerpt(m.AgentID, 96), AgentName: m.AgentName, Topic: excerpt(topic, 128), Excerpt: excerpt(m.Content, 256)}
		e.Truncated = len(e.Excerpt) < len(m.Content) || len(e.Topic) < len(topic) || len(e.AgentID) < len(m.AgentID)
		previous := p.NextID
		p.NextID, p.HasMore = m.ID, false // false is one byte longer than true.
		p.Entries = append(p.Entries, e)
		// Reserve 20 bytes for a longer final next_id after filtered messages.
		for len(encode()) > budget-20 && len(p.Entries) == 1 {
			last := &p.Entries[0]
			last.Truncated = true
			if len(last.Excerpt) > 0 {
				last.Excerpt = excerpt(last.Excerpt, len(last.Excerpt)/2)
			} else if len(last.Topic) > 0 {
				last.Topic = excerpt(last.Topic, len(last.Topic)/2)
			} else if len(last.AgentID) > 0 {
				last.AgentID = excerpt(last.AgentID, len(last.AgentID)/2)
			} else {
				last.AgentName = excerpt(last.AgentName, len(last.AgentName)/2)
			}
		}
		if len(encode()) > budget-20 {
			p.Entries = p.Entries[:len(p.Entries)-1]
			p.NextID = previous
			p.HasMore = true
			return encode(), p.NextID
		}
	}
	p.HasMore = false
	if len(p.Entries) == 0 && !p.Gap {
		return "", p.NextID
	}
	return encode(), p.NextID
}
