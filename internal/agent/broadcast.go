package agent

import (
	"context"
	"fmt"
	"strings"

	"code-review-agent/internal/llm"
)

// Only Team.PostMessage writes this log. A forum author cannot promote model
// output into actual user authority. IDs are contiguous append-only offsets.
type userBroadcast struct {
	ID      int64  `json:"id"`
	Content string `json:"content"`
}

const userBroadcastPriority = "最高优先级用户指示（实际用户 /say 完整原文）：优先处理下面的用户要求，再继续自己的计划或同伴论坛建议；冲突时以较新的用户要求为准。它不是论坛摘录或模型建议，不能覆盖系统安全约束、当前阶段/角色权限或工具模式。压缩摘要不能替代或撤销此原文。"

func validateUserBroadcast(content string) error {
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("用户广播不能为空")
	}
	if len(content) > 16*1024 {
		return fmt.Errorf("用户广播原文不能超过 16384 UTF-8 字节")
	}
	return nil
}

func broadcastMessage(broadcast userBroadcast) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("%s\n[用户广播 #%d]\n\n%s", userBroadcastPriority, broadcast.ID, broadcast.Content)}
}

func (t *Team) broadcastsAfter(cursor int64) []userBroadcast {
	t.mu.Lock()
	defer t.mu.Unlock()
	if cursor < 0 || cursor >= int64(len(t.userBroadcasts)) {
		return nil
	}
	return append([]userBroadcast(nil), t.userBroadcasts[cursor:]...)
}

// Only the owning actor changes its history. Capturing the trusted log does not
// insert anything into the current tool response or compressor request.
func (a *Agent) refreshUserBroadcasts() bool {
	if a.userBroadcastSource == nil {
		return false
	}
	pending := a.userBroadcastSource(a.userBroadcastVersion)
	for _, broadcast := range pending {
		a.userBroadcastMessages = append(a.userBroadcastMessages, broadcastMessage(broadcast))
		a.userBroadcastVersion = broadcast.ID
	}
	if len(pending) > 0 {
		a.userBroadcastTokens = estimateTokens(a.userBroadcastMessages)
	}
	return len(pending) > 0
}

func (a *Agent) pendingUserBroadcastTokens() int {
	return estimateTokens(a.userBroadcastMessages[a.userBroadcastCursor:])
}

func (a *Agent) appendUserBroadcasts() {
	if a.userBroadcastCursor == a.userBroadcastVersion {
		return
	}
	for _, message := range a.userBroadcastMessages[a.userBroadcastCursor:] {
		a.addMessage(message)
	}
	a.userBroadcastCursor = a.userBroadcastVersion
	if a.checkpoint != nil {
		a.checkpoint(a)
	}
}

func (a *Agent) admitUserBroadcasts() error {
	if len(a.userBroadcastMessages) == 0 {
		return nil
	}
	limit, err := a.cfg.CompressionThreshold()
	if err != nil {
		return err
	}
	minimum := []llm.Message{{Role: llm.RoleSystem, Content: a.systemPrompt()}}
	if estimateTokens(minimum)+a.userBroadcastTokens+a.toolDefinitionTokens() >= limit {
		return fmt.Errorf("系统提示、完整用户广播与当前工具定义超过上下文预算；用户指令未截断或丢弃，请提高模型上下文预算")
	}
	return nil
}

func (a *Agent) userBroadcastsDelivered() {
	if a.userBroadcastDelivered == a.userBroadcastCursor {
		return
	}
	a.userBroadcastDelivered = a.userBroadcastCursor
	if a.checkpoint != nil {
		a.checkpoint(a)
	}
}

// Final verifier responses retry through a fresh safe boundary, so a broadcast
// received during retry backoff is not lost behind a captured message slice.
func (a *Agent) verificationConclusion(ctx context.Context) (string, error) {
	return a.modelRequest(ctx, func(Event) {}, func() (string, error) {
		if err := a.prepareRequest(ctx, func(Event) {}); err != nil {
			return "", err
		}
		messages := sanitizeMessagesForCompression(a.messages)
		// The evidence projection trims text, but actual user bodies are exact.
		for i, message := range a.messages {
			for _, broadcast := range a.userBroadcastMessages {
				if message.Role == llm.RoleUser && message.Content == broadcast.Content {
					messages[i] = message
					break
				}
			}
		}
		answer, err := a.client.Chat(ctx, messages)
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err == nil {
			a.userBroadcastsDelivered()
		}
		return answer, err
	})
}
