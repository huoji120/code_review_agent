package llm

import (
	"bufio"
	"bytes"
	"code-review-agent/internal/config"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type Message struct {
	Role      Role   `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
	Type      string `json:"type,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	ID        string `json:"id,omitempty"`
}

type ToolDefinition struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type FunctionCall struct {
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolResponse struct {
	Content  string
	Thinking string
	Calls    []FunctionCall
}

type ToolClient interface {
	ChatTools(context.Context, []Message, []ToolDefinition, func(Delta) error) (ToolResponse, error)
}

var ErrToolProtocol = errors.New("openai tool protocol error")

type ToolProtocolError struct {
	Kind string
	Err  error
}

func (e *ToolProtocolError) Error() string {
	if e.Err == nil {
		return "openai tool protocol: " + e.Kind
	}
	return "openai tool protocol " + e.Kind + ": " + e.Err.Error()
}
func (e *ToolProtocolError) Unwrap() error        { return e.Err }
func (e *ToolProtocolError) Is(target error) bool { return target == ErrToolProtocol }

type Client interface {
	Chat(ctx context.Context, messages []Message) (string, error)
	ChatStream(ctx context.Context, messages []Message, emit func(Delta) error) error
}

// GenerationProgress describes output from the current native request, not context
// input. ToolTokens is available only for estimates; providers do not report it.
type GenerationProgress struct {
	OutputTokens    int64  `json:"output_tokens"`
	ReasoningTokens int64  `json:"reasoning_tokens"`
	ToolTokens      int64  `json:"tool_tokens"`
	Estimated       bool   `json:"estimated"`
	ReceivedAt      string `json:"received_at,omitempty"`
}

type Delta struct {
	Content  string
	Thinking string
	Progress *GenerationProgress
}

type OpenAIClient struct {
	cfg        config.OpenAIConfig
	httpClient *http.Client
}

func (c *OpenAIClient) ChatStream(ctx context.Context, messages []Message, emit func(Delta) error) (resultErr error) {
	if c.cfg.APIKey == "" {
		return fmt.Errorf("missing API key; set openai.api_key directly, or set openai.api_key_env to an environment variable name")
	}
	ctx, usage := beginUsage(ctx, messages, nil)
	defer func() {
		usage.finish()
		if usage != nil && resultErr == nil {
			resultErr = context.Cause(usage.ctx)
		}
	}()
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if c.useResponsesAPI() {
		return c.responsesStream(ctx, messages, emit)
	}
	reqBody := chatRequest{
		Model:       c.cfg.Model,
		Messages:    messages,
		Temperature: c.cfg.Temperature,
		TopP:        c.cfg.TopP,
		MaxTokens:   c.cfg.MaxOutputTokens,
		Stream:      true,
	}
	if usage != nil {
		reqBody.StreamOptions = &chatStreamOptions{IncludeUsage: true}
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	url := c.endpoint("/chat/completions")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, ctx, err := c.doStream(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, readErr := readResponsesBody(resp.Body)
		if readErr != nil {
			return readErr
		}
		return fmt.Errorf("openai status %d: %s", resp.StatusCode, string(body))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), responseWireLimit(nativeTextLimit(c.cfg.MaxOutputTokens)))
	for scanner.Scan() {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			return context.Cause(ctx)
		}
		var chunk streamResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			_ = usage.add(len(payload))
			return err
		}
		usage.supplied(chunk.Usage)
		for _, choice := range chunk.Choices {
			delta := Delta{Content: choice.Delta.Content, Thinking: firstNonEmpty(choice.Delta.ReasoningContent, choice.Delta.Reasoning, choice.Delta.ReasoningText)}
			if err := usage.chat(choice.Delta); err != nil {
				return err
			}
			if delta.Content == "" && delta.Thinking == "" {
				continue
			}
			if err := emit(delta); err != nil {
				return err
			}
		}
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("openai chat completions: stream ended without [DONE]: %w", io.ErrUnexpectedEOF)
}

func NewOpenAIClient(cfg config.OpenAIConfig) *OpenAIClient {
	return &OpenAIClient{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second},
	}
}

// Streaming requests share the transport, but not the client's whole-request
// deadline. The watchdog bounds header wait, then each period without body bytes.
func (c *OpenAIClient) doStream(req *http.Request) (*http.Response, context.Context, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	idle := &streamIdle{ctx: ctx, cancel: cancel, timeout: c.httpClient.Timeout}
	if idle.timeout > 0 {
		idle.deadline = time.Now().Add(idle.timeout)
		idle.timer = time.AfterFunc(idle.timeout, idle.expire)
	}
	client := *c.httpClient
	client.Timeout = 0
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			err = cause
		}
		idle.close()
		return nil, ctx, err
	}
	idle.activity()
	resp.Body = &streamBody{ReadCloser: resp.Body, idle: idle}
	return resp, ctx, nil
}

type streamTimeoutError struct {
	timeout time.Duration
}

func (e *streamTimeoutError) Error() string {
	return fmt.Sprintf("openai stream: no network progress for %s", e.timeout)
}

func (e *streamTimeoutError) Timeout() bool { return true }

func (e *streamTimeoutError) Temporary() bool { return true }

func (e *streamTimeoutError) Unwrap() error { return context.DeadlineExceeded }

type streamIdle struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelCauseFunc
	timeout  time.Duration
	deadline time.Time
	timer    *time.Timer
	closed   bool
}

func (s *streamIdle) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return
	}
	// Reset can race a callback already scheduled by the old deadline. Only
	// expire after the latest observed activity, never the stale timer firing.
	if remaining := time.Until(s.deadline); remaining > 0 {
		s.timer.Reset(remaining)
		return
	}
	s.cancel(&streamTimeoutError{timeout: s.timeout})
}

func (s *streamIdle) activity() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed && s.timer != nil && s.ctx.Err() == nil {
		s.deadline = time.Now().Add(s.timeout)
		s.timer.Reset(s.timeout)
	}
}

func (s *streamIdle) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
	}
	s.cancel(nil)
}

type streamBody struct {
	io.ReadCloser
	idle *streamIdle
}

func (b *streamBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if cause := context.Cause(b.idle.ctx); cause != nil {
		return 0, cause
	}
	if n > 0 {
		b.idle.activity()
	}
	return n, err
}

func (b *streamBody) Close() error {
	b.idle.close()
	return b.ReadCloser.Close()
}

func (c *OpenAIClient) Chat(ctx context.Context, messages []Message) (result string, resultErr error) {
	if c.cfg.Stream {
		var thinking strings.Builder
		var content strings.Builder
		err := c.ChatStream(ctx, messages, func(delta Delta) error {
			thinking.WriteString(delta.Thinking)
			content.WriteString(delta.Content)
			return nil
		})
		if err != nil {
			return "", err
		}
		return joinAssistantParts(thinking.String(), content.String()), err
	}
	if c.cfg.APIKey == "" {
		return "", fmt.Errorf("missing API key; set openai.api_key directly, or set openai.api_key_env to an environment variable name")
	}
	ctx, usage := beginUsage(ctx, messages, nil)
	defer func() {
		usage.finish()
		if usage != nil && resultErr == nil && usage.ctx.Err() != nil {
			result, resultErr = "", context.Cause(usage.ctx)
		}
	}()
	if ctx.Err() != nil {
		return "", context.Cause(ctx)
	}
	if c.useResponsesAPI() {
		return c.responsesChat(ctx, messages)
	}
	reqBody := chatRequest{
		Model:       c.cfg.Model,
		Messages:    messages,
		Temperature: c.cfg.Temperature,
		TopP:        c.cfg.TopP,
		MaxTokens:   c.cfg.MaxOutputTokens,
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	url := c.endpoint("/chat/completions")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	accountUnreadableBody(ctx, body, err)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("openai status %d: %s", resp.StatusCode, string(body))
	}
	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	usage.supplied(parsed.Usage)
	if usage != nil {
		for _, choice := range parsed.Choices {
			msg := choice.Message
			if err := usage.chat(msg); err != nil {
				return "", err
			}
		}
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("openai returned no choices")
	}
	msg := parsed.Choices[0].Message
	return joinAssistantParts(firstNonEmpty(msg.ReasoningContent, msg.Reasoning, msg.ReasoningText), msg.Content), nil
}

// ChatTools uses the provider's native function-call protocol. Tool calls are
// collected but never inferred from text/reasoning channels.
func (c *OpenAIClient) ChatTools(ctx context.Context, messages []Message, tools []ToolDefinition, emit func(Delta) error) (result ToolResponse, resultErr error) {
	if c.cfg.APIKey == "" {
		return ToolResponse{}, fmt.Errorf("missing API key")
	}
	if err := validateToolHistory(messages); err != nil {
		return ToolResponse{}, err
	}
	ctx, usage := beginUsage(ctx, messages, tools)
	defer func() {
		usage.finish()
		if usage != nil && resultErr == nil && usage.ctx.Err() != nil {
			result, resultErr = ToolResponse{}, context.Cause(usage.ctx)
		}
	}()
	if ctx.Err() != nil {
		return ToolResponse{}, context.Cause(ctx)
	}
	if c.useResponsesAPI() {
		return c.responsesTools(ctx, messages, tools, emit)
	}
	return c.chatCompletionsTools(ctx, messages, tools, emit)
}

func (c *OpenAIClient) useResponsesAPI() bool {
	api := strings.TrimSpace(c.cfg.APIInterface)
	return api == "" || strings.EqualFold(api, "responses")
}

func (c *OpenAIClient) endpoint(path string) string {
	return strings.TrimRight(c.cfg.BaseURL, "/") + path
}

func (c *OpenAIClient) responsesStream(ctx context.Context, messages []Message, emit func(Delta) error) error {
	reqBody := responsesRequest{Model: c.cfg.Model, Input: responsesInput(messages), Temperature: c.cfg.Temperature, TopP: c.cfg.TopP, MaxOutputTokens: c.cfg.MaxOutputTokens, Stream: true}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/responses"), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	resp, ctx, err := c.doStream(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, readErr := readResponsesBody(resp.Body)
		if readErr != nil {
			return readErr
		}
		return fmt.Errorf("openai status %d: %s", resp.StatusCode, string(body))
	}
	// Track only emitted part identities, never a second copy of generated text.
	seen := make(map[responsesPart]bool)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), responseWireLimit(nativeTextLimit(c.cfg.MaxOutputTokens)))
	for scanner.Scan() {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			return fmt.Errorf("openai responses: stream ended without response.completed: %w", io.ErrUnexpectedEOF)
		}
		var chunk responsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			_ = usageFor(ctx).add(len(payload))
			return fmt.Errorf("openai responses event: %w", err)
		}
		if err := usageFor(ctx).event(chunk); err != nil {
			return err
		}
		var delta Delta
		part := responsesPart{Output: chunk.OutputIndex, Index: chunk.ContentIndex}
		switch chunk.Type {
		case "response.output_text.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			var text string
			if err := json.Unmarshal(chunk.Delta, &text); err != nil {
				return fmt.Errorf("openai responses delta: %w", err)
			}
			if text == "" {
				continue
			}
			switch chunk.Type {
			case "response.output_text.delta":
				part.Kind = "output_text"
				delta.Content = text
			case "response.reasoning_text.delta":
				part.Kind = "reasoning_text"
				delta.Thinking = text
			case "response.reasoning_summary_text.delta":
				part.Kind = "summary_text"
				part.Index = chunk.SummaryIndex
				delta.Thinking = text
			}
			if !seen[part] {
				if len(seen) >= responsesMaxBytes/64 {
					return fmt.Errorf("openai responses: too many streamed content parts")
				}
				seen[part] = true
			}
		case "response.completed":
			if err := chunk.Response.completed(); err != nil {
				return err
			}
			if err := chunk.Response.eachText(func(part responsesPart, delta Delta) error {
				if ctx.Err() != nil {
					return context.Cause(ctx)
				}
				if seen[part] {
					return nil
				}
				return emit(delta)
			}); err != nil {
				return err
			}
			return context.Cause(ctx)
		case "response.failed", "response.incomplete", "response.cancelled":
			return fmt.Errorf("openai responses %s: %s", chunk.Type, chunk.Response.failureDetail())
		case "error", "response.error":
			return fmt.Errorf("openai responses error: %s", firstNonEmpty(chunk.Message, chunk.Error.Message, chunk.Code, chunk.Error.Code, "unspecified API error"))
		default:
			// Done snapshots and unrelated tool/audio deltas are not answer text.
			continue
		}
		if err := emit(delta); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("openai responses: stream ended without response.completed: %w", io.ErrUnexpectedEOF)
}

func (c *OpenAIClient) responsesChat(ctx context.Context, messages []Message) (string, error) {
	reqBody := responsesRequest{Model: c.cfg.Model, Input: responsesInput(messages), Temperature: c.cfg.Temperature, TopP: c.cfg.TopP, MaxOutputTokens: c.cfg.MaxOutputTokens}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/responses"), bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := readResponsesBody(resp.Body)
	accountUnreadableBody(ctx, body, err)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("openai status %d: %s", resp.StatusCode, string(body))
	}
	var parsed responsesResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	if err := usageFor(ctx).response(parsed); err != nil {
		return "", err
	}
	if err := parsed.completed(); err != nil {
		return "", err
	}
	var thinking, content strings.Builder
	if err := parsed.eachText(func(_ responsesPart, delta Delta) error {
		thinking.WriteString(delta.Thinking)
		content.WriteString(delta.Content)
		return nil
	}); err != nil {
		return "", err
	}
	return joinAssistantParts(thinking.String(), content.String()), nil
}

func responsesInput(messages []Message) []responsesInputItem {
	items := make([]responsesInputItem, 0, len(messages))
	for _, msg := range messages {
		role := string(msg.Role)
		if msg.Role == RoleTool {
			// Text-protocol tool results are user input, not native function outputs.
			role = string(RoleUser)
		}
		items = append(items, responsesInputItem{Role: role, Content: msg.Content})
	}
	return items
}

func joinAssistantParts(thinking, content string) string {
	if thinking == "" {
		return content
	}
	var b strings.Builder
	b.WriteString("<think>")
	b.WriteString(thinking)
	b.WriteString("</think>")
	b.WriteString(content)
	return b.String()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type chatRequest struct {
	Model         string             `json:"model"`
	Messages      []Message          `json:"messages"`
	Temperature   float64            `json:"temperature"`
	TopP          float64            `json:"top_p"`
	MaxTokens     int                `json:"max_tokens,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	StreamOptions *chatStreamOptions `json:"stream_options,omitempty"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatModelMessage `json:"message"`
		FinishReason string           `json:"finish_reason"`
	} `json:"choices"`
	Error responsesAPIError `json:"error"`
	Usage *generationUsage  `json:"usage"`
}

type chatModelMessage struct {
	Content          string             `json:"content"`
	ReasoningContent string             `json:"reasoning_content"`
	Reasoning        string             `json:"reasoning"`
	ReasoningText    string             `json:"reasoning_text"`
	ToolCalls        []chatFunctionCall `json:"tool_calls"`
}

type chatFunctionCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type streamResponse struct {
	Choices []struct {
		Index        int              `json:"index"`
		Delta        chatModelMessage `json:"delta"`
		FinishReason string           `json:"finish_reason"`
	} `json:"choices"`
	Error responsesAPIError `json:"error"`
	Usage *generationUsage  `json:"usage"`
}

type responsesRequest struct {
	Model           string               `json:"model"`
	Input           []responsesInputItem `json:"input"`
	Temperature     float64              `json:"temperature,omitempty"`
	TopP            float64              `json:"top_p,omitempty"`
	MaxOutputTokens int                  `json:"max_output_tokens,omitempty"`
	Stream          bool                 `json:"stream,omitempty"`
}

type responsesInputItem struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responsesContentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

const responsesMaxBytes = 1024 * 1024

func readResponsesBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, responsesMaxBytes+1))
	if err != nil {
		return data, err
	}
	if len(data) > responsesMaxBytes {
		return data, fmt.Errorf("openai responses: response body exceeds %d bytes", responsesMaxBytes)
	}
	return data, nil
}

type responsesAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responsesOutputItem struct {
	Type      string                 `json:"type"`
	Role      string                 `json:"role"`
	ID        string                 `json:"id"`
	CallID    string                 `json:"call_id"`
	Name      string                 `json:"name"`
	Arguments string                 `json:"arguments"`
	Status    string                 `json:"status"`
	Content   []responsesContentItem `json:"content"`
	Summary   []responsesContentItem `json:"summary"`
}

type responsesResponse struct {
	Status            string            `json:"status"`
	Error             responsesAPIError `json:"error"`
	IncompleteDetails struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []responsesOutputItem `json:"output"`
	Usage  *generationUsage      `json:"usage"`
}

func (r responsesResponse) failureDetail() string {
	return firstNonEmpty(r.Error.Message, r.Error.Code, r.IncompleteDetails.Reason, r.Status, "missing response status")
}

func (r responsesResponse) completed() error {
	if r.Status != "completed" || r.Error.Message != "" || r.Error.Code != "" {
		return fmt.Errorf("openai responses did not complete: %s", r.failureDetail())
	}
	return nil
}

type responsesPart struct {
	Output int
	Index  int
	Kind   string
}

func (r responsesResponse) eachText(emit func(responsesPart, Delta) error) error {
	for outputIndex, item := range r.Output {
		for contentIndex, content := range item.Content {
			var delta Delta
			switch {
			case item.Type == "message" && item.Role == "assistant" && content.Type == "output_text":
				delta.Content = content.Text
			case item.Type == "reasoning" && content.Type == "reasoning_text":
				delta.Thinking = content.Text
			default:
				continue
			}
			if delta.Content != "" || delta.Thinking != "" {
				if err := emit(responsesPart{Output: outputIndex, Index: contentIndex, Kind: content.Type}, delta); err != nil {
					return err
				}
			}
		}
		if item.Type == "reasoning" {
			for summaryIndex, summary := range item.Summary {
				if summary.Type == "summary_text" && summary.Text != "" {
					if err := emit(responsesPart{Output: outputIndex, Index: summaryIndex, Kind: summary.Type}, Delta{Thinking: summary.Text}); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

type responsesStreamEvent struct {
	Type         string              `json:"type"`
	Delta        json.RawMessage     `json:"delta"`
	OutputIndex  int                 `json:"output_index"`
	ContentIndex int                 `json:"content_index"`
	SummaryIndex int                 `json:"summary_index"`
	Response     responsesResponse   `json:"response"`
	Code         string              `json:"code"`
	Message      string              `json:"message"`
	Error        responsesAPIError   `json:"error"`
	Item         responsesOutputItem `json:"item"`
	ItemID       string              `json:"item_id"`
	Arguments    string              `json:"arguments"`
	Text         string              `json:"text"`
}
