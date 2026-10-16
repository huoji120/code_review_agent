package agent

import (
	"context"
	"testing"

	"code-review-agent/internal/llm"
)

func TestWorkerProtocolFailureDoesNotKillAgent(t *testing.T) {
	calls := 0
	client := &noticeClient{tools: func(_ context.Context, _ []llm.Message) (llm.ToolResponse, error) {
		calls++
		if calls <= 5 {
			return llm.ToolResponse{Calls: []llm.FunctionCall{{Name: "forum_post", Arguments: `{"content":"unsafe"}`}}}, nil
		}
		return toolReply("audit_plan_done", map[string]any{
			"summary":     "recovered",
			"audit_map":   "entry",
			"audit_files": []string{"entry1.go"},
		}), nil
	}}
	team := newTestTeam(t, client)
	team.createStageLocked(phaseRecon)
	agent := team.workers[0].agent
	agent.Run(context.Background(), "inspect", func(Event) {})
	if calls != 6 || agent.runErr != nil || !agent.completed || agent.protocolFailures != 0 {
		t.Fatalf("protocol recovery calls=%d err=%v completed=%v streak=%d", calls, agent.runErr, agent.completed, agent.protocolFailures)
	}
}
