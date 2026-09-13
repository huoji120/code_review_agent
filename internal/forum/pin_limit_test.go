package forum

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func TestThreePinSlotsConcurrentAdmissionAndReplyBump(t *testing.T) {
	b := testBoard()
	b.Register("moderator", "moderator")
	roots := make([]Message, 8)
	for i := range roots {
		roots[i], _ = b.Post("a", "audit", "*", 0, fmt.Sprint("root", i), "evidence")
	}
	var wg sync.WaitGroup
	for _, root := range roots {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			b.Call(context.Background(), "moderator", "moderator", "forum_moderate", json.RawMessage(fmt.Sprintf(`{"thread_id":%d,"action":"pin","reason":"important"}`, id)))
		}(root.ID)
	}
	wg.Wait()
	posts := b.ListPosts(1, 64, "").Posts
	pinned := []int64{}
	ordinary := int64(0)
	for _, p := range posts {
		if p.Root.Pinned {
			pinned = append(pinned, p.ID)
		} else if p.Root.Topic == "root0" || ordinary == 0 {
			ordinary = p.ID
		}
	}
	if len(pinned) != 3 {
		t.Fatalf("pin race exceeded slots: %v", pinned)
	}
	before := b.Messages()
	moderationCall(t, b, "moderator", "moderator", "forum_announce", `{"content":"fourth","pinned":true}`, false)
	if !reflect.DeepEqual(before, b.Messages()) {
		t.Fatal("rejected announcement modified board")
	}
	moderationCall(t, b, "moderator", "moderator", "forum_moderate", fmt.Sprintf(`{"thread_id":%d,"action":"pin","reason":"same slot"}`, pinned[2]), true)
	if _, err := b.Post("b", "audit", "*", pinned[2], "", "latest pinned reply"); err != nil {
		t.Fatal(err)
	}
	if got := b.ListPosts(1, 64, "").Posts[0].ID; got != pinned[2] {
		t.Fatalf("pinned reply did not bump: %d", got)
	}
	if _, err := b.Post("b", "audit", "*", ordinary, "", "latest normal reply"); err != nil {
		t.Fatal(err)
	}
	posts = b.ListPosts(1, 64, "").Posts
	if posts[3].ID != ordinary || !posts[0].Root.Pinned {
		t.Fatal("normal reply must bump below pin zone")
	}
	moderationCall(t, b, "moderator", "moderator", "forum_moderate", fmt.Sprintf(`{"thread_id":%d,"action":"unpin","reason":"free slot"}`, pinned[0]), true)
	moderationCall(t, b, "moderator", "moderator", "forum_announce", `{"content":"replacement","pinned":true}`, true)
	restored := testBoard()
	if err := restored.Restore(b.Messages()); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, p := range restored.ListPosts(1, 64, "").Posts {
		if p.Root.Pinned {
			count++
		}
	}
	if count != 3 {
		t.Fatal("pin slots changed on restore")
	}
}
