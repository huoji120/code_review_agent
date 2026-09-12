package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"code-review-agent/internal/config"
	"code-review-agent/internal/llm"
	"code-review-agent/internal/prompt"
	"code-review-agent/internal/tools"
)

func tokenBudgetWorker(t *testing.T, main, compressor llm.Client) *Agent {
	t.Helper()
	registry, err := tools.NewRegistry(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { registry.Close() })
	cfg := config.Config{
		OpenAI:         config.OpenAIConfig{MaxContextTokens: 128000, MaxOutputTokens: 1000},
		CompressOpenAI: config.OpenAIConfig{MaxContextTokens: 16000, MaxOutputTokens: 1000},
		Agent:          config.AgentConfig{MaxTurns: 120, RetryAttempts: -1, CompressAtRatio: .75, MaxToolResultChars: 1024},
	}
	prompts := prompt.Prompts{System: "Audit", Compress: "Summarize evidence", Templates: map[string]string{
		"initial_audit_instruction": "!{input}",
		"no_tool_retry":             "Continue with a tool",
		"compress_user":             "Summarize this:\n!{state_and_conversation}",
		"state_after_compress":      "!{state}",
		"resume_after_compress":     "Continue remaining work",
	}}
	return newWorker(cfg, prompts, main, compressor, registry, "budget-worker", phaseAudit, nil)
}

func TestManySmallTurnsKeepHistoryPrefixWithoutPeriodicCompaction(t *testing.T) {
	calls := 0
	var prior []llm.Message
	var a *Agent
	client := &noticeClient{call: func(_ context.Context, messages []llm.Message) (string, error) {
		calls++
		if len(messages) < len(prior) || !reflect.DeepEqual(messages[:len(prior)], prior) && len(prior) > 0 {
			t.Fatal("ordinary turn rewrote or dropped an earlier history prefix")
		}
		prior = append([]llm.Message(nil), messages...)
		// Changed metadata must not rewrite an existing request prefix.
		a.prompts.System = "new metadata for the next compressed prefix"
		return "continue", nil
	}}
	compressor := &noticeClient{call: func(context.Context, []llm.Message) (string, error) {
		t.Fatal("small turns triggered periodic compression")
		return "", nil
	}}
	a = tokenBudgetWorker(t, client, compressor)
	a.Run(context.Background(), "inspect", func(Event) {})
	if calls != 120 {
		t.Fatalf("did not exercise many small turns: %d (%v)", calls, a.runErr)
	}
}

func TestTokenThresholdExactBoundaryAndStablePrefix(t *testing.T) {
	calls := 0
	compressor := &noticeClient{call: func(context.Context, []llm.Message) (string, error) {
		calls++
		return "Keep checking entry.go", nil
	}}
	a := tokenBudgetWorker(t, compressor, compressor)
	limit, err := a.cfg.CompressionThreshold()
	if err != nil {
		t.Fatal(err)
	}
	a.messages = []llm.Message{{Role: llm.RoleSystem, Content: "fixed prefix"}, {Role: llm.RoleUser}}
	padding := limit - 1 - estimateTokens(a.messages)
	a.messages[1].Content = strings.Repeat("界", padding)
	before := append([]llm.Message(nil), a.messages...)
	if estimateTokens(a.messages) != limit-1 {
		t.Fatal("boundary fixture has wrong estimate")
	}
	if err := a.compressIfNeeded(context.Background(), func(Event) {}); err != nil || calls != 0 || !reflect.DeepEqual(a.messages, before) {
		t.Fatalf("T-1 changed history or compressed: %v", err)
	}
	a.messages[1].Content += "界"
	if err := a.compressIfNeeded(context.Background(), func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || estimateTokens(a.messages) >= limit {
		t.Fatal("T did not compact to below admission limit")
	}
	if err := a.compressIfNeeded(context.Background(), func(Event) {}); err != nil || calls != 1 {
		t.Fatalf("compact result immediately retriggered: %v", err)
	}
}

func TestRenderedCompressionRequestAdmittedWithinOutputBudget(t *testing.T) {
	var a *Agent
	calls := 0
	compressor := &noticeClient{call: func(_ context.Context, messages []llm.Message) (string, error) {
		calls++
		if len(messages) != 2 || estimateTokens(messages)+a.cfg.CompressOpenAI.MaxOutputTokens > a.cfg.CompressOpenAI.MaxContextTokens {
			t.Fatal("rendered compression request exceeded budget including output")
		}
		return "next evidence", nil
	}}
	a = tokenBudgetWorker(t, compressor, compressor)
	a.cfg.CompressOpenAI.MaxContextTokens = 6000
	a.prompts.Templates["compress_user"] = strings.Repeat("界", 200) + "!{state_and_conversation}\n!{state_and_conversation}"
	for i := 0; i < 40; i++ {
		a.messages = append(a.messages, llm.Message{Role: llm.RoleAssistant, Content: strings.Repeat("证", 100)})
	}
	if err := a.compressContext(context.Background(), func(Event) {}, "budget test"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("rendered request did not reach summarizer")
	}
}

func TestFailedCompactionPreservesExactHistoryAndPendingNotice(t *testing.T) {
	for _, failure := range []string{"base request", "model failure", "replacement overflow", "empty summary"} {
		t.Run(failure, func(t *testing.T) {
			calls := 0
			compressor := &noticeClient{call: func(context.Context, []llm.Message) (string, error) {
				calls++
				if failure == "model failure" {
					return "", errors.New("summarizer unavailable")
				}
				if failure == "empty summary" {
					return "", nil
				}
				return "summary", nil
			}}
			a := tokenBudgetWorker(t, compressor, compressor)
			a.forumPending = "Forum notification:\n exact pending text  "
			a.messages = []llm.Message{{Role: llm.RoleSystem, Content: "original prefix"}, {Role: llm.RoleAssistant, Content: "<think>private reasoning</think>evidence"}, {Role: llm.RoleUser, Content: a.forumPending}}
			before := append([]llm.Message(nil), a.messages...)
			if failure == "base request" {
				a.prompts.Templates["compress_user"] = strings.Repeat("界", 16001) + "!{state_and_conversation}"
			}
			if failure == "replacement overflow" {
				a.prompts.Templates["resume_after_compress"] = strings.Repeat("界", 128001)
			}
			if err := a.compressContext(context.Background(), func(Event) {}, "failure test"); err == nil {
				t.Fatal("invalid compaction unexpectedly succeeded")
			}
			if !reflect.DeepEqual(a.messages, before) || a.forumPending != before[2].Content {
				t.Fatal("failed compaction changed authoritative history or pending notice")
			}
			if (failure == "base request" || failure == "replacement overflow") && calls != 0 {
				t.Fatal("impossible base budget sent a summarizer request")
			}
		})
	}
}

func TestOversizedSummaryDoesNotCommitReplacement(t *testing.T) {
	calls := 0
	compressor := &noticeClient{call: func(context.Context, []llm.Message) (string, error) {
		calls++
		return strings.Repeat("界", 40000), nil
	}}
	a := tokenBudgetWorker(t, compressor, compressor)
	a.cfg.Agent.MaxToolResultChars = 65536
	a.forumPending = "Forum notification:\n pending unchanged  "
	a.messages = []llm.Message{{Role: llm.RoleSystem, Content: "original prefix"}, {Role: llm.RoleUser, Content: a.forumPending}}
	before := append([]llm.Message(nil), a.messages...)
	if err := a.compressContext(context.Background(), func(Event) {}, "response too large"); err == nil {
		t.Fatal("oversized summary was accepted")
	}
	if calls != 1 || !reflect.DeepEqual(a.messages, before) || a.forumPending != before[1].Content {
		t.Fatal("post-response budget failure altered history or pending notice")
	}
}
