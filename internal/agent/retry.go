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

// A failed request is retried twice, then cooled down once and probed once.
// The callback cancels the entire team only when the recovery probe fails.
func (a *Agent) modelRequest(ctx context.Context, emit func(Event), request func() (string, error)) (string, error) {
	wait := a.waitRetry
	if wait == nil {
		wait = waitModelRetry
	}
	incompleteRetries := 0
	for attempt := 0; attempt < 4; attempt++ {
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
			attempt--
			continue
		}
		if !isModelDisconnect(err) {
			return "", err
		}
		if attempt == 3 {
			if a.onDisconnect != nil {
				a.onDisconnect(err)
			}
			return "", fmt.Errorf("模型服务恢复探测失败，任务已暂停；输入 go 可继续: %w", err)
		}
		if attempt == 2 {
			if emit != nil {
				emit(Event{Kind: "waiting", Content: "模型服务连续 3 次断线；等待 5 分钟后仅探测一次，失败将暂停整个任务"})
			}
			if err := wait(ctx, 5*time.Minute); err != nil {
				return "", err
			}
		} else if emit != nil {
			emit(Event{Kind: "info", Content: fmt.Sprintf("模型服务断线，正在尝试第 %d/3 次请求", attempt+2)})
		}
	}
	panic("unreachable model retry state")
}

func (t *Team) modelDisconnected(err error) {
	t.mu.Lock()
	cancel := t.cancel
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	t.publish(Event{Kind: "error", Content: "模型服务在五分钟冷却后的探测仍失败；整个任务已安全暂停，输入 go 继续。" + err.Error()})
}
