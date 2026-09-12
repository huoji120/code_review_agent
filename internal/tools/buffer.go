package tools

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"unicode/utf8"
)

var nextBufferID atomic.Uint64

// A saved transcript must never alias a new process's buffer counter.
var bufferProcessID = func() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(fmt.Errorf("initialize tool buffer identity: %w", err))
	}
	return fmt.Sprintf("%x", id)
}()

type bufferPage struct {
	OK         bool   `json:"ok"`
	Truncated  bool   `json:"truncated,omitempty"`
	BufferID   string `json:"buffer_id"`
	Tool       string `json:"tool,omitempty"`
	TotalBytes int64  `json:"total_bytes"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset"`
	EOF        bool   `json:"eof"`
	Preview    string `json:"preview,omitempty"`
	Content    string `json:"content,omitempty"`
}

func bufferError(message string) string {
	data, _ := json.Marshal(Result{OK: false, Error: message})
	return string(data)
}

// A worker retains only its last result. Every non-buffer tool clears it BEFORE
// execution, including tools returning short results or errors. Paging preserves it.
func (r *Registry) ClearBuffer() {
	r.bufferMu.Lock()
	r.buffer = resultBuffer{}
	r.bufferMu.Unlock()
}

// BoundResult caps the serialized envelope, not just the unescaped payload.
// Retaining the immutable string needs no second copy of a potentially large result.
func (r *Registry) BoundResult(tool, full string) string {
	r.bufferMu.Lock()
	defer r.bufferMu.Unlock()
	r.buffer = resultBuffer{}
	if len(full) <= r.maxToolResultChars {
		return full
	}
	id := fmt.Sprintf("buffer-%s-%d", bufferProcessID, nextBufferID.Add(1))
	r.buffer = resultBuffer{id: id, content: full}
	var outcome struct {
		OK bool `json:"ok"`
	}
	_ = json.Unmarshal([]byte(full), &outcome)
	page := bufferPage{OK: outcome.OK, Truncated: true, BufferID: id, Tool: tool, TotalBytes: int64(len(full))}
	return fitBufferPage(page, full, r.maxToolResultChars, true)
}

func utf8Prefix(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

func fitBufferPage(page bufferPage, content string, budget int, preview bool) string {
	n := len(content)
	if n > budget/2 {
		n = utf8Prefix(content, budget/2)
	}
	for {
		page.NextOffset = page.Offset + int64(n)
		page.EOF = page.NextOffset == page.TotalBytes
		if preview {
			page.Preview = content[:n]
		} else {
			page.Content = content[:n]
		}
		data, _ := json.Marshal(page)
		if len(data) <= budget {
			return string(data)
		}
		if n == 0 {
			return bufferError("工具 buffer 元数据超过返回上限")
		}
		next := n - (len(data) - budget)
		if next < 0 {
			next = n / 2
		}
		n = utf8Prefix(content, next)
	}
}

type readBufferArgs struct {
	BufferID string `json:"buffer_id"`
	Offset   int64  `json:"offset"`
	Limit    int    `json:"limit"`
}

func (r *Registry) readToolBuffer(raw json.RawMessage) string {
	args, err := decodeArgs[readBufferArgs](raw)
	if err != nil {
		return bufferError("无效的 buffer 参数")
	}
	if args.Offset < 0 || args.Limit < 0 {
		return bufferError("offset 和 limit 不能为负数")
	}
	if args.Limit == 0 || args.Limit > r.maxToolResultChars {
		args.Limit = r.maxToolResultChars
	}
	if args.Limit < utf8.UTFMax {
		args.Limit = utf8.UTFMax
	}
	r.bufferMu.Lock()
	defer r.bufferMu.Unlock()
	buffer := r.buffer
	if buffer.id == "" || args.BufferID != buffer.id {
		return bufferError("buffer 已失效或属于另一个 Agent；调用非 read_tool_buffer 工具即清除旧 buffer，请重新调用原工具")
	}
	if args.Offset > int64(len(buffer.content)) {
		return bufferError("offset 超过 buffer 长度")
	}
	content := buffer.content[args.Offset:]
	if len(content) > 0 && !utf8.RuneStart(content[0]) {
		return bufferError("offset 必须是 UTF-8 字符边界；请使用上次返回的 next_offset")
	}
	end := utf8Prefix(content, args.Limit)
	page := bufferPage{OK: true, BufferID: args.BufferID, Offset: args.Offset, TotalBytes: int64(len(buffer.content))}
	return fitBufferPage(page, content[:end], r.maxToolResultChars, false)
}

func (r *Registry) Fork() *Registry {
	return &Registry{workspace: r.workspace, maxToolResultChars: r.maxToolResultChars, inventory: r.inventory, interesting: r.interesting, nextTodoID: 1}
}

func (r *Registry) Close() error { r.ClearBuffer(); return nil }
