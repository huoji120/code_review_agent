package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func responseCallEvent(kind, args string) string {
	return fmt.Sprintf("data: {\"type\":%q,\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_real\",\"call_id\":\"call_real\",\"name\":\"read_file\",\"arguments\":%q}}\n\n", kind, args)
}
func TestNativeResponsesNeverReleaseInvalidOrUnfinishedCalls(t *testing.T) {
	start := responseCallEvent("response.output_item.added", "")
	complete := `data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"
	for _, tc := range []struct {
		name, events string
		protocol     bool
	}{
		{"EOF after complete JSON", start + `data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_real","delta":"{}"}` + "\n\n", false},
		{"cancelled terminal", start + responseCallEvent("response.output_item.done", `{}`) + `data: {"type":"response.cancelled","response":{"status":"cancelled"}}` + "\n\n", false},
		{"mismatched item identity", start + `data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_other","delta":"{}"}` + "\n\n" + complete, true},
		{"snapshot replaces generated arguments", start + `data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_real","delta":"{\"path\":\"good\"}"}` + "\n\n" + responseCallEvent("response.output_item.done", `{"path":"different"}`) + complete, true},
		{"missing call ID", `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","id":"fc_not_a_call_id","name":"read_file","arguments":"{}"}]}}` + "\n\n", true},
		{"array arguments", start + responseCallEvent("response.output_item.done", `[]`) + complete, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := readNativeResponses(context.Background(), strings.NewReader(tc.events), nil)
			if err == nil || len(result.Calls) != 0 || result.Content != "" || result.Thinking != "" {
				t.Fatalf("invalid stream exposed executable output: %+v %v", result, err)
			}
			if tc.protocol && !errors.Is(err, ErrToolProtocol) {
				t.Fatalf("model format error not classifiable: %v", err)
			}
		})
	}
}
func TestNativeResponsesSnapshotDeduplicatesCallsAndKeepsReasoningInert(t *testing.T) {
	examples := `data: {"type":"response.reasoning_text.delta","output_index":1,"content_index":0,"delta":"<tool_call>dangerous example</tool_call>"}` + "\n\n"
	events := examples + responseCallEvent("response.output_item.added", "") + `data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_real","delta":"{\"path\":"}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_real","delta":"\"a.go\"}"}` + "\n\n" + responseCallEvent("response.output_item.done", `{"path":"a.go"}`) +
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","id":"fc_real","call_id":"call_real","name":"read_file","arguments":"{\"path\":\"a.go\"}"},{"type":"reasoning","content":[{"type":"reasoning_text","text":"<tool_call>dangerous example</tool_call>"}]}]}}` + "\n\n"
	result, err := readNativeResponses(context.Background(), strings.NewReader(events), nil)
	if err != nil || len(result.Calls) != 1 || result.Calls[0].CallID != "call_real" || result.Calls[0].Arguments != `{"path":"a.go"}` || result.Thinking != "<tool_call>dangerous example</tool_call>" {
		t.Fatalf("stream/snapshot mismatch: %+v %v", result, err)
	}
}
func TestNativeChatRequiresSuccessfulTerminalAndStableCallIdentity(t *testing.T) {
	start := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"real","type":"function","function":{"name":"read_file","arguments":"{}"}}]}}]}` + "\n\n"
	for _, ending := range []string{
		"data: [DONE]\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\ndata: [DONE]\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"changed","function":{"arguments":""}}]}}]}` + "\n\ndata: [DONE]\n\n",
	} {
		result, err := readNativeChat(context.Background(), strings.NewReader(start+ending), nil)
		if err == nil || len(result.Calls) != 0 {
			t.Fatalf("unfinished/ambiguous call accepted: %+v %v", result, err)
		}
	}
	result, err := readNativeChat(context.Background(), strings.NewReader(start), nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) || len(result.Calls) != 0 {
		t.Fatalf("unterminated chat call exposed: %+v %v", result, err)
	}
}
func TestNativeToolStreamCancellationDiscardsCollectedCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := responseCallEvent("response.output_item.added", `{}`) + `data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n" + `data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"
	result, err := readNativeResponses(ctx, strings.NewReader(events), func(d Delta) error {
		if d.Content != "" {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || len(result.Calls) != 0 {
		t.Fatalf("cancellation released tool call: %+v %v", result, err)
	}
}

func TestNativeProgressArrivesBeforeTerminalAndUsesProviderUsage(t *testing.T) {
	for _, api := range []string{"responses", "chat_completions"} {
		t.Run(api, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			advance := make(chan struct{})
			progress := make(chan GenerationProgress, 16)
			finished := make(chan error, 1)
			client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				send := func(event string) { fmt.Fprintf(w, "data: %s\n\n", event); w.(http.Flusher).Flush() }
				select {
				case <-advance:
				case <-ctx.Done():
					return
				}
				if api == "responses" {
					send(`{"type":"response.reasoning_text.delta","delta":"abcdefghijklmnop"}`)
				} else {
					send(`{"choices":[{"index":0,"delta":{"reasoning_content":"abcdefghijklmnop"}}]}`)
				}
				select {
				case <-advance:
				case <-ctx.Done():
					return
				}
				fmt.Fprint(w, ": keepalive\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-advance:
				case <-ctx.Done():
					return
				}
				if api == "responses" {
					send(`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}`)
				} else {
					send(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}}]}`)
				}
				select {
				case <-advance:
				case <-ctx.Done():
					return
				}
				if api == "responses" {
					send(`{"type":"response.completed","response":{"status":"completed","usage":{"output_tokens":3,"output_tokens_details":{"reasoning_tokens":1}},"output":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"abcdefghijklmnop"}]},{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}]}}`)
				} else {
					send(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
					send(`{"choices":[],"usage":{"completion_tokens":3,"completion_tokens_details":{"reasoning_tokens":1}}}`)
					send(`[DONE]`)
				}
			}, true, api)
			go func() {
				_, err := client.ChatTools(ctx, nil, nil, func(d Delta) error {
					if d.Progress != nil {
						progress <- *d.Progress
					}
					return nil
				})
				finished <- err
			}()
			next := func() GenerationProgress {
				t.Helper()
				select {
				case p := <-progress:
					return p
				case <-ctx.Done():
					t.Fatal("timed out waiting for progress")
					return GenerationProgress{}
				}
			}
			step := func() {
				t.Helper()
				select {
				case advance <- struct{}{}:
				case <-ctx.Done():
					t.Fatal("stream did not advance")
				}
			}
			initial := next()
			if initial.OutputTokens != 0 || !initial.Estimated || initial.ReceivedAt != "" {
				t.Fatalf("reset attributed output before network data: %+v", initial)
			}
			step()
			first := next()
			if first.OutputTokens != 4 || first.ReasoningTokens != 4 || !first.Estimated || first.ReceivedAt == "" {
				t.Fatalf("first reasoning not visible: %+v", first)
			}
			step()
			select {
			case p := <-progress:
				t.Fatalf("heartbeat generated progress: %+v", p)
			case <-time.After(300 * time.Millisecond):
			}
			step()
			second := next()
			if second.OutputTokens != 5 || second.ToolTokens != 1 || second.ReceivedAt == first.ReceivedAt {
				t.Fatalf("ongoing tool output not counted: %+v", second)
			}
			select {
			case err := <-finished:
				t.Fatalf("request ended before terminal: %v", err)
			default:
			}
			step()
			final := next()
			if final.Estimated || final.OutputTokens != 3 || final.ReasoningTokens != 1 || final.ToolTokens != 0 || final.ReceivedAt != second.ReceivedAt {
				t.Fatalf("provider usage did not replace estimates or snapshot duplicated output: %+v", final)
			}
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("request did not finish")
			}
		})
	}
}

func TestNativeResponseProgressRecoversSnapshotsWithoutDuplicateTokens(t *testing.T) {
	events := `data: {"type":"response.output_text.delta","output_index":0,"delta":"ab"}` + "\n\n" +
		`data: {"type":"response.output_text.done","output_index":0,"text":"abcd"}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"reasoning","content":[{"type":"reasoning_text","text":"12345678"}]}}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"abcd"}]},{"type":"reasoning","content":[{"type":"reasoning_text","text":"12345678"}]},{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}]}}` + "\n\n"
	var final GenerationProgress
	result, err := readNativeResponses(context.Background(), strings.NewReader(events), func(d Delta) error {
		if d.Progress != nil {
			final = *d.Progress
		}
		return nil
	})
	if err != nil || result.Content != "abcd" || result.Thinking != "12345678" || len(result.Calls) != 1 {
		t.Fatalf("snapshot recovery: %+v, %v", result, err)
	}
	if !final.Estimated || final.OutputTokens != 4 || final.ReasoningTokens != 2 || final.ToolTokens != 1 {
		t.Fatalf("snapshots double counted: %+v", final)
	}
}

func TestNativeProgressCancellationNeverReturnsExecutableCalls(t *testing.T) {
	for _, authoritative := range []bool{false, true} {
		for _, api := range []string{"responses", "chat_completions"} {
			t.Run(fmt.Sprintf("%s/final=%t", api, authoritative), func(t *testing.T) {
				events := responseCallEvent("response.output_item.done", "{}") + `data: {"type":"response.completed","response":{"status":"completed","usage":{"output_tokens":1}}}` + "\n\n"
				reader := readNativeResponses
				if api == "chat_completions" {
					reader = readNativeChat
					events = `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":1}}` + "\n\ndata: [DONE]\n\n"
				}
				result, err := reader(context.Background(), strings.NewReader(events), func(d Delta) error {
					if d.Progress != nil && d.Progress.OutputTokens > 0 && d.Progress.Estimated != authoritative {
						return context.Canceled
					}
					return nil
				})
				if !errors.Is(err, context.Canceled) || len(result.Calls) != 0 {
					t.Fatalf("progress cancellation exposed calls: %+v %v", result, err)
				}
			})
		}
	}
}

func TestNativeNonstreamGenerationUsesSnapshotAndProviderUsage(t *testing.T) {
	for _, api := range []string{"responses", "chat_completions"} {
		t.Run(api, func(t *testing.T) {
			client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if api == "responses" {
					fmt.Fprint(w, `{"status":"completed","usage":{"output_tokens":7,"output_tokens_details":{"reasoning_tokens":2}},"output":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"thought"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}]}`)
				} else {
					fmt.Fprint(w, `{"choices":[{"finish_reason":"tool_calls","message":{"content":"answer","reasoning_content":"thought","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}}],"usage":{"completion_tokens":7,"completion_tokens_details":{"reasoning_tokens":2}}}`)
				}
			}, false, api)
			var updates []GenerationProgress
			result, err := client.ChatTools(context.Background(), nil, nil, func(d Delta) error {
				if d.Progress != nil {
					updates = append(updates, *d.Progress)
				}
				return nil
			})
			if err != nil || result.Content != "answer" || result.Thinking != "thought" || len(result.Calls) != 1 {
				t.Fatalf("snapshot response: %+v, %v", result, err)
			}
			if len(updates) < 2 {
				t.Fatal("missing generation reset/final snapshot")
			}
			first, final := updates[0], updates[len(updates)-1]
			if first.OutputTokens != 0 || first.ReceivedAt != "" || final.Estimated || final.OutputTokens != 7 || final.ReasoningTokens != 2 || final.ReceivedAt == "" {
				t.Fatalf("invalid snapshot usage transition: %+v", updates)
			}
		})
	}
}
