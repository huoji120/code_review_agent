package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"code-review-agent/internal/llm"
)

func TestCompressionShrinksOldestCompleteToolUnitsUntilAccepted(t *testing.T) {
	calls, previous := 0, 1<<30
	compressor := &noticeClient{call: func(ctx context.Context, m []llm.Message) (string, error) {
		calls++
		body := m[1].Content
		if len(body) >= previous {
			t.Fatal("oversized compressor request was repeated")
		}
		previous = len(body)
		for i := 0; i < 8; i++ {
			call := strings.Contains(body, fmt.Sprintf("arguments=CALL%d", i))
			out := strings.Contains(body, fmt.Sprintf("RESULT%d", i))
			if call != out {
				t.Fatalf("split call/output unit %d", i)
			}
		}
		if strings.Contains(body, "CALL6") {
			return "", errors.New("maximum context length exceeded")
		}
		if !strings.Contains(body, "CALL7") {
			t.Fatal("discarded newest evidence before necessary")
		}
		return "Keep investigating entry.go newest evidence", nil
	}}
	a := tokenBudgetWorker(t, compressor, compressor)
	for i := 0; i < 8; i++ {
		a.messages = append(a.messages, llm.Message{Type: "function_call", Name: "read_file", CallID: fmt.Sprint(i), Arguments: fmt.Sprintf("CALL%d", i)}, llm.Message{Type: "function_call_output", CallID: fmt.Sprint(i), Content: fmt.Sprintf("RESULT%d", i)})
	}
	if err := a.compressContext(context.Background(), func(Event) {}, "server smaller than configured"); err != nil {
		t.Fatal(err)
	}
	if calls < 3 || !strings.Contains(a.messages[1].Content, "newest evidence") {
		t.Fatalf("not recovered: calls=%d", calls)
	}
}

func TestCompressionMinimalRejectionTerminatesWithoutLosingHistory(t *testing.T) {
	calls := 0
	compressor := &noticeClient{call: func(context.Context, []llm.Message) (string, error) {
		calls++
		return "", errors.New("context length exceeded")
	}}
	a := tokenBudgetWorker(t, compressor, compressor)
	a.messages = []llm.Message{{Role: llm.RoleSystem, Content: "original"}, {Role: llm.RoleUser, Content: "task"}}
	before := append([]llm.Message(nil), a.messages...)
	if err := a.compressContext(context.Background(), func(Event) {}, "too large"); err == nil || !strings.Contains(err.Error(), "最小请求") {
		t.Fatal(err)
	}
	if calls != 2 || !reflect.DeepEqual(before, a.messages) {
		t.Fatalf("unbounded retry or history loss: calls=%d", calls)
	}
}
