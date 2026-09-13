package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNativeProgressPreventsStallButHeartbeatsDoNot(t *testing.T) {
	for _, api := range []string{"responses", "chat_completions"} {
		for _, progressing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/progress=%v", api, progressing), func(t *testing.T) {
				client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					ticker := time.NewTicker(30 * time.Millisecond)
					defer ticker.Stop()
					for n := 0; n < 12; n++ {
						select {
						case <-r.Context().Done():
							return
						case <-ticker.C:
						}
						if progressing {
							if api == "responses" {
								fmt.Fprint(w, "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"x\"}\n\n")
							} else {
								fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"x\"}}]}\n\n")
							}
						} else {
							fmt.Fprint(w, ": heartbeat\n\n")
						}
						w.(http.Flusher).Flush()
					}
					if api == "responses" {
						fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
					} else {
						fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					}
				}, true, api)
				client.httpClient.Timeout = 150 * time.Millisecond
				response, err := client.ChatTools(context.Background(), nil, nil, nil)
				if progressing {
					if err != nil || response.Thinking != strings.Repeat("x", 12) {
						t.Fatalf("healthy generation timed out: %+v %v", response, err)
					}
				} else if !errors.Is(err, ErrIncompleteGeneration) || len(response.Calls) != 0 {
					t.Fatalf("heartbeat stall escaped: %+v %v", response, err)
				}
			})
		}
	}
}
