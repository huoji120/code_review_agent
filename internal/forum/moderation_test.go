package forum

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func moderationCall(t *testing.T, b *Board, id, stage, name, args string, wantOK bool) string {
	t.Helper()
	out := b.Call(context.Background(), id, stage, name, json.RawMessage(args))
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.OK != wantOK {
		t.Fatalf("%s as %s/%s: %s", name, id, stage, out)
	}
	return out
}

func TestModeratorPermissionsAndClosedNestedReplies(t *testing.T) {
	b := testBoard()
	root, _ := b.Post("a", "audit", "*", 0, "evidence", "original evidence")
	reply, _ := b.Post("b", "audit", "*", root.ID, "", "nested evidence")
	args := fmt.Sprintf(`{"thread_id":%d,"action":"close","reason":"Superseded by verified evidence"}`, reply.ID)
	before := b.Messages()
	moderationCall(t, b, "moderator", "moderator", "forum_moderate", args, false)
	b.Register("moderator", "moderator")
	b.Register("impostor", "moderator")
	for _, identity := range [][2]string{{"a", "audit"}, {"moderator", "audit"}, {"impostor", "moderator"}} {
		moderationCall(t, b, identity[0], identity[1], "forum_moderate", args, false)
		moderationCall(t, b, identity[0], identity[1], "forum_announce", `{"topic":"forged","content":"forged announcement","pinned":true}`, false)
	}
	moderationCall(t, b, "moderator", "moderator", "forum_moderate", fmt.Sprintf(`{"thread_id":%d,"action":"close","reason":" "}`, root.ID), false)
	if !reflect.DeepEqual(before, b.Messages()) {
		t.Fatal("denied management mutated forum evidence")
	}
	// A hook must be able to read the canonical board without deadlocking.
	var hookMessage Message
	b.SetOnPost(func(m Message) {
		_ = b.Messages()
		hookMessage = m
	})
	moderationCall(t, b, "moderator", "moderator", "forum_moderate", args, true)
	if hookMessage.AgentID != "moderator" || hookMessage.AgentName != "论坛管理员" || !strings.Contains(hookMessage.Content, "Superseded by verified evidence") {
		t.Fatal("moderation was not logged with retained reason and identity")
	}
	for _, target := range []int64{root.ID, reply.ID} {
		if _, err := b.Post("a", "audit", "*", target, "", "forbidden reply"); err == nil {
			t.Fatal("closed nested thread accepted reply")
		}
		if _, err := b.Post("moderator", "moderator", "*", target, "", "privileged bypass"); err == nil {
			t.Fatal("moderator bypassed closed thread")
		}
	}
	for i, m := range b.Messages()[:2] {
		if !m.Closed || m.Content != before[i].Content || m.ModerationReason != "Superseded by verified evidence" {
			t.Fatal("moderation lost history or did not mirror state")
		}
	}
	moderationCall(t, b, "moderator", "moderator", "forum_moderate", fmt.Sprintf(`{"thread_id":%d,"action":"reopen","reason":"New evidence available"}`, root.ID), true)
	if _, err := b.Post("c", "audit", "*", reply.ID, "", "new evidence"); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedAnnouncementSurvivesRootEvictionAndRestore(t *testing.T) {
	b := testBoard()
	b.Register("moderator", "moderator")
	var hookMessage Message
	b.SetOnPost(func(m Message) { hookMessage = m })
	out := moderationCall(t, b, "moderator", "moderator", "forum_announce", `{"topic":"Review scope","content":"@all Please review uncovered code","pinned":true}`, true)
	var result struct {
		Message Message `json:"message"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	root := result.Message
	if !hookMessage.Pinned || !hookMessage.Announcement {
		t.Fatal("announcement hook observed incomplete metadata")
	}
	reply, _ := b.Post("a", "audit", "*", root.ID, "", "review evidence")
	b.Post("b", "audit", "*", 0, "newer", "newer root")
	if posts := b.ListPosts(1, 10, "").Posts; posts[0].ID != root.ID || !posts[0].Root.Announcement {
		t.Fatal("pinned announcement did not sort first")
	}
	moderationCall(t, b, "moderator", "moderator", "forum_moderate", fmt.Sprintf(`{"thread_id":%d,"action":"close","reason":"Review completed"}`, root.ID), true)
	// A retention snapshot can start after the root. The remaining nested
	// messages must retain the closed, pinned, and announcement state.
	encoded, err := json.Marshal(b.Messages()[1:])
	if err != nil {
		t.Fatal(err)
	}
	var saved []Message
	if err := json.Unmarshal(encoded, &saved); err != nil {
		t.Fatal(err)
	}
	restored := testBoard()
	restored.Register("moderator", "moderator")
	if err := restored.Restore(saved); err != nil {
		t.Fatal(err)
	}
	post := restored.ListPosts(1, 10, "").Posts[0]
	if post.ID != root.ID || !post.RootMissing || !post.Root.Closed || !post.Root.Pinned || !post.Root.Announcement || post.Root.ModerationReason != "Review completed" {
		t.Fatalf("retained thread metadata lost: %+v", post)
	}
	if _, err := restored.Post("b", "audit", "*", reply.ID, "", "forbidden after restore"); err == nil {
		t.Fatal("root eviction reopened thread")
	}
	var pageResult struct {
		Posts []postSummary `json:"posts"`
	}
	out = restored.Call(context.Background(), "b", "audit", "forum_threads", json.RawMessage(`{"limit":1}`))
	if err := json.Unmarshal([]byte(out), &pageResult); err != nil || len(pageResult.Posts) != 1 || !pageResult.Posts[0].Announcement || !pageResult.Posts[0].Closed || !pageResult.Posts[0].Pinned {
		t.Fatalf("thread discovery hid moderation state: %s", out)
	}
	moderationCall(t, restored, "moderator", "moderator", "forum_moderate", fmt.Sprintf(`{"thread_id":%d,"action":"unpin","reason":"No longer current"}`, root.ID), true)
	if restored.ListPosts(1, 10, "").Posts[0].ID == root.ID {
		t.Fatal("unpin did not restore latest-activity ordering")
	}
}

func TestRestoreRejectsContradictoryThreadStateAtomically(t *testing.T) {
	b := testBoard()
	root, _ := b.Post("a", "audit", "*", 0, "", "original")
	b.Post("b", "audit", "*", root.ID, "", "reply")
	before := b.Messages()
	bad := append([]Message(nil), before...)
	bad[0].Closed, bad[0].ModerationReason = true, "closed with evidence"
	if err := b.Restore(bad); err == nil {
		t.Fatal("contradictory state accepted")
	}
	if !reflect.DeepEqual(before, b.Messages()) {
		t.Fatal("failed restore mutated board")
	}
	bad = append([]Message(nil), before...)
	bad[1].ThreadID = bad[1].ID
	if err := b.Restore(bad); err == nil {
		t.Fatal("nested reply was allowed to escape its thread")
	}
}
