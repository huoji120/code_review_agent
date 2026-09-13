package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func streamTestEvents(api string) (string, string) {
	if api == "responses" {
		return `{"type":"response.output_text.delta","delta":"answer"}`, `{"type":"response.completed","response":{"status":"completed"}}`
	}
	return `{"choices":[{"delta":{"content":"answer"}}]}`, "[DONE]"
}

func TestStreamHeartbeatBytesOutliveRequestTimeout(t *testing.T) {
	for _, api := range []string{"responses", "chat_completions"} {
		t.Run(api, func(t *testing.T) {
			const timeout = 150 * time.Millisecond
			client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				// Keep one comment line open: activity must be observed below the
				// SSE scanner, even when no complete line or token is available.
				fmt.Fprint(w, ": heartbeat")
				w.(http.Flusher).Flush()
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				finish := time.NewTimer(3 * timeout)
				defer finish.Stop()
				for {
					select {
					case <-r.Context().Done():
						return
					case <-ticker.C:
						fmt.Fprint(w, " ")
						w.(http.Flusher).Flush()
					case <-finish.C:
						delta, terminal := streamTestEvents(api)
						fmt.Fprintf(w, "\n\ndata: %s\n\ndata: %s\n\n", delta, terminal)
						return
					}
				}
			}, true, api)
			// Internal injection exercises the production timeout source without
			// integer-second delays or a public testing-only configuration knob.
			client.httpClient.Timeout = timeout
			got, err := client.Chat(context.Background(), nil)
			if err != nil || got != "answer" {
				t.Fatalf("active stream = %q, %v", got, err)
			}
		})
	}
}

func TestStreamIdleTimeoutBoundsHeadersAndBody(t *testing.T) {
	for _, api := range []string{"responses", "chat_completions"} {
		for _, phase := range []string{"headers", "body"} {
			t.Run(api+"/"+phase, func(t *testing.T) {
				stopped := make(chan struct{})
				client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					defer close(stopped)
					io.Copy(io.Discard, r.Body)
					if phase == "body" {
						delta, _ := streamTestEvents(api)
						fmt.Fprintf(w, "data: %s\n\n", delta)
						w.(http.Flusher).Flush()
					}
					select {
					case <-r.Context().Done():
					case <-time.After(3 * time.Second):
					}
				}, true, api)
				client.httpClient.Timeout = 100 * time.Millisecond
				got, err := client.Chat(context.Background(), nil)
				var timeout net.Error
				if got != "" || !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Fatalf("stalled stream = %q, %v", got, err)
				}
				select {
				case <-stopped:
				case <-time.After(time.Second):
					t.Fatal("timed-out request remained open on server")
				}
			})
		}
	}
}

func TestStreamParentCancellationInterruptsHeadersAndBody(t *testing.T) {
	for _, api := range []string{"responses", "chat_completions"} {
		for _, phase := range []string{"headers", "body"} {
			t.Run(api+"/"+phase, func(t *testing.T) {
				ready := make(chan struct{})
				stopped := make(chan struct{})
				client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					defer close(stopped)
					io.Copy(io.Discard, r.Body)
					if phase == "body" {
						fmt.Fprint(w, ": heartbeat\n\n")
						w.(http.Flusher).Flush()
					}
					close(ready)
					select {
					case <-r.Context().Done():
					case <-time.After(3 * time.Second):
					}
				}, true, api)
				client.httpClient.Timeout = 5 * time.Second
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				result := make(chan error, 1)
				go func() { result <- client.ChatStream(ctx, nil, func(Delta) error { return nil }) }()
				select {
				case <-ready:
				case <-time.After(time.Second):
					t.Fatal("request did not reach server")
				}
				cancel()
				select {
				case err := <-result:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancelled stream = %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("parent cancellation waited for idle timeout")
				}
				select {
				case <-stopped:
				case <-time.After(time.Second):
					t.Fatal("cancelled request remained open on server")
				}
			})
		}
	}
}

func TestStreamEarlyReturnClosesBody(t *testing.T) {
	for _, api := range []string{"responses", "chat_completions"} {
		for _, outcome := range []string{"completed", "callback_error", "malformed"} {
			t.Run(api+"/"+outcome, func(t *testing.T) {
				stopped := make(chan struct{})
				client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					defer close(stopped)
					io.Copy(io.Discard, r.Body)
					delta, terminal := streamTestEvents(api)
					if outcome == "malformed" {
						delta = "{"
					}
					fmt.Fprintf(w, "data: %s\n\n", delta)
					if outcome == "completed" {
						fmt.Fprintf(w, "data: %s\n\n", terminal)
					}
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
					case <-time.After(3 * time.Second):
					}
				}, true, api)
				client.httpClient.Timeout = time.Second
				stop := errors.New("consumer stopped")
				var content strings.Builder
				err := client.ChatStream(context.Background(), nil, func(delta Delta) error {
					if outcome == "callback_error" {
						return stop
					}
					content.WriteString(delta.Content)
					return nil
				})
				switch outcome {
				case "completed":
					if err != nil || content.String() != "answer" {
						t.Fatalf("completed stream = %q, %v", content.String(), err)
					}
				case "callback_error":
					if !errors.Is(err, stop) {
						t.Fatalf("callback error = %v", err)
					}
				case "malformed":
					if err == nil || content.String() != "" {
						t.Fatalf("malformed stream = %q, %v", content.String(), err)
					}
				}
				select {
				case <-stopped:
				case <-time.After(time.Second):
					t.Fatal("returned stream retained its response body")
				}
			})
		}
	}
}

func TestChatCompletionsEOFDiscardsPartialOutput(t *testing.T) {
	client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"<tool_call>partial</tool_call>"}}]}`+"\n\n")
	}, true, "chat_completions")
	got, err := client.Chat(context.Background(), nil)
	if got != "" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("unterminated stream exposed partial text: %q, %v", got, err)
	}
}

func TestNonStreamRetainsWholeRequestTimeout(t *testing.T) {
	for _, api := range []string{"responses", "chat_completions"} {
		t.Run(api, func(t *testing.T) {
			client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-r.Context().Done():
						return
					case <-ticker.C:
						if _, err := fmt.Fprint(w, " "); err != nil {
							return
						}
						w.(http.Flusher).Flush()
					}
				}
			}, false, api)
			client.httpClient.Timeout = 100 * time.Millisecond
			got, err := client.Chat(context.Background(), nil)
			if got != "" || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("nonstream request = %q, %v", got, err)
			}
		})
	}
}
