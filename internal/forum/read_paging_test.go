package forum

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func bodyRequest(t *testing.T, b *Board, id string, args any) (bodyPage, string) {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	raw := b.Call(context.Background(), id, "audit", "forum_read", data)
	if len(raw) > 4096 {
		t.Fatalf("body JSON exceeded page budget: %d", len(raw))
	}
	var p bodyPage
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	return p, raw
}

func TestForumLongBodyDiscoveryAndExactUTF8Reconstruction(t *testing.T) {
	b := testBoard()
	content := strings.Repeat("中\"\\<>&\n", 900)
	m, err := b.Post("a", "audit", "*", 0, strings.Repeat("\"", 200), content)
	if err != nil {
		t.Fatal(err)
	}
	raw := b.Call(context.Background(), "b", "audit", "forum_read", json.RawMessage(`{"limit":64}`))
	var discovery page
	if err := json.Unmarshal([]byte(raw), &discovery); err != nil {
		t.Fatal(err)
	}
	if len(raw) > 4096 || !discovery.OK || len(discovery.Messages) != 1 || discovery.NextID != m.ID || discovery.HasMore {
		t.Fatalf("message discovery cursor or serialized budget failed: %s", raw)
	}
	preview := discovery.Messages[0]
	if !preview.HasMore || preview.TotalBytes != len(content) || preview.NextOffset != len(preview.Content) || len(preview.Content) > 512 || preview.Content != content[:preview.NextOffset] {
		t.Fatal("discovery returned full body or lost byte-paging markers")
	}
	rebuilt, offset := preview.Content, preview.NextOffset
	for offset < len(content) {
		p, raw := bodyRequest(t, b, "b", map[string]any{"message_id": m.ID, "offset": offset, "max_bytes": 4096})
		if !p.OK {
			t.Fatal(raw)
		}
		if p.Message.ID != m.ID || p.Offset != offset || p.TotalBytes != len(content) || p.NextOffset != offset+len(p.Message.Content) || p.NextOffset <= offset || !utf8.ValidString(p.Message.Content) || p.HasMore != (p.NextOffset < len(content)) {
			t.Fatal("body page lost ID, exact byte offset, or UTF-8 boundary")
		}
		rebuilt += p.Message.Content
		offset = p.NextOffset
	}
	if rebuilt != content || b.Messages()[0].Content != content {
		t.Fatal("body paging changed exact evidence or canonical TUI content")
	}
	end, raw := bodyRequest(t, b, "b", map[string]any{"message_id": m.ID, "offset": len(content)})
	if !end.OK || end.HasMore || end.NextOffset != len(content) || end.Message.Content != "" {
		t.Fatalf("EOF not stable: %s", raw)
	}
}

func TestForumBodyRejectsInvalidByteOffsetsAndMixedCursors(t *testing.T) {
	b := testBoard()
	m, err := b.Post("a", "audit", "*", 0, "", "中文 evidence")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range []map[string]any{
		{"message_id": m.ID, "offset": -1},
		{"message_id": m.ID, "offset": 1},
		{"message_id": m.ID, "offset": len(m.Content) + 1},
		{"message_id": m.ID, "max_bytes": 1},
		{"message_id": m.ID, "after_id": m.ID},
		{"message_id": 0},
		{"offset": 3},
	} {
		p, raw := bodyRequest(t, b, "b", args)
		if p.OK || !strings.Contains(raw, `"error":`) {
			t.Fatalf("invalid page accepted: %v %s", args, raw)
		}
	}
	p, raw := bodyRequest(t, b, "b", map[string]any{"message_id": m.ID, "offset": 3, "max_bytes": 3})
	if !p.OK || p.Message.Content != "文" || p.NextOffset != 6 || !p.HasMore {
		t.Fatalf("valid byte boundary failed: %s", raw)
	}
}

func TestForumBodyVisibilityThreadFiltersAndExpiredIDs(t *testing.T) {
	b := testBoard()
	root, _ := b.Post("a", "audit", "b", 0, "private routing", "root body")
	reply, _ := b.Post("b", "audit", "a", root.ID, "", "reply body")
	nested, _ := b.Post("a", "audit", "b", reply.ID, "", "nested body")
	other, _ := b.Post("c", "audit", "*", 0, "", "other body")
	for _, args := range []map[string]any{
		{"message_id": root.ID},
		{"message_id": root.ID, "all": true, "from": "b"},
		{"message_id": nested.ID, "all": true, "thread_id": other.ID},
	} {
		p, raw := bodyRequest(t, b, "c", args)
		if p.OK || strings.Contains(raw, "root body") || strings.Contains(raw, "nested body") {
			t.Fatalf("body visibility filter bypassed: %s", raw)
		}
	}
	p, raw := bodyRequest(t, b, "c", map[string]any{"message_id": nested.ID, "all": true, "from": "a", "thread_id": reply.ID})
	if !p.OK || p.Message.Content != nested.Content {
		t.Fatalf("nested reply body filtering failed: %s", raw)
	}
	retained := testBoard()
	if err := retained.Restore([]Message{other}); err != nil {
		t.Fatal(err)
	}
	p, raw = bodyRequest(t, retained, "b", map[string]any{"message_id": root.ID, "offset": 3, "all": true})
	if p.OK || !strings.Contains(raw, "expired") || strings.Contains(raw, other.Content) {
		t.Fatalf("expired body cursor reused another message: %s", raw)
	}
}

func TestForumDiscoveryAndWaitRespectSerializedBudgetWithoutSkipping(t *testing.T) {
	b := testBoard()
	var ids []int64
	for i := 0; i < 18; i++ {
		m, err := b.Post("a", "audit", "*", 0, strings.Repeat("\\\"", 80), strings.Repeat("\\\"\n", 300))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	offset, seen := int64(0), 0
	for {
		args := json.RawMessage(fmt.Sprintf(`{"after_id":%d,"limit":64}`, offset))
		raw := b.Call(context.Background(), "b", "audit", "forum_wait", args)
		var p page
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		if !p.OK || len(raw) > 4096 || len(p.Messages) == 0 {
			t.Fatalf("bounded wait discovery failed: %s", raw)
		}
		for _, m := range p.Messages {
			if seen >= len(ids) || m.ID != ids[seen] || !m.HasMore || m.NextOffset != len(m.Content) {
				t.Fatal("bounded page skipped message or concealed truncated body")
			}
			seen++
		}
		if p.NextID != p.Messages[len(p.Messages)-1].ID || p.NextID <= offset {
			t.Fatal("byte cursor replaced whole-message cursor")
		}
		offset = p.NextID
		if !p.HasMore {
			break
		}
	}
	if seen != len(ids) {
		t.Fatal("discovery dropped messages across serialized boundary")
	}
	for i := 0; i < 128; i++ {
		id := fmt.Sprintf("peer-%03d-%s", i, strings.Repeat("x", 48))
		b.Register(id, "audit")
		b.SetStatus(id, "completed")
	}
	b.SetStatus("a", "completed")
	b.SetStatus("c", "completed")
	raw := b.Call(context.Background(), "b", "audit", "forum_wait", json.RawMessage(fmt.Sprintf(`{"after_id":%d}`, offset)))
	var p page
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if len(raw) > 4096 || !p.PeersDone || !p.RosterHasMore || p.TimedOut {
		t.Fatalf("wait roster exceeded page budget or lost completion state: %s", raw)
	}
}
