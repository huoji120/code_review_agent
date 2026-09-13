package forum

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func notice(t *testing.T, b *Board, id string, after int64, budget int) (notificationPage, int64, string) {
	t.Helper()
	text, next := b.Notification(id, after, budget)
	var p notificationPage
	if text != "" {
		limit := budget
		if limit < 1024 {
			limit = 1024
		}
		if limit > 2048 {
			limit = 2048
		}
		if len(text) > limit || !utf8.ValidString(text) {
			t.Fatalf("invalid bounded UTF-8 notice: %d", len(text))
		}
		if !strings.HasPrefix(text, notificationMarker) {
			t.Fatal("missing marker")
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(text, notificationMarker)), &p); err != nil {
			t.Fatal(err)
		}
		if p.NextID != next {
			t.Fatal("payload and cursor differ")
		}
	}
	return p, next, text
}

func TestNotificationThreadParticipantsAndNewRoots(t *testing.T) {
	b := testBoard()
	b.Register("d", "audit")
	root, _ := b.Post("a", "audit", "b", 0, "title", "new root")
	joined, _ := b.Post("b", "audit", "c", root.ID, "", "B joins")
	for _, id := range []string{"a", "b", "c", "d"} {
		p, _, _ := notice(t, b, id, 0, 1024)
		for _, e := range p.Entries {
			if e.LatestMessageID == joined.ID && id != "a" {
				t.Fatalf("nonparticipant %s received reply", id)
			}
		}
		if id == "c" && (len(p.Entries) != 1 || p.Entries[0].LatestMessageID != root.ID) {
			t.Fatal("new root must notify unrelated readers")
		}
	}
	last, _ := b.Post("d", "audit", "c", joined.ID, "", "D replies to the thread")
	for _, id := range []string{"a", "b"} {
		p, cursor, _ := notice(t, b, id, joined.ID, 1024)
		if len(p.Entries) != 1 || p.Entries[0].ThreadID != root.ID || p.Entries[0].LatestMessageID != last.ID || p.Entries[0].Topic != "title" {
			t.Fatalf("participant %s missed reply: %+v", id, p)
		}
		if _, _, text := notice(t, b, id, cursor, 1024); text != "" {
			t.Fatal("duplicate quiet-turn notice")
		}
	}
	for _, id := range []string{"c", "d"} {
		if _, cursor, text := notice(t, b, id, joined.ID, 1024); text != "" || cursor != last.ID {
			t.Fatal("outsider/self reply notice")
		}
	}
	// Merely reading does not subscribe C. D's later participation does not
	// retroactively deliver B's earlier reply.
	b.Call(context.Background(), "c", "audit", "forum_read", json.RawMessage(`{"all":true}`))
	p, _, _ := notice(t, b, "d", 0, 2048)
	if len(p.Entries) != 1 || p.Entries[0].LatestMessageID != root.ID {
		t.Fatal("participation backdated")
	}
	b.Register("user", "user")
	user, _ := b.Post("user", "user", "*", 0, "user title", "user broadcast")
	p, _, _ = notice(t, b, "c", last.ID, 1024)
	if len(p.Entries) != 1 || p.Entries[0].LatestMessageID != user.ID {
		t.Fatal("user broadcast omitted")
	}
}

func TestNotificationBurstByteBoundsAndRetentionRestore(t *testing.T) {
	b := testBoard()
	var want []int64
	for i := 0; i < 25; i++ {
		m, _ := b.Post("b", "audit", "*", 0, strings.Repeat("<题>", 60), strings.Repeat("<原文>\n", 1000))
		want = append(want, m.ID)
	}
	var cursor int64
	var got []int64
	pages := 0
	for {
		p, next, text := notice(t, b, "a", cursor, 1024)
		if text == "" {
			break
		}
		pages++
		if next <= cursor {
			t.Fatal("pagination stalled")
		}
		for _, e := range p.Entries {
			if !e.Truncated || len(e.Excerpt) > 256 {
				t.Fatal("full body leaked")
			}
			got = append(got, e.LatestMessageID)
		}
		cursor = next
		if !p.HasMore {
			break
		}
	}
	if pages < 2 || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("burst skipped/duplicated messages: %v", got)
	}

	b = testBoard()
	root, _ := b.Post("a", "audit", "*", 0, "retained thread", "root")
	joined, _ := b.Post("b", "audit", "*", root.ID, "", "join")
	for i := 0; i < maxMessages-2; i++ {
		b.Post("c", "audit", "*", 0, "", "filler")
	}
	late, _ := b.Post("b", "audit", "*", joined.ID, "", "retained reply")
	b.Post("c", "audit", "*", 0, "", "evict early join")
	messages, names, members := b.Checkpoint()
	if messages[0].ID <= joined.ID {
		t.Fatal("fixture did not evict original contributors")
	}
	restored := testBoard()
	if err := restored.Restore(messages); err != nil {
		t.Fatal(err)
	}
	if err := restored.RestoreNames(names); err != nil {
		t.Fatal(err)
	}
	if err := restored.RestoreParticipants(members); err != nil {
		t.Fatal(err)
	}
	last, _ := restored.Post("c", "audit", "*", late.ID, "", "after restore")
	p, _, _ := notice(t, restored, "a", last.ID-1, 2048)
	if len(p.Entries) != 1 || p.Entries[0].LatestMessageID != last.ID {
		t.Fatal("expired root author lost subscription")
	}
	p, _, _ = notice(t, restored, "a", 0, 1024)
	if !p.Gap || p.OldestID != messages[1].ID {
		t.Fatal("retention gap hidden")
	}
}

func TestForumNameRegistrationAndPersistence(t *testing.T) {
	b := New(0)
	b.Register("a", "audit")
	b.Register("b", "recon")
	if !strings.Contains(b.Call(context.Background(), "a", "audit", "forum_threads", json.RawMessage(`{}`)), `"ok":true`) {
		t.Fatal("unnamed registered worker cannot use forum")
	}
	if IsTool("forum_register") || !strings.Contains(b.Call(context.Background(), "a", "audit", "forum_register", json.RawMessage(`{"name":"Melon"}`)), `"ok":false`) {
		t.Fatal("model can still register names through audit tools")
	}
	prior, err := b.Post("a", "audit", "*", 0, "", "before naming")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "user", "SYSTEM", "coordinator", "b", "two\nlines", "zero\u200bwidth", strings.Repeat("名", 22)} {
		if err := b.RegisterName("a", name); err == nil {
			t.Fatalf("invalid/reserved name accepted %q", name)
		}
	}
	if err := b.RegisterName("a", "  Falcon  "); err != nil {
		t.Fatal(err)
	}
	posts := b.Messages()
	if posts[0].ID != prior.ID || posts[0].AgentName != "Falcon" || posts[0].Content != prior.Content {
		t.Fatal("name binding failed to backfill immutable post evidence")
	}
	if err := b.RegisterName("a", "Falcon"); err != nil {
		t.Fatal("same-name retry failed")
	}
	if err := b.RegisterName("a", "Other"); err == nil {
		t.Fatal("silent rename")
	}
	b.SetStatus("a", "completed")
	if err := b.RegisterName("b", "FALCON"); err == nil {
		t.Fatal("completed name reused")
	}
	m, _ := b.Post("a", "audit", "*", 0, "", "named message")
	if m.AgentName != "Falcon" {
		t.Fatal("post name snapshot missing")
	}
	messages, names, _ := b.Checkpoint()
	restored := New(0)
	restored.Register("a", "audit")
	restored.Register("b", "recon")
	if err := restored.Restore(messages); err != nil {
		t.Fatal(err)
	}
	if err := restored.RestoreNames(names); err != nil {
		t.Fatal(err)
	}
	if err := restored.RegisterName("b", "falcon"); err == nil {
		t.Fatal("restored name not reserved")
	}
}

func TestForumNumberedSearchPagesAndReplyExcerpt(t *testing.T) {
	b := testBoard()
	var first Message
	for i := 0; i < 61; i++ {
		m, _ := b.Post("a", "audit", "*", 0, fmt.Sprintf("title %d", i), "body")
		if i == 0 {
			first = m
		}
	}
	p := b.ListPosts(1, 0, "")
	if len(p.Posts) != 60 || p.TotalPages != 2 || !p.HasMore {
		t.Fatalf("default paging: %+v", p)
	}
	p = b.ListPosts(99, 0, "")
	if p.Page != 2 || len(p.Posts) != 1 || p.HasMore {
		t.Fatal("out-of-range page not clamped")
	}
	reply, _ := b.Post("b", "audit", "*", first.ID, "reply title", strings.Repeat("prefix ", 80)+"RareNeedle evidence")
	p = b.ListPosts(1, 0, "rareneedle")
	if p.TotalPosts != 1 || p.Posts[0].ID != first.ID {
		t.Fatal("reply body search failed")
	}
	p = b.ListPosts(1, 0, "REPLY TITLE")
	if p.TotalPosts != 1 {
		t.Fatal("reply title search failed")
	}
	var result struct {
		Posts []postSummary `json:"posts"`
	}
	if err := json.Unmarshal([]byte(b.Call(context.Background(), "c", "audit", "forum_threads", json.RawMessage(`{"query":"rareneedle"}`))), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Posts) != 1 || result.Posts[0].ExcerptMessageID != reply.ID || !strings.Contains(result.Posts[0].Excerpt, "RareNeedle") || !result.Posts[0].ExcerptTruncated {
		t.Fatal("search match missing from bounded excerpt")
	}
	if p := b.ListPosts(99, 0, "absent"); p.Page != 1 || p.TotalPosts != 0 || len(p.Posts) != 0 {
		t.Fatal("empty search not clamped")
	}
}

func TestNameBackfillHonorsRetentionBudget(t *testing.T) {
	b := New(0)
	b.Register("a", "audit")
	overhead := messageBytes(Message{AgentID: "a", Stage: "audit", To: "*"})
	remaining := maxRetainedBytes
	for remaining > overhead {
		size := remaining - overhead
		if size > maxContent {
			size = maxContent
		}
		if _, err := b.Post("a", "audit", "*", 0, "", strings.Repeat("x", size)); err != nil {
			t.Fatal(err)
		}
		remaining -= size + overhead
	}
	before := b.Messages()
	if err := b.RegisterName("a", "Melon"); err != nil {
		t.Fatal(err)
	}
	after := b.Messages()
	if after[0].ID <= before[0].ID {
		t.Fatal("name bytes did not evict oldest retained post")
	}
	total := 0
	for _, m := range after {
		if m.AgentName != "Melon" {
			t.Fatal("retained author was not backfilled")
		}
		total += messageBytes(m)
	}
	if total > maxRetainedBytes {
		t.Fatal("name backfill exceeded board retention budget")
	}
}

func TestAllMentionReachesNonparticipantsWithoutSelfNotification(t *testing.T) {
	for _, mention := range []string{"@all", "@全体成员", "@全体"} {
		t.Run(mention, func(t *testing.T) {
			b := testBoard()
			root, _ := b.Post("a", "audit", "*", 0, "review", "original")
			first, _ := b.Post("b", "audit", "a", root.ID, "", "ordinary reply")
			if _, cursor, text := notice(t, b, "c", root.ID, 1024); text != "" || cursor != first.ID {
				t.Fatal("ordinary reply notified nonparticipant")
			}
			broadcast, _ := b.Post("b", "audit", "a", first.ID, "", mention+" "+strings.Repeat("新证据<>", 300))
			for _, id := range []string{"a", "c"} {
				p, cursor, _ := notice(t, b, id, first.ID, 1024)
				if len(p.Entries) != 1 || p.Entries[0].LatestMessageID != broadcast.ID || p.Entries[0].ThreadID != root.ID || cursor != broadcast.ID {
					t.Fatalf("mention did not reach %s: %+v", id, p)
				}
				if _, _, text := notice(t, b, id, cursor, 1024); text != "" {
					t.Fatal("mention was delivered twice")
				}
			}
			if _, cursor, text := notice(t, b, "b", first.ID, 1024); text != "" || cursor != broadcast.ID {
				t.Fatal("mention notified its author")
			}
			// Receiving a mention does not permanently subscribe the outsider.
			plain, _ := b.Post("a", "audit", "*", first.ID, "", "ordinary follow-up")
			if _, cursor, text := notice(t, b, "c", broadcast.ID, 1024); text != "" || cursor != plain.ID {
				t.Fatal("mention silently subscribed nonparticipant")
			}
		})
	}
}
