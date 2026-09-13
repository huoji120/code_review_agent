package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"code-review-agent/internal/config"
	"code-review-agent/internal/llm"
)

func TestIncompleteSSERetriesFromCompletedToolCheckpoint(t *testing.T) {
	for _, scenario := range []string{"recover", "exhaust", "filtered"} {
		t.Run(scenario, func(t *testing.T) {
			requests := 0
			var initialInput json.RawMessage
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				data, _ := io.ReadAll(r.Body)
				var request struct {
					Input json.RawMessage `json:"input"`
				}
				if err := json.Unmarshal(data, &request); err != nil {
					t.Error(err)
				}
				if requests == 1 {
					initialInput = append(json.RawMessage(nil), request.Input...)
				} else if string(request.Input) != string(initialInput) {
					t.Error("retry altered the completed checkpoint or retained partial output")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if scenario == "recover" && requests == 4 {
					fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"status\":\"completed\",\"id\":\"new-item\",\"call_id\":\"new-call\",\"name\":\"read_handoff\",\"arguments\":\"{}\"}]}}\n\n")
					return
				}
				fmt.Fprint(w, "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"FAILED_THINK\"}\n\n")
				fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"partial\",\"call_id\":\"partial-call\",\"name\":\"forum_post\"}}\n\n")
				fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{\\\"content\\\":\\\"PARTIAL\"}\n\n")
				reason := "max_output_tokens"
				if scenario == "filtered" {
					reason = "content_filter"
				}
				fmt.Fprintf(w, "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":%q}}}\n\n", reason)
			}))
			defer server.Close()
			client := llm.NewOpenAIClient(config.OpenAIConfig{BaseURL: server.URL, APIKey: "fixture", APIInterface: "responses", Model: "fixture", Stream: true, MaxOutputTokens: 1000, TimeoutSeconds: 5})
			team := newTestTeam(t, client)
			team.createStageLocked(phaseRecon)
			a := team.workers[0].agent
			previous := toolReply("read_handoff", map[string]any{})
			call, _ := a.recordResponse(previous)
			a.executeNativeTool(context.Background(), func(Event) {}, call)
			if err := a.prepareRequest(context.Background(), func(Event) {}); err != nil {
				t.Fatal(err)
			}
			before := append([]llm.Message(nil), a.messages...)
			retries := 0
			response, err := a.chatStream(context.Background(), func(event Event) {
				if strings.Contains(event.Content, "回退到上次完整结果，重试") {
					retries++
				}
			})
			if !reflect.DeepEqual(before, a.messages) {
				t.Fatal("failed generation mutated checkpoint or replayed completed tool")
			}
			if strings.Contains(string(initialInput), "PARTIAL") || strings.Contains(string(initialInput), "FAILED_THINK") {
				t.Fatal("partial output entered history")
			}
			if scenario == "filtered" {
				if err == nil || requests != 1 || retries != 0 {
					t.Fatalf("filtered error retried: requests=%d retries=%d err=%v", requests, retries, err)
				}
				return
			}
			if requests != 4 || retries != 3 {
				t.Fatalf("requests=%d retries=%d err=%v", requests, retries, err)
			}
			if scenario == "recover" {
				if err != nil || len(response.Calls) != 1 || response.Calls[0].CallID != "new-call" {
					t.Fatalf("recovery returned partial result: %+v %v", response, err)
				}
			} else if !errors.Is(err, llm.ErrIncompleteGeneration) || len(response.Calls) != 0 {
				t.Fatalf("exhaustion lost error or exposed partial call: %+v %v", response, err)
			}
		})
	}
}
