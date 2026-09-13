package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"code-review-agent/internal/config"
	"code-review-agent/internal/llm"
)

func TestBillingFailureImmediatelyCancelsWholeTeam(t *testing.T) {
	for _, code := range []int{402, 429, 200} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var requests, stopped atomic.Int32
			all := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				n := requests.Add(1)
				if n == 5 {
					close(all)
				}
				select {
				case <-all:
				case <-r.Context().Done():
					return
				}
				if n == 1 {
					if code == 200 {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"type\":\"error\",\"error\":{\"code\":\"insufficient_quota\",\"message\":\"Please check your account\"}}\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(code)
					fmt.Fprint(w, `{"error":{"code":"insufficient_quota","message":"账户余额不足"}}`)
					return
				}
				<-r.Context().Done()
				stopped.Add(1)
			}))
			defer server.Close()
			client := llm.NewOpenAIClient(config.OpenAIConfig{BaseURL: server.URL, APIKey: "fixture", APIInterface: "responses", Model: "fixture", Stream: true, TimeoutSeconds: 3})
			team := newTestTeam(t, client)
			team.cfg.Agent.ModeratorEnabled = nil
			team.resetBoard()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var notified atomic.Bool
			team.Run(ctx, "inspect", func(e Event) {
				if e.Kind == "error" && strings.Contains(e.Content, "API余额或额度不足") {
					notified.Store(true)
				}
			})
			if ctx.Err() != nil {
				t.Fatalf("billing did not immediately cancel team: %v", ctx.Err())
			}
			if !notified.Load() || requests.Load() != 5 {
				t.Fatalf("missing notice or retried: notice=%v requests=%d", notified.Load(), requests.Load())
			}
			deadline := time.After(time.Second)
			for stopped.Load() != 4 {
				select {
				case <-deadline:
					t.Fatalf("inflight requests not canceled: %d", stopped.Load())
				case <-time.After(time.Millisecond):
				}
			}
			if team.running || team.Phase() != phaseRecon {
				t.Fatal("billing incorrectly completed or continued team")
			}
		})
	}
}

func TestBillingDetectionDoesNotTreatRateLimitAsExhaustion(t *testing.T) {
	for _, text := range []string{"openai status 429: rate_limit_exceeded", "openai status 503: service unavailable", "maximum context length exceeded"} {
		if isBillingExhausted(fmt.Errorf("%s", text)) {
			t.Fatalf("nonbilling error classified as exhausted: %s", text)
		}
	}
}
