package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

func validCall(call FunctionCall) error {
	if len(call.Arguments) > responsesMaxBytes || len(call.Name) > responsesMaxBytes-len(call.Arguments) {
		return &ToolProtocolError{Kind: "function arguments too large"}
	}
	if strings.TrimSpace(call.CallID) == "" {
		return &ToolProtocolError{Kind: "missing call_id"}
	}
	if strings.TrimSpace(call.Name) == "" {
		return &ToolProtocolError{Kind: "missing function name"}
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil || args == nil {
		return &ToolProtocolError{Kind: "arguments must be a complete JSON object", Err: err}
	}
	return nil
}

func validateCalls(calls []FunctionCall) error {
	seen := make(map[string]bool, len(calls))
	for _, call := range calls {
		if err := validCall(call); err != nil {
			return err
		}
		if seen[call.CallID] {
			return &ToolProtocolError{Kind: "duplicate call_id"}
		}
		seen[call.CallID] = true
	}
	return nil
}

// A locally corrupted checkpoint is not a model response failure: do not retry it
// as though a new model output could repair the already-persisted call lineage.
func validateToolHistory(messages []Message) error {
	pending := map[string]bool{}
	seen := map[string]bool{}
	for _, msg := range messages {
		switch msg.Type {
		case "function_call":
			if err := validCall(FunctionCall{CallID: msg.CallID, Name: msg.Name, Arguments: msg.Arguments}); err != nil {
				return fmt.Errorf("invalid native history call: %v", err)
			}
			if seen[msg.CallID] {
				return fmt.Errorf("native history duplicates call_id %q", msg.CallID)
			}
			seen[msg.CallID] = true
			pending[msg.CallID] = true
		case "function_call_output":
			if !pending[msg.CallID] {
				return fmt.Errorf("native history has orphan output %q", msg.CallID)
			}
			delete(pending, msg.CallID)
		case "":
			if len(pending) != 0 {
				return fmt.Errorf("native history interrupts an unresolved function call")
			}
		default:
			return fmt.Errorf("unsupported native history item type %q", msg.Type)
		}
	}
	if len(pending) != 0 {
		return fmt.Errorf("native history has unresolved function calls")
	}
	return nil
}

func nativeResponsesInput(messages []Message) []any {
	input := make([]any, 0, len(messages))
	for _, msg := range messages {
		switch msg.Type {
		case "function_call":
			item := map[string]any{"type": msg.Type, "call_id": msg.CallID, "name": msg.Name, "arguments": msg.Arguments}
			if msg.ID != "" {
				item["id"] = msg.ID
			}
			input = append(input, item)
		case "function_call_output":
			input = append(input, map[string]any{"type": msg.Type, "call_id": msg.CallID, "output": msg.Content})
		default:
			role := msg.Role
			if role == RoleTool {
				role = RoleUser
			} // Legacy traces are inert text, not native tool results.
			input = append(input, responsesInputItem{Role: string(role), Content: msg.Content})
		}
	}
	return input
}

func nativeChatInput(messages []Message) []any {
	input := make([]any, 0, len(messages))
	for _, msg := range messages {
		switch msg.Type {
		case "function_call":
			call := map[string]any{"id": msg.CallID, "type": "function", "function": map[string]string{"name": msg.Name, "arguments": msg.Arguments}}
			// A valid retained multi-call assistant message must remain one message.
			if len(input) > 0 {
				if prior, ok := input[len(input)-1].(map[string]any); ok && prior["role"] == "assistant" && prior["tool_calls"] != nil {
					prior["tool_calls"] = append(prior["tool_calls"].([]any), call)
					continue
				}
			}
			input = append(input, map[string]any{"role": "assistant", "tool_calls": []any{call}})
		case "function_call_output":
			input = append(input, map[string]any{"role": "tool", "tool_call_id": msg.CallID, "content": msg.Content})
		default:
			role := msg.Role
			if role == RoleTool {
				role = RoleUser
			}
			input = append(input, map[string]any{"role": string(role), "content": msg.Content})
		}
	}
	return input
}

func (c *OpenAIClient) nativeRequest(ctx context.Context, path string, payload any) (*http.Response, context.Context, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, ctx, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(path), bytes.NewReader(data))
	if err != nil {
		return nil, ctx, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	var resp *http.Response
	if c.cfg.Stream {
		req.Header.Set("Accept", "text/event-stream")
		resp, ctx, err = c.doStream(req)
	} else {
		resp, err = c.httpClient.Do(req)
	}
	if err != nil {
		return nil, ctx, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		body, err := readResponsesBody(resp.Body)
		if err != nil {
			return nil, ctx, err
		}
		return nil, ctx, fmt.Errorf("openai status %d: %s", resp.StatusCode, body)
	}
	return resp, ctx, nil
}

type toolText struct {
	content, thinking strings.Builder
	emit              func(Delta) error
	limit             int
}

func (s *toolText) add(delta Delta) error {
	limit := s.limit
	if limit < responsesMaxBytes {
		limit = responsesMaxBytes
	}
	remaining := limit - s.content.Len() - s.thinking.Len()
	if len(delta.Content) > remaining || len(delta.Thinking) > remaining-len(delta.Content) {
		return fmt.Errorf("openai native text exceeds %d bytes", limit)
	}
	s.content.WriteString(delta.Content)
	s.thinking.WriteString(delta.Thinking)
	if s.emit != nil && (delta.Content != "" || delta.Thinking != "") {
		return s.emit(delta)
	}
	return nil
}
func (s *toolText) result(calls []FunctionCall) ToolResponse {
	return ToolResponse{Content: s.content.String(), Thinking: s.thinking.String(), Calls: calls}
}

// Keep individual SSE frames and tool arguments bounded separately. Text may
// span many frames; allow generous UTF-8 headroom for the configured generation.
func nativeTextLimit(tokens int) int {
	maxInt := int(^uint(0) >> 1)
	if tokens > maxInt/16 {
		return maxInt
	}
	if tokens <= responsesMaxBytes/16 {
		return responsesMaxBytes
	}
	return tokens * 16
}

func responseWireLimit(textLimit int) int {
	// JSON may escape every byte as six characters. Keep wire overhead bounded
	// independently and retain the decoded text/argument checks after parsing.
	if textLimit < responsesMaxBytes {
		textLimit = responsesMaxBytes
	}
	maxInt := int(^uint(0) >> 1)
	limit := maxInt - 1
	if textLimit <= (maxInt-1-responsesMaxBytes)/6 {
		limit = textLimit*6 + responsesMaxBytes
	}
	return limit
}

func readNativeBody(body io.Reader, textLimit int) ([]byte, error) {
	limit := responseWireLimit(textLimit)
	data, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil {
		return data, err
	}
	if len(data) > limit {
		return data, fmt.Errorf("openai native response body exceeds %d bytes", limit)
	}
	return data, nil
}

func (c *OpenAIClient) responsesTools(ctx context.Context, messages []Message, tools []ToolDefinition, emit func(Delta) error) (ToolResponse, error) {
	payload := struct {
		Model             string           `json:"model"`
		Input             []any            `json:"input"`
		Tools             []ToolDefinition `json:"tools"`
		ParallelToolCalls bool             `json:"parallel_tool_calls"`
		Temperature       float64          `json:"temperature"`
		TopP              float64          `json:"top_p"`
		MaxOutputTokens   int              `json:"max_output_tokens,omitempty"`
		Stream            bool             `json:"stream"`
	}{c.cfg.Model, nativeResponsesInput(messages), tools, false, c.cfg.Temperature, c.cfg.TopP, c.cfg.MaxOutputTokens, c.cfg.Stream}
	progress, err := newGenerationTracker(ctx, emit)
	if err != nil {
		return ToolResponse{}, err
	}
	defer progress.flushPending()
	progress.textLimit = nativeTextLimit(c.cfg.MaxOutputTokens)
	resp, ctx, err := c.nativeRequest(ctx, "/responses", payload)
	if err != nil {
		return ToolResponse{}, err
	}
	defer resp.Body.Close()
	if c.cfg.Stream {
		return consumeNativeResponses(ctx, resp.Body, progress)
	}
	body, err := readNativeBody(resp.Body, progress.textLimit)
	accountUnreadableBody(ctx, body, err)
	if err != nil {
		return ToolResponse{}, err
	}
	var parsed responsesResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ToolResponse{}, &ToolProtocolError{Kind: "invalid Responses response", Err: err}
	}
	if err := usageFor(ctx).response(parsed); err != nil {
		return ToolResponse{}, err
	}
	if err := parsed.completed(); err != nil {
		return ToolResponse{}, err
	}
	text := toolText{emit: progress.text, limit: progress.textLimit}
	if err := parsed.eachText(func(_ responsesPart, d Delta) error { return text.add(d) }); err != nil {
		return ToolResponse{}, err
	}
	calls := make([]FunctionCall, 0)
	for _, item := range parsed.Output {
		if item.Type == "function_call" {
			if item.Status != "" && item.Status != "completed" {
				return ToolResponse{}, &ToolProtocolError{Kind: "function call did not complete"}
			}
			calls = append(calls, FunctionCall{ID: item.ID, CallID: item.CallID, Name: item.Name, Arguments: item.Arguments})
			if err := progress.add(0, 0, len(item.Arguments)); err != nil {
				return ToolResponse{}, err
			}
		}
	}
	if err := validateCalls(calls); err != nil {
		return ToolResponse{}, err
	}
	if err := progress.finish(parsed.Usage); err != nil {
		return ToolResponse{}, err
	}
	if ctx.Err() != nil {
		return ToolResponse{}, context.Cause(ctx)
	}
	return text.result(calls), nil
}

type responseCallState struct {
	call     FunctionCall
	args     strings.Builder
	snapshot bool
}

func (s *responseCallState) merge(item responsesOutputItem, final bool) error {
	for _, pair := range [][2]string{{s.call.ID, item.ID}, {s.call.CallID, item.CallID}, {s.call.Name, item.Name}} {
		if pair[0] != "" && pair[1] != "" && pair[0] != pair[1] {
			return &ToolProtocolError{Kind: "conflicting function identity in stream"}
		}
	}
	if item.ID != "" {
		s.call.ID = item.ID
	}
	if item.CallID != "" {
		s.call.CallID = item.CallID
	}
	if item.Name != "" {
		s.call.Name = item.Name
	}
	if final || item.Arguments != "" {
		previous := s.args.String()
		if !strings.HasPrefix(item.Arguments, previous) || (s.snapshot && previous != item.Arguments) {
			return &ToolProtocolError{Kind: "conflicting function arguments in stream"}
		}
		s.args.WriteString(item.Arguments[len(previous):])
		s.snapshot = final
	}
	if final && item.Status != "" && item.Status != "completed" {
		return &ToolProtocolError{Kind: "incomplete function item"}
	}
	return nil
}

func readNativeResponses(ctx context.Context, body io.Reader, emit func(Delta) error) (ToolResponse, error) {
	progress, err := newGenerationTracker(ctx, emit)
	if err != nil {
		return ToolResponse{}, err
	}
	defer progress.flushPending()
	return consumeNativeResponses(ctx, body, progress)
}

func consumeNativeResponses(ctx context.Context, body io.Reader, progress *generationTracker) (ToolResponse, error) {
	progress.ctx = ctx
	text := toolText{emit: progress.text, limit: progress.textLimit}
	seen := map[responsesPart]*strings.Builder{}
	addPart := func(part responsesPart, d Delta, snapshot bool) error {
		value := d.Content
		if part.Kind != "output_text" {
			value = d.Thinking
		}
		previous := seen[part]
		if previous == nil {
			if len(seen) >= 1024 {
				return fmt.Errorf("too many native text parts")
			}
			previous = &strings.Builder{}
			seen[part] = previous
		}
		if snapshot {
			if !strings.HasPrefix(value, previous.String()) {
				return &ToolProtocolError{Kind: "conflicting text snapshot"}
			}
			value = value[previous.Len():]
		}
		previous.WriteString(value)
		if part.Kind == "output_text" {
			return text.add(Delta{Content: value})
		}
		return text.add(Delta{Thinking: value})
	}
	states := map[int]*responseCallState{}
	stateFor := func(index int) (*responseCallState, error) {
		if index < 0 || index > 1024 {
			return nil, &ToolProtocolError{Kind: "invalid output_index"}
		}
		state := states[index]
		if state == nil {
			state = &responseCallState{}
			states[index] = state
		}
		return state, nil
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), responseWireLimit(progress.textLimit))
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ToolResponse{}, context.Cause(ctx)
		}
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			return ToolResponse{}, fmt.Errorf("openai native Responses missing response.completed: %w", io.ErrUnexpectedEOF)
		}
		var event responsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			_ = usageFor(ctx).add(len(payload))
			return ToolResponse{}, &ToolProtocolError{Kind: "invalid Responses event", Err: err}
		}
		if err := usageFor(ctx).event(event); err != nil {
			return ToolResponse{}, err
		}
		switch event.Type {
		case "response.output_text.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.output_text.done", "response.reasoning_text.done", "response.reasoning_summary_text.done":
			var value string
			snapshot := strings.HasSuffix(event.Type, ".done")
			if snapshot {
				value = event.Text
			} else if err := json.Unmarshal(event.Delta, &value); err != nil {
				return ToolResponse{}, &ToolProtocolError{Kind: "invalid text delta", Err: err}
			}
			if value == "" {
				continue
			}
			part := responsesPart{Output: event.OutputIndex, Index: event.ContentIndex}
			d := Delta{}
			switch event.Type {
			case "response.output_text.delta", "response.output_text.done":
				part.Kind = "output_text"
				d.Content = value
			case "response.reasoning_text.delta", "response.reasoning_text.done":
				part.Kind = "reasoning_text"
				d.Thinking = value
			default:
				part.Kind = "summary_text"
				part.Index = event.SummaryIndex
				d.Thinking = value
			}
			if err := addPart(part, d, snapshot); err != nil {
				return ToolResponse{}, err
			}
		case "response.output_item.added", "response.output_item.done":
			if event.Item.Type != "function_call" {
				if err := (responsesResponse{Output: []responsesOutputItem{event.Item}}).eachText(func(part responsesPart, d Delta) error {
					part.Output = event.OutputIndex
					return addPart(part, d, true)
				}); err != nil {
					return ToolResponse{}, err
				}
				continue
			}
			state, err := stateFor(event.OutputIndex)
			if err != nil {
				return ToolResponse{}, err
			}
			previousBytes := state.args.Len()
			if err := state.merge(event.Item, event.Type == "response.output_item.done"); err != nil {
				return ToolResponse{}, err
			}
			if err := progress.add(0, 0, state.args.Len()-previousBytes); err != nil {
				return ToolResponse{}, err
			}
		case "response.function_call_arguments.delta", "response.function_call_arguments.done":
			state, err := stateFor(event.OutputIndex)
			if err != nil {
				return ToolResponse{}, err
			}
			if event.ItemID != "" {
				if state.call.ID != "" && state.call.ID != event.ItemID {
					return ToolResponse{}, &ToolProtocolError{Kind: "argument delta item_id mismatch"}
				}
				state.call.ID = event.ItemID
			}
			previousBytes := state.args.Len()
			if event.Type == "response.function_call_arguments.done" {
				if err := state.merge(responsesOutputItem{Arguments: event.Arguments}, true); err != nil {
					return ToolResponse{}, err
				}
			} else {
				if state.snapshot {
					return ToolResponse{}, &ToolProtocolError{Kind: "arguments delta after final snapshot"}
				}
				var value string
				if err := json.Unmarshal(event.Delta, &value); err != nil {
					return ToolResponse{}, &ToolProtocolError{Kind: "invalid arguments delta", Err: err}
				}
				if state.args.Len()+len(value) > responsesMaxBytes {
					return ToolResponse{}, &ToolProtocolError{Kind: "function arguments too large"}
				}
				state.args.WriteString(value)
			}
			if err := progress.add(0, 0, state.args.Len()-previousBytes); err != nil {
				return ToolResponse{}, err
			}
		case "response.completed":
			if err := event.Response.completed(); err != nil {
				return ToolResponse{}, err
			}
			for index, item := range event.Response.Output {
				if item.Type == "function_call" {
					state, err := stateFor(index)
					if err != nil {
						return ToolResponse{}, err
					}
					previousBytes := state.args.Len()
					if err := state.merge(item, true); err != nil {
						return ToolResponse{}, err
					}
					if err := progress.add(0, 0, state.args.Len()-previousBytes); err != nil {
						return ToolResponse{}, err
					}
				}
			}
			calls := make([]FunctionCall, 0, len(states))
			indices := make([]int, 0, len(states))
			for index := range states {
				indices = append(indices, index)
			}
			sort.Ints(indices)
			for _, index := range indices {
				state := states[index]
				state.call.Arguments = state.args.String()
				calls = append(calls, state.call)
			}
			if err := validateCalls(calls); err != nil {
				return ToolResponse{}, err
			}
			if err := event.Response.eachText(func(part responsesPart, d Delta) error {
				return addPart(part, d, true)
			}); err != nil {
				return ToolResponse{}, err
			}
			if err := progress.finish(event.Response.Usage); err != nil {
				return ToolResponse{}, err
			}
			if ctx.Err() != nil {
				return ToolResponse{}, context.Cause(ctx)
			}
			return text.result(calls), nil
		case "response.incomplete":
			return ToolResponse{}, incompleteResponse(event.Response)
		case "response.failed", "response.cancelled":
			return ToolResponse{}, fmt.Errorf("openai native %s: %s", event.Type, event.Response.failureDetail())
		case "error", "response.error":
			return ToolResponse{}, fmt.Errorf("openai native error: %s", firstNonEmpty(event.Message, event.Error.Message, event.Code, event.Error.Code, "unspecified API error"))
		}
	}
	if ctx.Err() != nil {
		return ToolResponse{}, context.Cause(ctx)
	}
	if err := scanner.Err(); err != nil {
		return ToolResponse{}, err
	}
	return ToolResponse{}, fmt.Errorf("openai native Responses missing response.completed: %w", io.ErrUnexpectedEOF)
}

func (c *OpenAIClient) chatCompletionsTools(ctx context.Context, messages []Message, tools []ToolDefinition, emit func(Delta) error) (ToolResponse, error) {
	nativeTools := make([]any, 0, len(tools))
	for _, tool := range tools {
		nativeTools = append(nativeTools, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.Parameters, "strict": tool.Strict}})
	}
	payload := struct {
		Model             string             `json:"model"`
		Messages          []any              `json:"messages"`
		Tools             []any              `json:"tools"`
		ParallelToolCalls bool               `json:"parallel_tool_calls"`
		Temperature       float64            `json:"temperature"`
		TopP              float64            `json:"top_p"`
		MaxTokens         int                `json:"max_tokens,omitempty"`
		Stream            bool               `json:"stream"`
		StreamOptions     *chatStreamOptions `json:"stream_options,omitempty"`
	}{c.cfg.Model, nativeChatInput(messages), nativeTools, false, c.cfg.Temperature, c.cfg.TopP, c.cfg.MaxOutputTokens, c.cfg.Stream, nil}
	if c.cfg.Stream && usageFor(ctx) != nil {
		payload.StreamOptions = &chatStreamOptions{IncludeUsage: true}
	}
	progress, err := newGenerationTracker(ctx, emit)
	if err != nil {
		return ToolResponse{}, err
	}
	defer progress.flushPending()
	progress.textLimit = nativeTextLimit(c.cfg.MaxOutputTokens)
	resp, ctx, err := c.nativeRequest(ctx, "/chat/completions", payload)
	if err != nil {
		return ToolResponse{}, err
	}
	defer resp.Body.Close()
	if c.cfg.Stream {
		return consumeNativeChat(ctx, resp.Body, progress)
	}
	body, err := readNativeBody(resp.Body, progress.textLimit)
	accountUnreadableBody(ctx, body, err)
	if err != nil {
		return ToolResponse{}, err
	}
	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ToolResponse{}, &ToolProtocolError{Kind: "invalid chat response", Err: err}
	}
	if usage := usageFor(ctx); usage != nil {
		usage.supplied(parsed.Usage)
		for _, choice := range parsed.Choices {
			if err := usage.chat(choice.Message); err != nil {
				return ToolResponse{}, err
			}
		}
	}
	if parsed.Error.Message != "" || parsed.Error.Code != "" {
		return ToolResponse{}, fmt.Errorf("openai native chat error: %s", firstNonEmpty(parsed.Error.Message, parsed.Error.Code))
	}
	if len(parsed.Choices) != 1 {
		return ToolResponse{}, &ToolProtocolError{Kind: "expected exactly one chat choice"}
	}
	choice := parsed.Choices[0]
	if choice.FinishReason != "stop" && choice.FinishReason != "tool_calls" {
		return ToolResponse{}, unfinishedChat(choice.FinishReason)
	}
	text := toolText{emit: progress.text, limit: progress.textLimit}
	if err := text.add(Delta{Content: choice.Message.Content, Thinking: firstNonEmpty(choice.Message.ReasoningContent, choice.Message.Reasoning, choice.Message.ReasoningText)}); err != nil {
		return ToolResponse{}, err
	}
	calls := make([]FunctionCall, 0, len(choice.Message.ToolCalls))
	for _, call := range choice.Message.ToolCalls {
		if call.Type != "function" {
			return ToolResponse{}, &ToolProtocolError{Kind: "unsupported chat tool type"}
		}
		calls = append(calls, FunctionCall{CallID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
		if err := progress.add(0, 0, len(call.Function.Arguments)); err != nil {
			return ToolResponse{}, err
		}
	}
	if err := validateCalls(calls); err != nil {
		return ToolResponse{}, err
	}
	if err := progress.finish(parsed.Usage); err != nil {
		return ToolResponse{}, err
	}
	if ctx.Err() != nil {
		return ToolResponse{}, context.Cause(ctx)
	}
	return text.result(calls), nil
}

func readNativeChat(ctx context.Context, body io.Reader, emit func(Delta) error) (ToolResponse, error) {
	progress, err := newGenerationTracker(ctx, emit)
	if err != nil {
		return ToolResponse{}, err
	}
	defer progress.flushPending()
	return consumeNativeChat(ctx, body, progress)
}

func consumeNativeChat(ctx context.Context, body io.Reader, progress *generationTracker) (ToolResponse, error) {
	progress.ctx = ctx
	text := toolText{emit: progress.text, limit: progress.textLimit}
	type chatCallState struct {
		id         string
		name, args strings.Builder
	}
	states := map[int]*chatCallState{}
	finish := ""
	var usage *generationUsage
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), responseWireLimit(progress.textLimit))
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ToolResponse{}, context.Cause(ctx)
		}
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			if finish != "stop" && finish != "tool_calls" {
				return ToolResponse{}, unfinishedChat(finish)
			}
			indices := make([]int, 0, len(states))
			for index := range states {
				indices = append(indices, index)
			}
			sort.Ints(indices)
			calls := make([]FunctionCall, 0, len(states))
			for _, index := range indices {
				state := states[index]
				calls = append(calls, FunctionCall{CallID: state.id, Name: state.name.String(), Arguments: state.args.String()})
			}
			if err := validateCalls(calls); err != nil {
				return ToolResponse{}, err
			}
			if err := progress.finish(usage); err != nil {
				return ToolResponse{}, err
			}
			if ctx.Err() != nil {
				return ToolResponse{}, context.Cause(ctx)
			}
			return text.result(calls), nil
		}
		var chunk streamResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			_ = usageFor(ctx).add(len(payload))
			return ToolResponse{}, &ToolProtocolError{Kind: "invalid chat event", Err: err}
		}
		if usage := usageFor(ctx); usage != nil {
			usage.supplied(chunk.Usage)
			for _, choice := range chunk.Choices {
				if err := usage.chat(choice.Delta); err != nil {
					return ToolResponse{}, err
				}
			}
		}
		if chunk.Error.Message != "" || chunk.Error.Code != "" {
			return ToolResponse{}, fmt.Errorf("openai native chat error: %s", firstNonEmpty(chunk.Error.Message, chunk.Error.Code))
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				return ToolResponse{}, &ToolProtocolError{Kind: "unexpected chat choice index"}
			}
			delta := choice.Delta
			if finish != "" && (delta.Content != "" || delta.ReasoningContent != "" || delta.Reasoning != "" || delta.ReasoningText != "" || len(delta.ToolCalls) > 0) {
				return ToolResponse{}, &ToolProtocolError{Kind: "chat delta after finish"}
			}
			if err := text.add(Delta{Content: delta.Content, Thinking: firstNonEmpty(delta.ReasoningContent, delta.Reasoning, delta.ReasoningText)}); err != nil {
				return ToolResponse{}, err
			}
			for _, call := range delta.ToolCalls {
				if call.Index < 0 || call.Index > 1024 {
					return ToolResponse{}, &ToolProtocolError{Kind: "invalid tool call index"}
				}
				if call.Type != "" && call.Type != "function" {
					return ToolResponse{}, &ToolProtocolError{Kind: "unsupported chat tool type"}
				}
				state := states[call.Index]
				if state == nil {
					state = &chatCallState{}
					states[call.Index] = state
				}
				if call.ID != "" {
					if state.id != "" && state.id != call.ID {
						return ToolResponse{}, &ToolProtocolError{Kind: "conflicting chat call ID"}
					}
					state.id = call.ID
				}
				if state.args.Len()+state.name.Len()+len(call.Function.Arguments)+len(call.Function.Name) > responsesMaxBytes {
					return ToolResponse{}, &ToolProtocolError{Kind: "chat function arguments too large"}
				}
				state.name.WriteString(call.Function.Name)
				state.args.WriteString(call.Function.Arguments)
				if err := progress.add(0, 0, len(call.Function.Arguments)); err != nil {
					return ToolResponse{}, err
				}
			}
			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}
		}
	}
	if ctx.Err() != nil {
		return ToolResponse{}, context.Cause(ctx)
	}
	if err := scanner.Err(); err != nil {
		return ToolResponse{}, err
	}
	return ToolResponse{}, fmt.Errorf("openai native chat missing [DONE]: %w", io.ErrUnexpectedEOF)
}
