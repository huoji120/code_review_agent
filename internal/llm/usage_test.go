package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func usageReply(w http.ResponseWriter, api string, stream, provider bool) {
	usage := ""
	if provider {
		if api == "responses" {
			usage = `,"usage":{"total_tokens":7}`
		} else {
			usage = `,"usage":{"prompt_tokens":3,"completion_tokens":4}`
		}
	}
	if api == "responses" {
		output := `"output":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"abcdefghijklmnop"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer12"}]},{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}]`
		if stream {
			fmt.Fprint(w, "data: "+`{"type":"response.reasoning_text.delta","output_index":0,"delta":"abc"}`+"\n\n")
			fmt.Fprint(w, "data: "+`{"type":"response.reasoning_text.delta","output_index":0,"delta":"defghijklmnop"}`+"\n\n")
			fmt.Fprint(w, "data: "+`{"type":"response.output_text.delta","output_index":1,"delta":"answer12"}`+"\n\n")
			fmt.Fprint(w, "data: "+`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{"}`+"\n\n")
			fmt.Fprint(w, "data: "+`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"}"}`+"\n\n")
			fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",%s%s}}\n\n", output, usage)
		} else {
			fmt.Fprintf(w, "{\"status\":\"completed\",%s%s}", output, usage)
		}
		return
	}
	message := `{"content":"answer12","reasoning_content":"abcdefghijklmnop","tool_calls":[{"index":0,"type":"function","id":"call_1","function":{"name":"read_file","arguments":"{}"}}]}`
	if stream {
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":%s,\"finish_reason\":\"tool_calls\"}]}\n\n", message)
		if provider {
			fmt.Fprintf(w, "data: {\"choices\":[]%s}\n\n", usage)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	} else {
		fmt.Fprintf(w, "{\"choices\":[{\"message\":%s,\"finish_reason\":\"tool_calls\"}]%s}", message, usage)
	}
}

func TestUsageProviderCorrectionAndFallbackAcrossClients(t *testing.T) {
	for _, api := range []string{"responses", "chat_completions"} {
		for _, stream := range []bool{false, true} {
			for _, native := range []bool{false, true} {
				for _, provider := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%t/native=%t/provider=%t", api, stream, native, provider), func(t *testing.T) {
						client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) { usageReply(w, api, stream, provider) }, stream, api)
						var updates []UsageUpdate
						ctx := WithUsageObserver(context.Background(), func(u UsageUpdate) { updates = append(updates, u) })
						messages := []Message{{Role: RoleUser, Content: strings.Repeat("input", 200)}}
						var err error
						if native {
							_, err = client.ChatTools(ctx, messages, nil, nil)
						} else {
							_, err = client.Chat(ctx, messages)
						}
						if err != nil {
							t.Fatal(err)
						}
						if len(updates) < 2 {
							t.Fatalf("missing lifecycle: %+v", updates)
						}
						initial, final := updates[0], updates[len(updates)-1]
						if initial.RequestID == 0 || !initial.Estimated || initial.Final || !final.Final {
							t.Fatalf("invalid lifecycle: %+v", updates)
						}
						for _, u := range updates[:len(updates)-1] {
							if u.RequestID != final.RequestID || u.Final {
								t.Fatalf("request identity/final changed: %+v", updates)
							}
						}
						if provider {
							if final.Estimated || final.TotalTokens != 7 || final.TotalTokens >= initial.TotalTokens {
								t.Fatalf("provider did not replace estimate: %+v", updates)
							}
						} else if !final.Estimated || final.TotalTokens-initial.TotalTokens != 9 {
							t.Fatalf("output counted twice or omitted (35 bytes, 9 estimated tokens): %+v", updates)
						}
					})
				}
			}
		}
	}
}

func TestUsageCancellationRetainsStreamedPartialAttempt(t *testing.T) {
	for _, api := range []string{"responses", "chat_completions"} {
		for _, native := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/native=%t", api, native), func(t *testing.T) {
				client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					if api == "responses" {
						fmt.Fprint(w, "data: "+`{"type":"response.reasoning_text.delta","delta":"abcdefghijklmnop"}`+"\n\n")
					} else {
						fmt.Fprint(w, "data: "+`{"choices":[{"index":0,"delta":{"reasoning_content":"abcdefghijklmnop"}}]}`+"\n\n")
					}
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}, true, api)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var updates []UsageUpdate
				ctx = WithUsageObserver(ctx, func(u UsageUpdate) {
					updates = append(updates, u)
					if u.TotalTokens >= updates[0].TotalTokens+4 {
						cancel()
					}
				})
				var err error
				if native {
					_, err = client.ChatTools(ctx, nil, nil, nil)
				} else {
					err = client.ChatStream(ctx, nil, func(Delta) error { return nil })
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("partial generation did not abort: %v", err)
				}
				last := updates[len(updates)-1]
				if !last.Final || !last.Estimated || last.TotalTokens-updates[0].TotalTokens != 4 {
					t.Fatalf("cancellation discarded partial usage: %+v", updates)
				}
			})
		}
	}
}

func TestUsageInitialCancellationPreventsNetworkAdmission(t *testing.T) {
	var requests int32
	client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&requests, 1) }, true, "responses")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var updates []UsageUpdate
	ctx = WithUsageObserver(ctx, func(u UsageUpdate) { updates = append(updates, u); cancel() })
	_, err := client.ChatTools(ctx, []Message{{Role: RoleUser, Content: "question"}}, []ToolDefinition{{Type: "function", Name: "read_file", Parameters: []byte(`{"type":"object"}`)}}, nil)
	if !errors.Is(err, context.Canceled) || atomic.LoadInt32(&requests) != 0 {
		t.Fatalf("budget admitted network request: %v count=%d", err, requests)
	}
	if len(updates) != 2 || !updates[1].Final || updates[0].TotalTokens != updates[1].TotalTokens {
		t.Fatalf("rejected attempt lifecycle: %+v", updates)
	}
}

func TestUsageConcurrentRequestsHaveIndependentFinals(t *testing.T) {
	client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) { usageReply(w, "responses", false, true) }, false, "responses")
	var mu sync.Mutex
	updates := map[uint64][]UsageUpdate{}
	ctx := WithUsageObserver(context.Background(), func(u UsageUpdate) { mu.Lock(); updates[u.RequestID] = append(updates[u.RequestID], u); mu.Unlock() })
	const count = 12
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.Chat(ctx, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(updates) != count {
		t.Fatalf("request IDs collided: got %d", len(updates))
	}
	for id, events := range updates {
		finals := 0
		for _, u := range events {
			if u.Final {
				finals++
				if u.TotalTokens != 7 || u.Estimated {
					t.Fatalf("request %d inherited another request's usage: %+v", id, u)
				}
			}
		}
		if finals != 1 {
			t.Fatalf("request %d final count=%d", id, finals)
		}
	}
}

func TestUsageFailedAttemptAndRetryKeepSeparateTotals(t *testing.T) {
	for _, api := range []string{"responses", "chat_completions"} {
		t.Run(api, func(t *testing.T) {
			var request int32
			client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if atomic.AddInt32(&request, 1) == 1 {
					if api == "responses" {
						fmt.Fprint(w, `{"status":"incomplete","usage":{"input_tokens":5,"output_tokens":8}}`)
					} else {
						fmt.Fprint(w, `{"choices":[{"message":{"content":"partial"},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":8}}`)
					}
					return
				}
				usageReply(w, api, false, true)
			}, false, api)
			var finals []UsageUpdate
			ctx := WithUsageObserver(context.Background(), func(u UsageUpdate) {
				if u.Final {
					finals = append(finals, u)
				}
			})
			if result, err := client.ChatTools(ctx, nil, nil, nil); err == nil || result.Content != "" {
				t.Fatalf("incomplete attempt accepted: %+v %v", result, err)
			}
			if _, err := client.ChatTools(ctx, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if len(finals) != 2 || finals[0].RequestID == finals[1].RequestID || finals[0].TotalTokens != 13 || finals[1].TotalTokens != 7 || finals[0].Estimated || finals[1].Estimated {
				t.Fatalf("failed/retried usage lost or inherited: %+v", finals)
			}
		})
	}
}

func TestUsageFinalCallbackCancellationDiscardsResult(t *testing.T) {
	client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) { usageReply(w, "responses", false, false) }, false, "responses")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = WithUsageObserver(ctx, func(u UsageUpdate) {
		if u.Final {
			cancel()
		}
	})
	result, err := client.ChatTools(ctx, nil, nil, nil)
	if !errors.Is(err, context.Canceled) || len(result.Calls) != 0 {
		t.Fatalf("final cancellation released calls: %+v %v", result, err)
	}
}
