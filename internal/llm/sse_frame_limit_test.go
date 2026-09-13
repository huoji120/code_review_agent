package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestConfiguredLargeSSEFrameAndTerminalSnapshot(t *testing.T) {
	for _, api := range []string{"responses", "chat_completions"} {
		for _, native := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/native=%v", api, native), func(t *testing.T) {
				text := strings.Repeat("x", responsesMaxBytes+65536)
				client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					event := map[string]any{}
					if api == "responses" {
						// Providers may repeat the complete output in one terminal JSON frame.
						event = map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": text}
						data, _ := json.Marshal(event)
						fmt.Fprint(w, "data: "+string(data)+"\n\n")
						event = map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}}}}
						data, _ = json.Marshal(event)
						fmt.Fprint(w, "data: "+string(data)+"\n\n")
					} else {
						event = map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": "stop"}}}
						data, _ := json.Marshal(event)
						fmt.Fprint(w, "data: "+string(data)+"\n\ndata: [DONE]\n\n")
					}
				}, true, api)
				client.cfg.MaxOutputTokens = 98304
				if native {
					result, err := client.ChatTools(context.Background(), nil, nil, nil)
					if err != nil || result.Content != text {
						t.Fatalf("large SSE frame/snapshot rejected or duplicated: bytes=%d err=%v", len(result.Content), err)
					}
				} else {
					var output strings.Builder
					err := client.ChatStream(context.Background(), nil, func(d Delta) error { output.WriteString(d.Content); return nil })
					if err != nil || output.String() != text {
						t.Fatalf("large plain SSE rejected or duplicated: bytes=%d err=%v", output.Len(), err)
					}
				}
			})
		}
	}
}
