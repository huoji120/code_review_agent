package llm

import (
	"context"
	"time"
)

type generationUsage struct {
	TotalTokens      *int64 `json:"total_tokens"`
	InputTokens      *int64 `json:"input_tokens"`
	PromptTokens     *int64 `json:"prompt_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	OutputDetails    struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	CompletionDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// Byte counts are deliberately approximate (UTF-8 bytes / 4), not a tokenizer.
// Accumulating bytes before rounding makes the estimate independent of SSE chunking.
type generationTracker struct {
	ctx                                  context.Context
	emit                                 func(Delta) error
	progress                             GenerationProgress
	textBytes, reasoningBytes, toolBytes int64
	lastEmit                             time.Time
	receivedAt                           time.Time
	dirty                                bool
	textLimit                            int
}

func newGenerationTracker(ctx context.Context, emit func(Delta) error) (*generationTracker, error) {
	g := &generationTracker{ctx: ctx, emit: emit, progress: GenerationProgress{Estimated: true}}
	return g, g.flush()
}

func (g *generationTracker) flush() error {
	if err := g.ctx.Err(); err != nil {
		return context.Cause(g.ctx)
	}
	if g.emit != nil {
		if !g.receivedAt.IsZero() {
			g.progress.ReceivedAt = g.receivedAt.UTC().Format(time.RFC3339Nano)
		}
		progress := g.progress
		if err := g.emit(Delta{Progress: &progress}); err != nil {
			return err
		}
	}
	g.lastEmit = time.Now()
	g.dirty = false
	if g.ctx.Err() != nil {
		return context.Cause(g.ctx)
	}
	return nil
}

func (g *generationTracker) add(textBytes, reasoningBytes, toolBytes int) error {
	if textBytes+reasoningBytes+toolBytes == 0 {
		return nil
	}
	streamOutputActivity(g.ctx)
	g.textBytes += int64(textBytes)
	g.reasoningBytes += int64(reasoningBytes)
	g.toolBytes += int64(toolBytes)
	first := g.receivedAt.IsZero()
	now := time.Now()
	g.receivedAt = now
	g.progress.OutputTokens = (g.textBytes + g.reasoningBytes + g.toolBytes + 3) / 4
	g.progress.ReasoningTokens = (g.reasoningBytes + 3) / 4
	g.progress.ToolTokens = (g.toolBytes + 3) / 4
	g.dirty = true
	if first || now.Sub(g.lastEmit) >= 250*time.Millisecond {
		return g.flush()
	}
	return nil
}

func (g *generationTracker) text(delta Delta) error {
	if err := g.add(len(delta.Content), len(delta.Thinking), 0); err != nil {
		return err
	}
	if g.emit != nil {
		return g.emit(delta)
	}
	return nil
}

func (g *generationTracker) finish(usage *generationUsage) error {
	if usage != nil {
		total, reasoning := usage.OutputTokens, usage.OutputDetails.ReasoningTokens
		if total == nil {
			total, reasoning = usage.CompletionTokens, usage.CompletionDetails.ReasoningTokens
		}
		if total != nil && *total >= 0 && reasoning >= 0 && reasoning <= *total {
			g.progress.OutputTokens, g.progress.ReasoningTokens = *total, reasoning
			g.progress.ToolTokens = 0
			g.progress.Estimated = false
			return g.flush()
		}
	}
	return g.flush()
}

// Preserve the latest estimate on failed attempts without replacing their error.
func (g *generationTracker) flushPending() {
	if g.dirty {
		_ = g.flush()
	}
}
