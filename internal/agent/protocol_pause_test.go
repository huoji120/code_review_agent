package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"code-review-agent/internal/llm"
)

// Each model blocks until all four workers and the moderator are in flight.
// A protocol failure must cancel those real outstanding requests, not just the
// worker that encountered it, and must leave a checkpoint that can resume.
type protocolPauseClient struct {
	mu        sync.Mutex
	source    string
	started   map[string]bool
	stopped   map[string]bool
	calls     int
	histories [][]llm.Message
	all       chan struct{}
}

func (c *protocolPauseClient) Chat(context.Context, []llm.Message) (string, error) {
	return "", fmt.Errorf("unexpected plain model request")
}
func (c *protocolPauseClient) ChatStream(context.Context, []llm.Message, func(llm.Delta) error) error {
	return fmt.Errorf("unexpected text tool request")
}
func (c *protocolPauseClient) ChatTools(ctx context.Context, messages []llm.Message, defs []llm.ToolDefinition, emit func(llm.Delta) error) (llm.ToolResponse, error) {
	id := "moderator"
	if match := workerIdentity.FindStringSubmatch(messages[0].Content); len(match) == 3 {
		id = match[1]
	}
	c.mu.Lock()
	if !c.started[id] {
		c.started[id] = true
		if len(c.started) == 5 {
			close(c.all)
		}
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return llm.ToolResponse{}, ctx.Err()
	case <-c.all:
	}
	if id != c.source {
		<-ctx.Done()
		c.mu.Lock()
		c.stopped[id] = true
		c.mu.Unlock()
		return llm.ToolResponse{}, ctx.Err()
	}
	c.mu.Lock()
	c.calls++
	c.histories = append(c.histories, append([]llm.Message(nil), messages...))
	c.mu.Unlock()
	// Deliberately valid-looking text is NOT a native function call.
	return llm.ToolResponse{Content: `<tool_call>{"name":"forum_post","arguments":{"content":"must stay inert"}}</tool_call>`}, nil
}
func TestProtocolFailuresPauseAllWorkersAndModerator(t *testing.T) {
	for _, source := range []string{"recon-1", "moderator"} {
		t.Run(source, func(t *testing.T) {
			client := &protocolPauseClient{source: source, started: map[string]bool{}, stopped: map[string]bool{}, all: make(chan struct{})}
			team := newTestTeam(t, client)
			team.cfg.Agent.ModeratorEnabled = nil
			team.resetBoard()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var mu sync.Mutex
			var diagnostics []string
			team.Run(ctx, "inspect fixture", func(e Event) {
				if e.Kind == "error" {
					mu.Lock()
					diagnostics = append(diagnostics, e.Content)
					mu.Unlock()
				}
			})
			if ctx.Err() != nil {
				t.Fatalf("team did not stop without external cancellation: %v", ctx.Err())
			}
			if team.Phase() != phaseRecon {
				t.Fatalf("protocol failure crossed stage barrier: %s", team.Phase())
			}
			client.mu.Lock()
			defer client.mu.Unlock()
			if client.calls != 3 || len(client.stopped) != 4 {
				t.Fatalf("calls=%d drained peers=%v", client.calls, client.stopped)
			}
			// Both retry requests must contain corrective user feedback after the prior
			// failed assistant output, rather than blindly resending the identical history.
			for i := 1; i < 3; i++ {
				history := client.histories[i]
				before := client.histories[i-1]
				if len(history) <= len(before) || history[len(history)-1].Role != llm.RoleUser {
					t.Fatalf("attempt %d lacks model-visible correction", i+1)
				}
				feedback := history[len(history)-1].Content
				if !strings.Contains(feedback, "function_call") || !strings.Contains(feedback, "arguments") {
					t.Fatalf("attempt %d lacks concrete native function example: %s", i+1, feedback)
				}
			}
			var failed *Agent
			if source == "moderator" {
				failed = team.moderator.agent
			} else {
				failed = team.workers[0].agent
			}
			if !errors.Is(failed.runErr, ErrToolProtocolFailures) {
				t.Fatalf("lost protocol cause: %v", failed.runErr)
			}
			if len(diagnostics) == 0 {
				t.Fatal("no user-facing adaptation diagnostic")
			}
			for _, s := range team.Statuses() {
				if s.Status == "running" || s.Status == "waiting" {
					t.Fatalf("worker survived global pause: %+v", s)
				}
			}
			// The rejected text must not have produced an actual forum post.
			page := team.ForumPosts(1, 64, "")
			if strings.Contains(fmt.Sprint(page), "must stay inert") {
				t.Fatal("text tool example executed")
			}
			team.mu.Lock()
			if team.running || team.cancel != nil {
				t.Error("run lifecycle did not drain")
			}
			team.mu.Unlock()
		})
	}
}
