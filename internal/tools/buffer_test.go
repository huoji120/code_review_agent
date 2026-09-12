package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestToolBufferSerializedBudgetAndUTF8RoundTrip(t *testing.T) {
	r, err := NewRegistry(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	payload, _ := json.Marshal(Result{OK: true, Data: strings.Repeat("中文🙂\"\\\n<>&", 3000)})
	result := r.BoundResult("search_content", string(payload))
	var page bufferPage
	decode := func(s string) {
		t.Helper()
		if len(s) > 1024 || !json.Valid([]byte(s)) || !utf8.ValidString(s) {
			t.Fatalf("invalid bounded page: %d bytes", len(s))
		}
		page = bufferPage{}
		if err := json.Unmarshal([]byte(s), &page); err != nil {
			t.Fatal(err)
		}
	}
	decode(result)
	if !page.Truncated || page.BufferID == "" || !page.OK {
		t.Fatalf("missing buffer envelope: %s", result)
	}
	all := page.Preview
	id := page.BufferID
	for !page.EOF {
		offset := page.NextOffset
		args, _ := json.Marshal(readBufferArgs{BufferID: id, Offset: offset, Limit: 127})
		bounded, full := r.CallWithFullResult(context.Background(), "read_tool_buffer", args)
		if bounded != full {
			t.Fatal("trace lost buffer page")
		}
		decode(bounded)
		if page.NextOffset <= offset {
			t.Fatal("page failed to advance")
		}
		all += page.Content
	}
	if all != string(payload) {
		t.Fatal("pagination lost or duplicated bytes")
	}
	bad, _ := json.Marshal(readBufferArgs{BufferID: id, Offset: -1})
	if strings.Contains(r.Call("read_tool_buffer", bad), `"ok":true`) {
		t.Fatal("negative offset accepted")
	}
}

func TestToolBufferLatestOnlyAndWorkerIsolation(t *testing.T) {
	r, err := NewRegistry(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	full := `{"ok":false,"error":"` + strings.Repeat("failed ", 1000) + `"}`
	var first bufferPage
	if err = json.Unmarshal([]byte(r.BoundResult("test", full)), &first); err != nil {
		t.Fatal(err)
	}
	if first.OK {
		t.Fatal("buffer changed tool failure into success")
	}
	fork := r.Fork()
	defer fork.Close()
	args, _ := json.Marshal(readBufferArgs{BufferID: first.BufferID})
	if strings.Contains(fork.Call("read_tool_buffer", args), `"ok":true`) {
		t.Fatal("fork exposed another worker buffer")
	}
	r.Call("todo_create", json.RawMessage(`{"title":"small result"}`))
	if strings.Contains(r.Call("read_tool_buffer", args), `"ok":true`) {
		t.Fatal("short non-buffer tool did not invalidate old result")
	}
	json.Unmarshal([]byte(r.BoundResult("test", full)), &first)
	args, _ = json.Marshal(readBufferArgs{BufferID: first.BufferID})
	r.Call("unknown", json.RawMessage(`{}`))
	if strings.Contains(r.Call("read_tool_buffer", args), `"ok":true`) {
		t.Fatal("failed tool did not invalidate old result")
	}
	json.Unmarshal([]byte(r.BoundResult("test", full)), &first)
	args, _ = json.Marshal(readBufferArgs{BufferID: first.BufferID})
	var replacement bufferPage
	json.Unmarshal([]byte(r.BoundResult("replacement", full)), &replacement)
	if strings.Contains(r.Call("read_tool_buffer", args), `"ok":true`) {
		t.Fatal("new output retained an older buffer")
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	args, _ = json.Marshal(readBufferArgs{BufferID: replacement.BufferID})
	if strings.Contains(r.Call("read_tool_buffer", args), `"ok":true`) {
		t.Fatal("closed registry still exposed its result")
	}
}

func TestToolBufferRejectsMidRuneOffset(t *testing.T) {
	r, err := NewRegistry(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	full := `{"ok":true,"data":"` + strings.Repeat("汉", 1000) + `"}`
	var p bufferPage
	json.Unmarshal([]byte(r.BoundResult("test", full)), &p)
	offset := strings.Index(full, "汉") + 1
	args, _ := json.Marshal(readBufferArgs{BufferID: p.BufferID, Offset: int64(offset), Limit: 1})
	if strings.Contains(r.Call("read_tool_buffer", args), `"ok":true`) {
		t.Fatal("accepted split character offset")
	}
	args, _ = json.Marshal(readBufferArgs{BufferID: p.BufferID, Offset: int64(offset - 1), Limit: 1})
	var got bufferPage
	json.Unmarshal([]byte(r.Call("read_tool_buffer", args)), &got)
	if got.Content != "汉" || got.NextOffset != int64(offset+2) {
		t.Fatalf("short page split character: %+v", got)
	}
}
