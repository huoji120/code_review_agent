package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestNativeConfiguredOutputCanExceedOneMiBButRemainsBounded(t *testing.T) {
	const outputTokens = 98304
	const limit = outputTokens * 16
	for _, api := range []string{"responses", "chat_completions"} {
		for _, size := range []int{responsesMaxBytes + 65536, limit + 1} {
			t.Run(fmt.Sprintf("%s/bytes=%d", api, size), func(t *testing.T) {
				client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					for remaining := size; remaining > 0; {
						n := 32768
						if remaining < n {
							n = remaining
						}
						text := strings.Repeat("x", n)
						var err error
						if api == "responses" {
							_, err = fmt.Fprintf(w, "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"%s\"}\n\n", text)
						} else {
							_, err = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"%s\"}}]}\n\n", text)
						}
						if err != nil {
							return
						}
						remaining -= n
					}
					if api == "responses" {
						fmt.Fprint(w, "data: "+`{"type":"response.completed","response":{"status":"completed"}}`+"\n\n")
					} else {
						fmt.Fprint(w, "data: "+`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
					}
				}, true, api)
				client.cfg.MaxOutputTokens = outputTokens
				result, err := client.ChatTools(context.Background(), nil, nil, nil)
				if size <= limit {
					if err != nil || result.Thinking != strings.Repeat("x", size) {
						t.Fatalf("configured long generation rejected: bytes=%d err=%v", len(result.Thinking), err)
					}
				} else if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("native text exceeds %d bytes", limit)) || result.Thinking != "" {
					t.Fatalf("configured safety limit not enforced: bytes=%d err=%v", len(result.Thinking), err)
				}
			})
		}
	}
}

func TestNativeTextLimitOverflowDoesNotWrap(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, tokens := range []int{maxInt/16 + 1, maxInt} {
		if got := nativeTextLimit(tokens); got != maxInt {
			t.Fatalf("overflowed token limit: tokens=%d bytes=%d", tokens, got)
		}
	}
}

func TestNativeNonstreamConfiguredTextAndArgumentBoundaries(t *testing.T) {
	for _, api := range []string{"responses", "chat_completions"} {
		for _, tool := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/tool=%v", api, tool), func(t *testing.T) {
				text := strings.Repeat("x", responsesMaxBytes+1)
				client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if api == "responses" {
						item := map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}
						if tool {
							item = map[string]any{"type": "function_call", "call_id": "c", "name": "read_file", "arguments": `{"path":"` + text + `"}`}
						}
						json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{item}})
					} else {
						message := map[string]any{"role": "assistant", "content": text}
						if tool {
							message = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"` + text + `"}`}}}}
						}
						json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": "stop"}}})
					}
				}, false, api)
				client.cfg.MaxOutputTokens = 98304
				result, err := client.ChatTools(context.Background(), nil, nil, nil)
				if !tool && (err != nil || result.Content != text) {
					t.Fatalf("configured nonstream text rejected: %v", err)
				}
				if tool && (err == nil || !strings.Contains(err.Error(), "arguments too large")) {
					t.Fatalf("oversized arguments escaped: %v", err)
				}
			})
		}
	}
}
