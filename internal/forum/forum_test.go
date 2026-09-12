package forum

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func callPage(t *testing.T, b *Board, id, name, args string) page {
	t.Helper()
	var p page
	if err := json.Unmarshal([]byte(b.Call(context.Background(), id, "audit", name, json.RawMessage(args))), &p); err != nil {
		t.Fatal(err)
	}
	return p
}
func testBoard() *Board {
	b := New(20 * time.Millisecond)
	b.Register("a", "audit")
	b.Register("b", "audit")
	b.Register("c", "audit")
	for _, id := range []string{"a", "b", "c"} {
		if err := b.RegisterName(id, "tester-"+id); err != nil {
			panic(err)
		}
	}
	return b
}

func TestForumWaitReplyCancellationAndTimeout(t *testing.T) {
	b := testBoard()
	question, err := b.Post("a", "audit", "b", 0, "问题", "请核实入口")
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan string, 1)
	go func() {
		result <- b.Call(context.Background(), "a", "audit", "forum_wait", json.RawMessage(fmt.Sprintf(`{"after_id":%d,"from":"b","timeout_seconds":1}`, question.ID)))
	}()
	reply, err := b.Post("b", "audit", "a", question.ID, "回复", "入口需要登录")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-result:
		var p page
		json.Unmarshal([]byte(raw), &p)
		if len(p.Messages) != 1 || p.Messages[0].ID != reply.ID || p.TimedOut {
			t.Fatalf("reply not delivered: %s", raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lost notification")
	}
	p := callPage(t, b, "a", "forum_wait", fmt.Sprintf(`{"after_id":%d,"from":"b","timeout_seconds":0.01}`, reply.ID))
	if !p.TimedOut || p.Cancelled {
		t.Fatalf("timeout not observable: %+v", p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var cancelled page
	json.Unmarshal([]byte(b.Call(ctx, "a", "audit", "forum_wait", json.RawMessage(fmt.Sprintf(`{"after_id":%d}`, reply.ID)))), &cancelled)
	if !cancelled.Cancelled || cancelled.OK {
		t.Fatal("cancellation ignored")
	}
	b.SetStatus("b", "completed")
	p = callPage(t, b, "a", "forum_wait", fmt.Sprintf(`{"after_id":%d,"from":"b"}`, reply.ID))
	if !p.PeersDone || p.TimedOut {
		t.Fatal("completed recipient not observable")
	}
}

func TestForumPublicThreadsAndPagination(t *testing.T) {
	b := testBoard()
	root, _ := b.Post("a", "audit", "b", 0, "root", "one")
	reply, _ := b.Post("b", "audit", "a", root.ID, "reply", "two")
	last, _ := b.Post("a", "audit", "b", reply.ID, "nested", "three")
	p := callPage(t, b, "c", "forum_read", `{"limit":1}`)
	if len(p.Messages) != 0 {
		t.Fatal("unrelated directed traffic entered inbox")
	}
	p = callPage(t, b, "c", "forum_read", fmt.Sprintf(`{"limit":2,"all":true,"thread_id":%d}`, root.ID))
	if len(p.Messages) != 2 || !p.HasMore || p.NextID != reply.ID {
		t.Fatalf("bad page %+v", p)
	}
	p = callPage(t, b, "c", "forum_read", fmt.Sprintf(`{"after_id":%d,"all":true,"thread_id":%d}`, p.NextID, root.ID))
	if len(p.Messages) != 1 || p.Messages[0].ID != last.ID {
		t.Fatal("nested reply omitted")
	}
	if !strings.Contains(b.Call(context.Background(), "fake", "audit", "forum_read", json.RawMessage(`{}`)), `"ok":false`) {
		t.Fatal("unregistered reader accepted")
	}
	if _, err := b.Post("a", "recon", "*", 0, "", "forged stage"); err == nil {
		t.Fatal("stage spoof accepted")
	}
}

func TestForumConcurrentIDsRetentionAndRestore(t *testing.T) {
	b := testBoard()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 32; j++ {
				if _, err := b.Post("a", "audit", "*", 0, "", "并发记录"); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	messages := b.Messages()
	for i, m := range messages {
		if m.ID != int64(i+1) {
			t.Fatal("IDs out of order")
		}
	}
	for i := 0; i < maxMessages; i++ {
		b.Post("b", "audit", "*", 0, "", "retention")
	}
	p := callPage(t, b, "a", "forum_read", `{}`)
	if !p.Gap || p.OldestID <= 1 {
		t.Fatal("retention gap hidden")
	}
	restored := testBoard()
	if err := restored.Restore(b.Messages()); err != nil {
		t.Fatal(err)
	}
	m, err := restored.Post("c", "audit", "*", 0, "", "after restore")
	if err != nil {
		t.Fatal(err)
	}
	if m.ID <= p.LatestID {
		t.Fatal("restored ID reused")
	}
}

func TestForumPostsSortByLatestActivityAndKeepReplies(t *testing.T) {
	b := testBoard()
	first, _ := b.Post("a", "audit", "*", 0, "first", "first body")
	second, _ := b.Post("b", "audit", "*", 0, "second", "second body")
	reply, _ := b.Post("c", "audit", "a", first.ID, "", "reply to first")
	nested, _ := b.Post("a", "audit", "c", reply.ID, "", "reply to reply")
	posts := GroupPosts(b.Messages())
	if len(posts) != 2 || posts[0].ID != first.ID || posts[1].ID != second.ID || len(posts[0].Replies) != 2 {
		t.Fatalf("post activity ordering or grouping failed: %+v", posts)
	}
	if nested.ThreadID != first.ID || posts[0].LastID != nested.ID {
		t.Fatal("recursive reply escaped post")
	}
	var result struct {
		Posts   []postSummary `json:"posts"`
		Page    int           `json:"page"`
		HasMore bool          `json:"has_more"`
	}
	json.Unmarshal([]byte(b.Call(context.Background(), "a", "audit", "forum_threads", json.RawMessage(`{"limit":1}`))), &result)
	if len(result.Posts) != 1 || result.Posts[0].ID != first.ID || !result.HasMore {
		t.Fatal("latest post index missing")
	}
	args := fmt.Sprintf(`{"limit":1,"page":%d}`, result.Page+1)
	json.Unmarshal([]byte(b.Call(context.Background(), "a", "audit", "forum_threads", json.RawMessage(args))), &result)
	if len(result.Posts) != 1 || result.Posts[0].ID != second.ID {
		t.Fatal("older post page skipped")
	}
	restored := testBoard()
	if err := restored.Restore(b.Messages()[2:]); err != nil {
		t.Fatal(err)
	}
	posts = GroupPosts(restored.Messages())
	if len(posts) != 1 || posts[0].ID != first.ID || !posts[0].RootMissing {
		t.Fatal("retention fabricated or split an original post")
	}
}

func TestForumNumberedPagesFollowActivityTime(t *testing.T) {
	b := testBoard()
	base := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	messages := []Message{
		{ID: 1, AgentID: "a", Stage: "audit", Content: "oldest", CreatedAt: base},
		{ID: 2, AgentID: "b", Stage: "audit", Content: "latest", CreatedAt: base.Add(2 * time.Second)},
		{ID: 3, AgentID: "c", Stage: "audit", Content: "clock corrected", CreatedAt: base.Add(time.Second)},
	}
	if err := b.Restore(messages); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Posts []postSummary `json:"posts"`
		Page  int           `json:"page"`
	}
	for _, want := range []int64{2, 3, 1} {
		args := fmt.Sprintf(`{"limit":1,"page":%d}`, result.Page+1)
		if err := json.Unmarshal([]byte(b.Call(context.Background(), "a", "audit", "forum_threads", json.RawMessage(args))), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Posts) != 1 || result.Posts[0].ID != want {
			t.Fatalf("time-ordered page skipped: want %d, got %+v", want, result.Posts)
		}
	}
}
