package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
)

// UsageUpdate is cumulative input plus output usage for one network request.
// Provider totals may correct an earlier estimate downward. Final closes the
// request even on cancellation; observers must not discard updates on ctx.Err().
type UsageUpdate struct {
	RequestID   uint64
	TotalTokens int64
	Estimated   bool
	Final       bool
}

type usageKey struct{}
type usageRequestKey struct{}

var usageSeq uint64

// WithUsageObserver observes all requests derived from ctx. Callbacks on
// different requests may run concurrently and may synchronously cancel ctx.
func WithUsageObserver(ctx context.Context, observer func(UsageUpdate)) context.Context {
	return context.WithValue(ctx, usageKey{}, observer)
}

type requestUsage struct {
	ctx         context.Context
	parts       map[responsesPart]int
	observer    func(UsageUpdate)
	update      UsageUpdate
	input       int64
	outputBytes int64
	provider    *generationUsage
	err         error
}

func beginUsage(ctx context.Context, messages []Message, tools []ToolDefinition) (context.Context, *requestUsage) {
	observer, _ := ctx.Value(usageKey{}).(func(UsageUpdate))
	if observer == nil {
		return ctx, nil
	}
	// UTF-8 bytes / 4, plus approximate message/schema framing. Do not copy
	// a potentially large retained history merely to estimate its token count.
	inputBytes := int64(12)
	for _, msg := range messages {
		inputBytes += int64(len(msg.Role)) + int64(len(msg.Content)) + int64(len(msg.Type)) + int64(len(msg.CallID)) + int64(len(msg.Name)) + int64(len(msg.Arguments)) + int64(len(msg.ID)) + 16
	}
	for _, tool := range tools {
		inputBytes += int64(len(tool.Type)) + int64(len(tool.Name)) + int64(len(tool.Description)) + int64(len(tool.Parameters)) + 32
	}
	u := &requestUsage{ctx: ctx, observer: observer, input: (inputBytes + 3) / 4}
	u.update = UsageUpdate{RequestID: atomic.AddUint64(&usageSeq, 1), TotalTokens: u.input, Estimated: true}
	u.observer(u.update)
	return context.WithValue(ctx, usageRequestKey{}, u), u
}

func usageFor(ctx context.Context) *requestUsage {
	u, _ := ctx.Value(usageRequestKey{}).(*requestUsage)
	return u
}

func (u *requestUsage) chat(msg chatModelMessage) error {
	if u == nil {
		return nil
	}
	bytes := len(msg.Content) + len(firstNonEmpty(msg.ReasoningContent, msg.Reasoning, msg.ReasoningText))
	for _, call := range msg.ToolCalls {
		bytes += len(call.Function.Name) + len(call.Function.Arguments)
	}
	return u.add(bytes)
}

func (u *requestUsage) part(part responsesPart, size int, snapshot bool) {
	if u.parts == nil {
		u.parts = make(map[responsesPart]int)
	}
	previous, exists := u.parts[part]
	// Bound accounting metadata independently of a provider's index values.
	if !exists && len(u.parts) >= responsesMaxBytes/64 {
		u.err = fmt.Errorf("openai usage: too many streamed content parts")
		u.outputBytes += int64(size)
		return
	}
	if snapshot {
		if size <= previous {
			return
		}
		u.parts[part] = size
		u.outputBytes += int64(size - previous)
	} else {
		u.parts[part] = previous + size
		u.outputBytes += int64(size)
	}
}

func (u *requestUsage) responseItem(index int, item responsesOutputItem) {
	if item.Type == "function_call" {
		u.part(responsesPart{Output: index, Kind: "function_name"}, len(item.Name), true)
		u.part(responsesPart{Output: index, Kind: "function_arguments"}, len(item.Arguments), true)
	}
	_ = (responsesResponse{Output: []responsesOutputItem{item}}).eachText(func(part responsesPart, delta Delta) error {
		part.Output = index
		u.part(part, len(delta.Content)+len(delta.Thinking), true)
		return nil
	})
}

func (u *requestUsage) response(snapshot responsesResponse) error {
	if u == nil {
		return nil
	}
	u.supplied(snapshot.Usage)
	for index, item := range snapshot.Output {
		u.responseItem(index, item)
	}
	u.publish(false)
	if u.err != nil {
		return u.err
	}
	return context.Cause(u.ctx)
}

func (u *requestUsage) event(event responsesStreamEvent) error {
	if u == nil {
		return nil
	}
	part := responsesPart{Output: event.OutputIndex, Index: event.ContentIndex}
	snapshot := false
	value := ""
	switch event.Type {
	case "response.output_text.delta", "response.output_text.done":
		part.Kind = "output_text"
	case "response.reasoning_text.delta", "response.reasoning_text.done":
		part.Kind = "reasoning_text"
	case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		part.Kind, part.Index = "summary_text", event.SummaryIndex
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		part.Kind, part.Index = "function_arguments", 0
	case "response.output_item.added", "response.output_item.done":
		u.responseItem(event.OutputIndex, event.Item)
	case "response.completed", "response.failed", "response.incomplete", "response.cancelled":
		return u.response(event.Response)
	}
	if part.Kind != "" {
		switch event.Type {
		case "response.output_text.done", "response.reasoning_text.done", "response.reasoning_summary_text.done":
			value, snapshot = event.Text, true
		case "response.function_call_arguments.done":
			value, snapshot = event.Arguments, true
		default:
			_ = json.Unmarshal(event.Delta, &value)
		}
		u.part(part, len(value), snapshot)
	}
	u.publish(false)
	if u.err != nil {
		return u.err
	}
	return context.Cause(u.ctx)
}

func (u *requestUsage) add(bytes int) error {
	if u == nil {
		return nil
	}
	u.outputBytes += int64(bytes)
	u.publish(false)
	return context.Cause(u.ctx)
}

func (u *requestUsage) supplied(usage *generationUsage) {
	if u != nil && usage != nil {
		u.provider = usage
		u.publish(false)
	}
}

func (u *requestUsage) publish(final bool) {
	total, estimated := u.input+(u.outputBytes+3)/4, true
	if p := u.provider; p != nil {
		input, output := p.InputTokens, p.OutputTokens
		if input == nil {
			input = p.PromptTokens
		}
		if output == nil {
			output = p.CompletionTokens
		}
		if input != nil && *input >= 0 {
			total = *input + (u.outputBytes+3)/4
		}
		if output != nil && *output >= 0 {
			total = u.input + *output
			if input != nil && *input >= 0 {
				total, estimated = *input+*output, false
			}
		}
		if p.TotalTokens != nil && *p.TotalTokens >= 0 {
			total, estimated = *p.TotalTokens, false
		}
	}
	if final || total != u.update.TotalTokens || estimated != u.update.Estimated {
		u.update.TotalTokens, u.update.Estimated, u.update.Final = total, estimated, final
		u.observer(u.update)
	}
}

func (u *requestUsage) finish() {
	if u != nil {
		u.publish(true)
	}
}

// An interrupted or malformed nonstream reply cannot be tokenized as model
// fields. Retain its received bytes as a conservative, explicitly estimated
// fallback instead of silently dropping the partially generated response.
func accountUnreadableBody(ctx context.Context, body []byte, readErr error) {
	if usage := usageFor(ctx); usage != nil && (readErr != nil || !json.Valid(body)) {
		_ = usage.add(len(body))
	}
}
