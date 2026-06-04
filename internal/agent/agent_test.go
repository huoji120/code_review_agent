package agent

import (
	"strings"
	"testing"

	"code-review-agent/internal/llm"
)

func TestEstimateTextTokensCountsChineseConservatively(t *testing.T) {
	text := strings.Repeat("上下文压缩", 100)
	if got := estimateTextTokens(text); got < 400 {
		t.Fatalf("estimateTextTokens() = %d, want at least 400", got)
	}
}

func TestSelectRecentMessagesKeepsNewestWithinBudget(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: strings.Repeat("old ", 100)},
		{Role: llm.RoleAssistant, Content: strings.Repeat("middle ", 100)},
		{Role: llm.RoleUser, Content: strings.Repeat("new ", 20)},
	}

	selected := selectRecentMessages(messages, 30)
	if len(selected) != 1 {
		t.Fatalf("len(selected) = %d, want 1", len(selected))
	}
	if selected[0].Content != messages[2].Content {
		t.Fatalf("selected newest content mismatch")
	}
}
