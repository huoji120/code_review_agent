package agent

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestModelDisconnectRetriesUntilCancellationWithIncreasingBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits []time.Duration
	requests := 0
	a := &Agent{onDisconnect: func(error) { t.Fatal("network failure cancelled team") }, waitRetry: func(ctx context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		if len(waits) == 8 {
			cancel()
		}
		return ctx.Err()
	}}
	_, err := a.modelRequest(ctx, nil, func() (string, error) {
		requests++
		return "", io.ErrUnexpectedEOF
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", err)
	}
	if requests != 8 || len(waits) != 8 {
		t.Fatalf("requests=%d waits=%d, want eight network attempts", requests, len(waits))
	}
	for i := 1; i < len(waits); i++ {
		if waits[i] <= waits[i-1] {
			t.Fatalf("backoff is not strictly increasing: %v", waits)
		}
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

func TestRetryWaitRespondsToCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitModelRetry(ctx, 5*time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error=%v", err)
	}
}

func TestBillingExhaustionStopsWithoutRetry(t *testing.T) {
	calls := 0
	waits := 0
	stops := 0
	a := &Agent{
		onDisconnect: func(error) { stops++ },
		waitRetry:    func(context.Context, time.Duration) error { waits++; return nil },
	}
	_, err := a.modelRequest(context.Background(), nil, func() (string, error) {
		calls++
		return "", errors.New("openai status 402: payment required")
	})
	if err == nil || calls != 1 || waits != 0 || stops != 1 {
		t.Fatalf("billing retry behavior: err=%v calls=%d waits=%d stops=%d", err, calls, waits, stops)
	}
}

func TestModelRetryBackoffSaturates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits []time.Duration
	a := &Agent{waitRetry: func(ctx context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		if len(waits) == 20 {
			cancel()
		}
		return ctx.Err()
	}}
	_, err := a.modelRequest(ctx, nil, func() (string, error) { return "", io.EOF })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", err)
	}
	if len(waits) != 20 || waits[len(waits)-1] != modelRetryMaxDelay {
		t.Fatalf("backoff did not saturate safely: %v", waits)
	}
	for _, delay := range waits {
		if delay <= 0 || delay > modelRetryMaxDelay {
			t.Fatalf("invalid retry delay %v", delay)
		}
	}
}
