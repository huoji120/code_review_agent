package forum

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// Post is a top-level topic with its replies, ordered by latest activity.
// RootMissing is explicit when retention removed the original post.
type Post struct {
	ID          int64
	Root        Message
	Replies     []Message
	UpdatedAt   time.Time
	LastID      int64
	RootMissing bool
}

func GroupPosts(messages []Message) []Post {
	groups := make(map[int64]int)
	parents := make(map[int64]int64)
	posts := make([]Post, 0)
	for _, m := range messages {
		id := m.ThreadID
		if id == 0 {
			id = m.ID
			if m.ReplyTo > 0 {
				id = parents[m.ReplyTo]
				if id == 0 {
					id = m.ReplyTo
				}
			}
		}
		parents[m.ID] = id
		index, ok := groups[id]
		if !ok {
			index = len(posts)
			groups[id] = index
			posts = append(posts, Post{ID: id, RootMissing: true})
		}
		p := &posts[index]
		if m.ID == id {
			p.Root = m
			p.RootMissing = false
		} else {
			p.Replies = append(p.Replies, m)
		}
		// Mirror state even when retention removed the original root.
		copyModeration(&p.Root, m)
		if m.CreatedAt.After(p.UpdatedAt) || (m.CreatedAt.Equal(p.UpdatedAt) && m.ID > p.LastID) {
			p.UpdatedAt = m.CreatedAt
			p.LastID = m.ID
		}
	}
	sort.Slice(posts, func(i, j int) bool {
		if posts[i].Root.Pinned != posts[j].Root.Pinned {
			return posts[i].Root.Pinned
		}
		if posts[i].UpdatedAt.Equal(posts[j].UpdatedAt) {
			return posts[i].LastID > posts[j].LastID
		}
		return posts[i].UpdatedAt.After(posts[j].UpdatedAt)
	})
	return posts
}

type postSummary struct {
	ID               int64           `json:"id"`
	Topic            string          `json:"topic"`
	AgentID          string          `json:"agent_id"`
	Stage            string          `json:"stage"`
	AgentName        string          `json:"agent_name,omitempty"`
	Excerpt          string          `json:"excerpt"`
	ExcerptTopic     string          `json:"excerpt_topic"`
	ExcerptMessageID int64           `json:"excerpt_message_id"`
	ExcerptTruncated bool            `json:"excerpt_truncated"`
	ReplyCount       int             `json:"reply_count"`
	LatestMessageID  int64           `json:"latest_message_id"`
	UpdatedAt        time.Time       `json:"updated_at"`
	RootMissing      bool            `json:"root_missing"`
	Closed           bool            `json:"closed"`
	Pinned           bool            `json:"pinned"`
	Announcement     bool            `json:"announcement"`
	ModerationReason string          `json:"moderation_reason,omitempty"`
	Matches          []searchHit     `json:"matches,omitempty"`
	ExcerptOffset    int             `json:"excerpt_offset"`
	ReadArgs         *searchReadArgs `json:"read_args,omitempty"`
}

const DefaultPageSize = 60

type PostPage struct {
	Posts      []Post
	Page       int
	PageSize   int
	TotalPosts int
	TotalPages int
	HasMore    bool
	Gap        bool
}

// ListPosts is the shared numbered-page/search implementation for UI and tools.
func (b *Board) ListPosts(page, pageSize int, search string) PostPage {
	return b.listPosts(page, pageSize, newPostSearch(search, "phrase"))
}

func (b *Board) listPosts(page, pageSize int, query postSearch) PostPage {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = DefaultPageSize
	}
	if pageSize > maxPage {
		pageSize = maxPage
	}
	b.mu.Lock()
	posts := GroupPosts(b.msgs)
	gap := len(b.msgs) > 0 && b.msgs[0].ID > 1
	b.mu.Unlock()
	if len(query.terms) > 0 {
		filtered := posts[:0]
		for _, p := range posts {
			if query.message(p).ID != 0 {
				filtered = append(filtered, p)
			}
		}
		posts = filtered
	}
	total := len(posts)
	pages := (total + pageSize - 1) / pageSize
	if pages > 0 && page > pages {
		page = pages
	}
	if pages == 0 {
		page = 1
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > total {
		end = total
	}
	return PostPage{Posts: posts[start:end], Page: page, PageSize: pageSize, TotalPosts: total, TotalPages: pages, HasMore: page < pages, Gap: gap}
}

func (b *Board) threadPage(raw json.RawMessage) string {
	var args struct {
		Page  int    `json:"page"`
		Limit int    `json:"limit"`
		Query string `json:"query"`
		Match string `json:"match"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil || args.Page < 0 || args.Limit < 0 {
		return failure("无效帖子分页参数；使用 page、limit、query、match")
	}
	if args.Match != "" && args.Match != "phrase" && args.Match != "all" && args.Match != "any" {
		return failure("match 必须为 phrase、all 或 any")
	}
	query := newPostSearch(args.Query, args.Match)
	if len(args.Query) > 512 || len(query.terms) > 8 {
		return failure("query 最多 512 UTF-8 字节；多关键词最多 8 项")
	}
	p := b.listPosts(args.Page, args.Limit, query)
	out := struct {
		OK         bool          `json:"ok"`
		Posts      []postSummary `json:"posts"`
		Page       int           `json:"page"`
		PageSize   int           `json:"page_size"`
		TotalPosts int           `json:"total_posts"`
		TotalPages int           `json:"total_pages"`
		HasMore    bool          `json:"has_more"`
		Gap        bool          `json:"gap"`
	}{OK: true, Posts: []postSummary{}, Page: p.Page, PageSize: p.PageSize, TotalPosts: p.TotalPosts, TotalPages: p.TotalPages, HasMore: p.HasMore, Gap: p.Gap}
	for _, post := range p.Posts {
		topic := post.Root.Topic
		if post.RootMissing {
			topic = "原帖已过期"
		}
		m := query.message(post)
		hits := query.hits(m)
		preview, offset := searchExcerpt(m.Content, "content", hits, 256)
		previewTopic, _ := searchExcerpt(m.Topic, "topic", hits, 128)
		var readArgs *searchReadArgs
		if len(hits) > 0 {
			readArgs = &searchReadArgs{MessageID: m.ID, Offset: offset, All: true}
		}
		out.Posts = append(out.Posts, postSummary{ID: post.ID, Topic: topic, AgentID: post.Root.AgentID, AgentName: post.Root.AgentName, Stage: post.Root.Stage, ReplyCount: len(post.Replies), LatestMessageID: post.LastID, UpdatedAt: post.UpdatedAt, RootMissing: post.RootMissing, Excerpt: preview, ExcerptTopic: previewTopic, ExcerptMessageID: m.ID, ExcerptTruncated: preview != m.Content || previewTopic != m.Topic, Closed: post.Root.Closed, Pinned: post.Root.Pinned, Announcement: post.Root.Announcement, ModerationReason: post.Root.ModerationReason, Matches: hits, ExcerptOffset: offset, ReadArgs: readArgs})
	}
	return marshal(out)
}
