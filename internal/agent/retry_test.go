package agent

import (
	"code-review-agent/internal/llm"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

func TestModelDisconnectCooldownProbeAndCancellation(t *testing.T) {
	for _, scenario := range []string{"recover", "probe_failure", "cancel_cooldown"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			requests, waits, stops := 0, 0, 0
			a := &Agent{onDisconnect: func(error) { stops++ }, waitRetry: func(ctx context.Context, delay time.Duration) error {
				waits++
				if requests != 3 || delay != 5*time.Minute {
					t.Fatalf("cooldown after %d requests, delay %v", requests, delay)
				}
				if scenario == "cancel_cooldown" {
					cancel()
					return ctx.Err()
				}
				return nil
			}}
			got, err := a.modelRequest(ctx, nil, func() (string, error) {
				requests++
				if requests == 4 && scenario == "recover" {
					return "complete", nil
				}
				return "partial must never execute", io.ErrUnexpectedEOF
			})
			if waits != 1 {
				t.Fatalf("waits=%d", waits)
			}
			if scenario == "recover" {
				if got != "complete" || err != nil || requests != 4 || stops != 0 {
					t.Fatalf("recovery=%q,%v requests=%d stops=%d", got, err, requests, stops)
				}
			} else if scenario == "probe_failure" {
				if got != "" || err == nil || requests != 4 || stops != 1 {
					t.Fatalf("probe failure=%q,%v requests=%d stops=%d", got, err, requests, stops)
				}
			} else if !errors.Is(err, context.Canceled) || requests != 3 || stops != 0 {
				t.Fatalf("cancellation=%v requests=%d stops=%d", err, requests, stops)
			}
		})
	}
}

func TestNonTransportFailuresNeverEnterCooldown(t *testing.T) {
	a := &Agent{waitRetry: func(context.Context, time.Duration) error {
		t.Fatal("non-transport failure entered cooldown")
		return nil
	}, onDisconnect: func(error) { t.Fatal("non-transport failure cancelled team") }}
	for _, failure := range []error{errors.New("达到 max_turns"), errors.New("maximum context length exceeded"), errors.New("openai status 401: unauthorized"), errors.New("max_output_tokens incomplete")} {
		calls := 0
		_, err := a.modelRequest(context.Background(), nil, func() (string, error) { calls++; return "partial", failure })
		if !errors.Is(err, failure) || calls != 1 {
			t.Fatalf("error=%v calls=%d", err, calls)
		}
	}
}

func TestCooldownWaitRespondsToCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitModelRetry(ctx, 5*time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error=%v", err)
	}
}

func TestDisconnectCancelsWholeTeamAndGoResumes(t *testing.T) {
	var offline atomic.Bool
	offline.Store(true)
	ordinary := newStagedClient(4, 4)
	client := &noticeClient{tools: func(ctx context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		if offline.Load() {
			return llm.ToolResponse{}, io.ErrUnexpectedEOF
		}
		return ordinary.ChatTools(ctx, messages, nil, nil)
	}}
	team := newTestTeam(t, client)
	team.createStageLocked(phaseRecon)
	for _, w := range team.workers {
		w.agent.waitRetry = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	team.Run(ctx, "inspect", func(Event) {})
	if ctx.Err() != nil || team.Phase() != phaseRecon || team.running {
		t.Fatalf("team did not drain failed probe: phase=%s err=%v", team.Phase(), ctx.Err())
	}
	for _, w := range team.workers {
		if w.saved.Completed {
			t.Fatal("disconnect marked unfinished worker complete")
		}
	}
	offline.Store(false)
	team.Run(ctx, "go", func(Event) {})
	if ctx.Err() != nil || team.Phase() != "completed" {
		t.Fatalf("go did not resume persisted worker histories: phase=%s err=%v", team.Phase(), ctx.Err())
	}
}
