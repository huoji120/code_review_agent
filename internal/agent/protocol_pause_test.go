package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"code-review-agent/internal/llm"
)

func TestWorkerProtocolFailureIsolatedAndDeliveredToModerator(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	failure := make(chan struct{})
	observed := make(chan struct{})
	advanced := make(chan string, 3)
	var posted, seen sync.Once
	client := &ircClient{call: func(ctx context.Context, messages []llm.Message, _ []llm.ToolDefinition, _ func(llm.Delta) error) (llm.ToolResponse, error) {
		id := "moderator"
		if match := workerIdentity.FindStringSubmatch(messages[0].Content); len(match) == 3 {
			id = match[1]
		}
		mu.Lock()
		calls[id]++
		n := calls[id]
		mu.Unlock()
		if id == "recon-1" {
			return llm.ToolResponse{Content: "ordinary answer without required tool"}, nil
		}
		if id == "moderator" {
			if n == 1 {
				select {
				case <-failure:
				case <-ctx.Done():
					return llm.ToolResponse{}, ctx.Err()
				}
				return toolReply("moderator_idle", map[string]any{}), nil
			}
			for _, message := range messages {
				if strings.Contains(message.Content, "Agent 失败待诊断") && strings.Contains(message.Content, "recon-1") {
					seen.Do(func() { close(observed) })
				}
			}
			return toolReply("moderator_idle", map[string]any{}), nil
		}
		if n == 1 {
			select {
			case <-failure:
			case <-ctx.Done():
				return llm.ToolResponse{}, ctx.Err()
			}
			return toolReply("read_handoff", map[string]any{}), nil
		}
		if n == 2 {
			advanced <- id
		}
		<-ctx.Done()
		return llm.ToolResponse{}, ctx.Err()
	}}
	team := newTestTeam(t, client)
	team.cfg.Agent.ModeratorEnabled = nil
	team.resetBoard()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		team.Run(ctx, "inspect", func(e Event) {
			if e.Kind == "forum" && e.Forum != nil && e.Forum.Topic == "Agent 失败待诊断" {
				if e.Forum.To != "moderator" || !strings.Contains(e.Forum.Content, "没有原生工具调用") {
					t.Error("failure notification lost recipient or cause")
				}
				posted.Do(func() { close(failure) })
			}
		})
	}()
	defer func() { cancel(); <-done }()
	for i := 0; i < 3; i++ {
		select {
		case <-advanced:
		case <-ctx.Done():
			t.Fatal("healthy workers stopped after peer failure")
		}
	}
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal("moderator did not receive failure notification")
	}
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
	found := false
	for _, s := range team.Statuses() {
		if s.ID == "recon-1" {
			found = s.Status == "failed"
		}
	}
	if !found {
		t.Fatalf("failed worker status missing: %+v", team.Statuses())
	}
	if team.Phase() != phaseRecon {
		t.Fatal("failed member treated as completed")
	}
	mu.Lock()
	attempts := calls["recon-1"]
	mu.Unlock()
	if attempts != 3 {
		t.Fatalf("protocol attempts=%d", attempts)
	}
}
