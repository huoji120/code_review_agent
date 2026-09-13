package forum

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSearchLocationsReadOriginalUnicodeReply(t *testing.T) {
	b := testBoard()
	root, err := b.Post("a", "audit", "*", 0, "root", "not the match")
	if err != nil {
		t.Fatal(err)
	}
	prefix := strings.Repeat("旧证据\n", 80) + "说明：İK中文 "
	body := prefix + "Needle\nTAIL"
	reply, err := b.Post("b", "audit", "a", root.ID, "reply title", body)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		OK    bool          `json:"ok"`
		Posts []postSummary `json:"posts"`
	}
	raw := b.Call(context.Background(), "c", "audit", "forum_threads", json.RawMessage(`{"query":"K中文 NEEDLE","match":"all"}`))
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || len(result.Posts) != 1 {
		t.Fatal(raw)
	}
	p := result.Posts[0]
	if p.ExcerptMessageID != reply.ID || len(p.Matches) != 2 || p.ReadArgs == nil {
		t.Fatal(raw)
	}
	first := p.Matches[0]
	second := p.Matches[1]
	if first.Offset != strings.Index(body, "K") || first.EndOffset != strings.Index(body, "K")+len("K中文") || second.Offset != len(prefix) || second.EndOffset != len(prefix)+len("Needle") || second.Line != 81 || second.Column != 9 {
		t.Fatalf("original location mismatch: %+v %+v", first, second)
	}
	args, _ := json.Marshal(p.ReadArgs)
	raw = b.Call(context.Background(), "c", "audit", "forum_read", args)
	var read bodyPage
	if err := json.Unmarshal([]byte(raw), &read); err != nil {
		t.Fatal(err)
	}
	if !read.OK || read.Message.ID != reply.ID || !utf8.ValidString(read.Message.Content) || read.Message.Content != body[read.Offset:read.NextOffset] || !strings.Contains(read.Message.Content, "Needle") {
		t.Fatal(raw)
	}
}

func TestSearchTermsCannotCombineDifferentReplies(t *testing.T) {
	b := testBoard()
	root, _ := b.Post("a", "audit", "*", 0, "title", "alpha")
	b.Post("b", "audit", "*", root.ID, "reply", "beta")
	same, _ := b.Post("a", "audit", "*", 0, "alpha title", "beta evidence")
	for _, c := range []struct {
		mode  string
		total int
	}{{"all", 1}, {"any", 2}, {"phrase", 0}} {
		raw := b.Call(context.Background(), "c", "audit", "forum_threads", json.RawMessage(`{"query":"alpha beta","match":"`+c.mode+`"}`))
		var p struct {
			OK    bool          `json:"ok"`
			Posts []postSummary `json:"posts"`
			Total int           `json:"total_posts"`
		}
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		if !p.OK || p.Total != c.total {
			t.Fatal(raw)
		}
		if c.mode == "all" && (p.Posts[0].ID != same.ID || p.Posts[0].Matches[0].Field != "topic" || p.Posts[0].Matches[1].Field != "content") {
			t.Fatal(raw)
		}
	}
	for _, rawArgs := range []string{`{"query":"x","match":"regex"}`, `{"query":"a b c d e f g h i","match":"all"}`, `{"query":"` + strings.Repeat("中", 180) + `"}`} {
		var p struct {
			OK bool `json:"ok"`
		}
		raw := b.Call(context.Background(), "c", "audit", "forum_threads", json.RawMessage(rawArgs))
		if err := json.Unmarshal([]byte(raw), &p); err != nil || p.OK {
			t.Fatal(raw)
		}
	}
}
