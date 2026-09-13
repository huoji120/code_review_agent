package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"code-review-agent/internal/config"
)

func responsesTestClient(t *testing.T, handler http.HandlerFunc, stream bool, api string) *OpenAIClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewOpenAIClient(config.OpenAIConfig{BaseURL: server.URL + "/v1", APIKey: "test", Model: "test", Stream: stream, APIInterface: api})
}

func TestResponsesReplaysHistoryAsEasyMessages(t *testing.T) {
	history := []Message{{Role: RoleSystem, Content: "instructions"}, {Role: RoleUser, Content: "question"}, {Role: RoleAssistant, Content: "prior answer"}, {Role: RoleTool, Content: "tool result"}}
	client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("request path = %s", r.URL.Path)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, ok := body["previous_response_id"]; ok {
			t.Error("stateless history unexpectedly depends on previous_response_id")
		}
		var input []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(body["input"], &input); err != nil {
			t.Errorf("easy-message content must accept assistant history: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(input) != len(history) {
			t.Errorf("replayed %d messages, want %d", len(input), len(history))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for i, item := range input {
			role := string(history[i].Role)
			if role == "tool" {
				role = "user"
			}
			if item.Role != role || item.Content != history[i].Content {
				t.Errorf("history item %d = %+v", i, item)
			}
		}
		fmt.Fprint(w, `{"status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"considered carefully"}],"content":[{"type":"output_text","text":"not an answer"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"},{"type":"reasoning_text","text":"not answer text"}]},{"type":"function_call","content":[{"type":"output_text","text":"not executable"}]}]}`)
	}, false, "")
	got, err := client.Chat(context.Background(), history)
	if err != nil || got != "<think>considered carefully</think>answer" {
		t.Fatalf("Chat = %q, %v", got, err)
	}
}

func TestResponsesStreamSeparatesReasoningAndRecoversOnlyMissingParts(t *testing.T) {
	client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.reasoning_text.delta","output_index":0,"content_index":0,"delta":"<tool_call>example</tool_call>"}`,
			`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"summary"}`,
			`{"type":"response.function_call_arguments.delta","delta":"do not execute"}`,
			`{"type":"unknown.delta","delta":{"text":"also not executable"}}`,
			`{"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"answer"}`,
			`{"type":"response.output_text.done","text":"answer"}`,
			`{"type":"response.reasoning_text.done","text":"<tool_call>example</tool_call>"}`,
			`{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"<tool_call>example</tool_call>"}],"summary":[{"type":"summary_text","text":"summary"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"},{"type":"output_text","text":" recovered"}]}]}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}, true, "responses")
	var thinking, content strings.Builder
	err := client.ChatStream(context.Background(), nil, func(delta Delta) error {
		thinking.WriteString(delta.Thinking)
		content.WriteString(delta.Content)
		return nil
	})
	if err != nil || content.String() != "answer recovered" || thinking.String() != "<tool_call>example</tool_call>summary" {
		t.Fatalf("stream = content %q, thinking %q, error %v", content.String(), thinking.String(), err)
	}
}

func TestResponsesCompletedSnapshotWithoutDeltas(t *testing.T) {
	client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: "+`{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"summary"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"safe answer"}]}]}}`+"\n\n")
	}, true, "")
	got, err := client.Chat(context.Background(), nil)
	if err != nil || got != "<think>summary</think>safe answer" {
		t.Fatalf("snapshot Chat = %q, %v", got, err)
	}
}

func TestResponsesStreamRejectsUnsuccessfulTermination(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal string
	}{
		{"failed", `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"failed generation"}}}`},
		{"incomplete", `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`},
		{"cancelled", `{"type":"response.cancelled","response":{"status":"cancelled"}}`},
		{"error", `{"type":"error","code":"server_error","message":"failed generation"}`},
		{"contradictory_completed", `{"type":"response.completed","response":{"status":"incomplete"}}`},
		{"bare_done", "[DONE]"},
		{"transport_eof", ""},
		{"malformed_terminal", `{"type":"response.completed"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, "data: "+`{"type":"response.output_text.delta","delta":"<tool_call>partial</tool_call>"}`+"\n\n")
				if tc.terminal != "" {
					fmt.Fprintf(w, "data: %s\n\n", tc.terminal)
				}
			}, true, "responses")
			if err := client.ChatStream(context.Background(), nil, func(Delta) error { return nil }); err == nil {
				t.Fatal("ChatStream accepted unsuccessful response")
			}
			got, err := client.Chat(context.Background(), nil)
			if err == nil || got != "" {
				t.Fatalf("unsuccessful Chat exposed partial text: %q, %v", got, err)
			}
		})
	}
}

func TestResponsesChatRejectsUnsuccessfulResponse(t *testing.T) {
	for _, body := range []string{
		`{"status":"failed","error":{"code":"server_error","message":"failed generation"}}`,
		`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"<tool_call>partial</tool_call>"}]}]}`,
		`{"status":"cancelled"}`,
		`{"error":{"message":"rejected"}}`,
	} {
		client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, body)
		}, false, "responses")
		got, err := client.Chat(context.Background(), nil)
		if err == nil || got != "" {
			t.Fatalf("unsuccessful Chat exposed text: %q, %v", got, err)
		}
	}
}

func TestResponsesStreamCancellationAndCallbackErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: "+`{"type":"response.output_text.delta","delta":"visible progress"}`+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}, true, "responses")
	err := client.ChatStream(ctx, nil, func(Delta) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled stream = %v", err)
	}
	stop := errors.New("consumer stopped")
	err = client.ChatStream(context.Background(), nil, func(Delta) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("callback error = %v", err)
	}
}

func TestExplicitChatCompletionsRemainsAvailable(t *testing.T) {
	for _, stream := range []bool{false, true} {
		client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/chat/completions" {
				t.Errorf("explicit chat path = %s", r.URL.Path)
			}
			var body struct {
				Messages []Message `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 1 || body.Messages[0].Content != "question" {
				t.Errorf("chat messages = %+v, %v", body.Messages, err)
			}
			if stream {
				fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"reasoning_content":"thinking","content":"answer"}}]}`+"\n\ndata: [DONE]\n\n")
			} else {
				fmt.Fprint(w, `{"choices":[{"message":{"reasoning_content":"thinking","content":"answer"}}]}`)
			}
		}, stream, "chat_completions")
		got, err := client.Chat(context.Background(), []Message{{Role: RoleUser, Content: "question"}})
		if err != nil || got != "<think>thinking</think>answer" {
			t.Fatalf("explicit chat stream=%v: %q, %v", stream, got, err)
		}
	}
}
