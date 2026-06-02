package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"code-review-agent/internal/config"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

type Client interface {
	Chat(ctx context.Context, messages []Message) (string, error)
	ChatStream(ctx context.Context, messages []Message, emit func(Delta) error) error
}

type Delta struct {
	Content  string
	Thinking string
}

type OpenAIClient struct {
	cfg        config.OpenAIConfig
	httpClient *http.Client
}

func (c *OpenAIClient) ChatStream(ctx context.Context, messages []Message, emit func(Delta) error) error {
	if c.cfg.APIKey == "" {
		return fmt.Errorf("missing API key; set openai.api_key directly, or set openai.api_key_env to an environment variable name")
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

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return readErr
		}
		return fmt.Errorf("openai status %d: %s", resp.StatusCode, string(body))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
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
			return nil
		}
		var chunk streamResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return err
		}
		for _, choice := range chunk.Choices {
			delta := Delta{Content: choice.Delta.Content, Thinking: firstNonEmpty(choice.Delta.ReasoningContent, choice.Delta.Reasoning, choice.Delta.ReasoningText)}
			if delta.Content == "" && delta.Thinking == "" {
				continue
			}
			if err := emit(delta); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func NewOpenAIClient(cfg config.OpenAIConfig) *OpenAIClient {
	return &OpenAIClient{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second},
	}
}

func (c *OpenAIClient) Chat(ctx context.Context, messages []Message) (string, error) {
	if c.cfg.Stream {
		var thinking strings.Builder
		var content strings.Builder
		err := c.ChatStream(ctx, messages, func(delta Delta) error {
			thinking.WriteString(delta.Thinking)
			content.WriteString(delta.Content)
			return nil
		})
		return joinAssistantParts(thinking.String(), content.String()), err
	}
	if c.cfg.APIKey == "" {
		return "", fmt.Errorf("missing API key; set openai.api_key directly, or set openai.api_key_env to an environment variable name")
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
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("openai returned no choices")
	}
	msg := parsed.Choices[0].Message
	return joinAssistantParts(firstNonEmpty(msg.ReasoningContent, msg.Reasoning, msg.ReasoningText), msg.Content), nil
}

func (c *OpenAIClient) useResponsesAPI() bool {
	return strings.EqualFold(strings.TrimSpace(c.cfg.APIInterface), "responses")
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
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return readErr
		}
		return fmt.Errorf("openai status %d: %s", resp.StatusCode, string(body))
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			return nil
		}
		var chunk responsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return err
		}
		delta := Delta{Content: firstNonEmpty(chunk.Delta, chunk.Text), Thinking: firstNonEmpty(chunk.ReasoningText, chunk.ReasoningDelta)}
		if delta.Content == "" && delta.Thinking == "" {
			continue
		}
		if err := emit(delta); err != nil {
			return err
		}
	}
	return scanner.Err()
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
	body, err := io.ReadAll(resp.Body)
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
	return joinAssistantParts(parsed.reasoningText(), parsed.outputText()), nil
}

func responsesInput(messages []Message) []responsesInputItem {
	items := make([]responsesInputItem, 0, len(messages))
	for _, msg := range messages {
		items = append(items, responsesInputItem{Role: string(msg.Role), Content: []responsesContentItem{{Type: "input_text", Text: msg.Content}}})
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
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature"`
	TopP        float64   `json:"top_p"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ReasoningText    string `json:"reasoning_text"`
		} `json:"message"`
	} `json:"choices"`
}

type streamResponse struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ReasoningText    string `json:"reasoning_text"`
		} `json:"delta"`
	} `json:"choices"`
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
	Role    string                 `json:"role"`
	Content []responsesContentItem `json:"content"`
}

type responsesContentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesResponse struct {
	Output []struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	OutputText string `json:"output_text"`
	Reasoning  []struct {
		Text string `json:"text"`
	} `json:"reasoning"`
}

func (r responsesResponse) outputText() string {
	if r.OutputText != "" {
		return r.OutputText
	}
	var b strings.Builder
	for _, item := range r.Output {
		for _, content := range item.Content {
			if content.Text != "" {
				b.WriteString(content.Text)
			}
		}
	}
	return b.String()
}

func (r responsesResponse) reasoningText() string {
	var b strings.Builder
	for _, item := range r.Reasoning {
		if item.Text != "" {
			b.WriteString(item.Text)
		}
	}
	return b.String()
}

type responsesStreamEvent struct {
	Type           string `json:"type"`
	Delta          string `json:"delta"`
	Text           string `json:"text"`
	ReasoningText  string `json:"reasoning_text"`
	ReasoningDelta string `json:"reasoning_delta"`
}
