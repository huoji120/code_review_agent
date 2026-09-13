package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"code-review-agent/internal/llm"
	"code-review-agent/internal/tools"
)

func reportRaw(title, evidence string) json.RawMessage {
	b, _ := json.Marshal(tools.Finding{Title: title, Path: "entry1.go", Line: 10, Evidence: evidence, Impact: "memory corruption"})
	return b
}

func TestReportQueueSeesPriorCommitAndKeepsDifferentRootCause(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	compared := []string{}
	client := &ircClient{call: func(ctx context.Context, m []llm.Message, d []llm.ToolDefinition, emit func(llm.Delta) error) (llm.ToolResponse, error) {
		var pair struct {
			Candidate tools.Finding `json:"candidate"`
			Existing  tools.Finding `json:"existing"`
		}
		if err := json.Unmarshal([]byte(m[1].Content), &pair); err != nil {
			return llm.ToolResponse{}, err
		}
		mu.Lock()
		compared = append(compared, pair.Candidate.Title)
		n := len(compared)
		mu.Unlock()
		if n == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return llm.ToolResponse{}, ctx.Err()
			}
		}
		decision := "distinct"
		if strings.HasSuffix(pair.Existing.Evidence, pair.Candidate.Evidence) {
			decision = "duplicate"
		}
		return toolReply("finding_duplicate_verdict", map[string]any{"decision": decision, "existing_key": pair.Existing.Key, "reason": "compare root cause and input path"}), nil
	}}
	team := newTestTeam(t, client)
	team.phase = phaseAudit
	team.createStageLocked(phaseAudit)
	submit := func(i int, title, evidence string) string {
		_, full := team.workers[i].agent.callTool(context.Background(), func(Event) {}, ToolCall{Name: "report_finding", Arguments: reportRaw(title, evidence)})
		return full
	}
	if result := submit(0, "original", "root-A"); !strings.Contains(result, "finding recorded") {
		t.Fatal(result)
	}
	second := make(chan string, 1)
	third := make(chan string, 1)
	go func() { second <- submit(1, "renamed duplicate", "root-A") }()
	waitSignal(t, entered)
	go func() { third <- submit(2, "original", "root-B") }()
	waitReportQueue(t, team, 2)
	close(release)
	if result := <-second; !strings.Contains(result, `"status":"duplicate"`) {
		t.Fatal(result)
	}
	if result := <-third; !strings.Contains(result, "finding recorded") {
		t.Fatal(result)
	}
	if result := submit(3, "later duplicate of B", "root-B"); !strings.Contains(result, `"status":"duplicate"`) {
		t.Fatal(result)
	}
	if findings := team.Snapshot().Findings; len(findings) != 2 {
		t.Fatalf("findings=%+v", findings)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(compared) != 4 || compared[0] != "renamed duplicate" || compared[1] != "original" {
		t.Fatalf("review order=%v", compared)
	}
}

func waitReportQueue(t *testing.T, team *Team, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		team.reportMu.Lock()
		got := len(team.reportQueue)
		team.reportMu.Unlock()
		if got == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("queue did not reach expected length")
}

func TestReportQueueFIFOAndCancellation(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	first, err := team.acquireReportSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	go func() {
		release, err := team.acquireReportSlot(ctx)
		if release != nil {
			release()
		}
		cancelled <- err
	}()
	waitReportQueue(t, team, 2)
	order := make(chan int, 2)
	finishSecond := make(chan struct{})
	go func() {
		release, err := team.acquireReportSlot(context.Background())
		if err != nil {
			panic(err)
		}
		defer release()
		order <- 2
		<-finishSecond
	}()
	waitReportQueue(t, team, 3)
	go func() {
		release, err := team.acquireReportSlot(context.Background())
		if err != nil {
			panic(err)
		}
		defer release()
		order <- 3
	}()
	waitReportQueue(t, team, 4)
	cancel()
	if err := <-cancelled; err != context.Canceled {
		t.Fatal(err)
	}
	waitReportQueue(t, team, 3)
	first()
	if got := <-order; got != 2 {
		t.Fatalf("not FIFO: %d", got)
	}
	select {
	case got := <-order:
		t.Fatalf("parallel admission: %d", got)
	default:
	}
	close(finishSecond)
	if got := <-order; got != 3 {
		t.Fatalf("not FIFO: %d", got)
	}
	waitReportQueue(t, team, 0)
}

func TestMalformedOrUncertainReviewDoesNotPublish(t *testing.T) {
	for _, mode := range []string{"uncertain", "wrong_key", "transport_error"} {
		t.Run(mode, func(t *testing.T) {
			client := &ircClient{call: func(ctx context.Context, m []llm.Message, d []llm.ToolDefinition, emit func(llm.Delta) error) (llm.ToolResponse, error) {
				if mode == "transport_error" {
					return llm.ToolResponse{}, fmt.Errorf("provider failed")
				}
				var pair struct {
					Existing tools.Finding `json:"existing"`
				}
				json.Unmarshal([]byte(m[1].Content), &pair)
				key := pair.Existing.Key
				if mode == "wrong_key" {
					key = "invented"
				}
				return toolReply("finding_duplicate_verdict", map[string]any{"decision": "uncertain", "existing_key": key, "reason": "insufficient evidence"}), nil
			}}
			team := newTestTeam(t, client)
			team.phase = phaseAudit
			team.createStageLocked(phaseAudit)
			team.reviewFindingReport(context.Background(), team.workers[0], reportRaw("seed", "A"))
			result := team.reviewFindingReport(context.Background(), team.workers[1], reportRaw("candidate", "B"))
			if !strings.Contains(result, `"ok":false`) || len(team.Snapshot().Findings) != 1 {
				t.Fatalf("failed review published: %s", result)
			}
			waitReportQueue(t, team, 0)
		})
	}
}
