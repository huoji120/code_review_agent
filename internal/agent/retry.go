package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"code-review-agent/internal/llm"
)

func waitModelRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Only transport/service availability failures enter the disconnect policy.
// Context budgets, invalid tool output and max_turns are never disconnects.
func isModelDisconnect(err error) bool {
	if err == nil || isContextLengthError(err) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var network net.Error
	if errors.As(err, &network) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "openai status 5") || strings.Contains(text, "openai status 408:") || strings.Contains(text, "openai status 429:") || strings.Contains(text, "stream ended without response.completed") || strings.Contains(text, "connection reset") || strings.Contains(text, "connection refused") || strings.Contains(text, "broken pipe")
}

// Inspect API errors only, never generated text or ordinary tool output.
func isBillingExhausted(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{"insufficient", "余额不足", "余额已耗尽", "额度不足", "额度已用尽", "欠费", "payment required", "credit balance", "credits exhausted", "quota exceeded", "quota_exceeded", "billing_hard_limit", "exceeded your current quota", "openai status 402:"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

const (
	modelRetryInitialDelay = time.Second
	modelRetryMaxDelay     = 5 * time.Minute
)

// A network failure is retried until the request context is cancelled. The
// delay grows exponentially, but saturates before duration arithmetic can
// overflow. Billing exhaustion is handled separately and never retried.
func (a *Agent) modelRequest(ctx context.Context, emit func(Event), request func() (string, error)) (string, error) {
	wait := a.waitRetry
	if wait == nil {
		wait = waitModelRetry
	}
	incompleteRetries := 0
	retryDelay := modelRetryInitialDelay
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		answer, err := request()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err == nil {
			return answer, nil
		}
		if isBillingExhausted(err) {
			if a.onDisconnect != nil {
				a.onDisconnect(err)
			}
			return "", fmt.Errorf("API余额或额度不足，已立即终止全队运行，不再重试: %w", err)
		}
		if errors.Is(err, llm.ErrIncompleteGeneration) {
			if incompleteRetries == 3 {
				return "", fmt.Errorf("生成未完成，回退到上次完整结果后重试3次仍失败；保留进度，输入 go 可继续: %w", err)
			}
			incompleteRetries++
			if emit != nil {
				emit(Event{Kind: "info", Content: fmt.Sprintf("生成未完成：%v；丢弃本次残缺输出，回退到上次完整结果，重试 %d/3", err, incompleteRetries)})
			}
			// The caller commits model output only on success. Repeat generation
			// from that boundary, never replay the preceding successful tool.
			continue
		}
		if !isModelDisconnect(err) {
			return "", err
		}
		if emit != nil {
			emit(Event{Kind: "waiting", Content: fmt.Sprintf("模型服务断线；等待 %s 后继续重试（取消任务可停止）", retryDelay)})
		}
		if err := wait(ctx, retryDelay); err != nil {
			return "", err
		}
		if retryDelay < modelRetryMaxDelay {
			if retryDelay > modelRetryMaxDelay/2 {
				retryDelay = modelRetryMaxDelay
			} else {
				retryDelay *= 2
			}
		}
	}
}

func (t *Team) modelDisconnected(err error) {
	if !isBillingExhausted(err) {
		return
	}
	t.mu.Lock()
	cancel := t.cancel
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	t.publish(Event{Kind: "error", Content: "API余额或额度不足：已立即终止全队运行（包括管理员），保留进度，不重试、不等待冷却；充值后可手动 go 继续。" + err.Error()})
}
