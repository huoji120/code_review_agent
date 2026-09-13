package llm

import (
	"code-review-agent/internal/config"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestThinkingOptionsAcrossNativeAndPlainTransports(t *testing.T) {
	for _, api := range []string{"responses", "chat_completions"} {
		for _, stream := range []bool{false, true} {
			for _, native := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%v/native=%v", api, stream, native), func(t *testing.T) {
					client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
						var request map[string]json.RawMessage
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
							return
						}
						var thinking config.ThinkingConfig
						json.Unmarshal(request["thinking"], &thinking)
						if thinking.Type != "enabled" {
							t.Errorf("thinking not enabled: %s", request["thinking"])
						}
						var effort string
						if api == "responses" {
							var reasoning struct {
								Effort string `json:"effort"`
							}
							json.Unmarshal(request["reasoning"], &reasoning)
							effort = reasoning.Effort
							if _, exists := request["reasoning_effort"]; exists {
								t.Error("chat-only field sent to Responses")
							}
						} else {
							json.Unmarshal(request["reasoning_effort"], &effort)
						}
						if effort != "max" {
							t.Errorf("explicit effort lost: %q", effort)
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							if api == "responses" {
								fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n")
							} else {
								fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
							}
						} else if api == "responses" {
							fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
						} else {
							fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
						}
					}, stream, api)
					client.cfg.Thinking = &config.ThinkingConfig{Type: "enabled"}
					client.cfg.ReasoningEffort = "max"
					if native {
						result, err := client.ChatTools(context.Background(), nil, nil, nil)
						if err != nil || result.Content != "ok" {
							t.Fatalf("native result: %+v %v", result, err)
						}
					} else {
						result, err := client.Chat(context.Background(), nil)
						if err != nil || result != "ok" {
							t.Fatalf("plain result: %s %v", result, err)
						}
					}
				})
			}
		}
	}
}
