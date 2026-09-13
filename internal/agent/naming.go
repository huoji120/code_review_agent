package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"code-review-agent/internal/llm"
)

// Naming owns no audit history. Its bounded request runs beside the worker and
// is cancelled and drained before the worker can release its board/workspace.
func (a *Agent) startNaming(ctx context.Context, emit func(Event)) func() {
	board, client, id, phase := a.board, a.nameClient, a.id, a.phase
	if board == nil || client == nil || board.Name(id) != "" {
		return func() {}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return
		}
		messages := []llm.Message{
			{Role: llm.RoleSystem, Content: "Agent nickname. 只返回 JSON {\"name\":\"名字\"}，不要思考过程、工具调用或解释。独立选一个轻松好玩的普通食物、水果或物件词：2–4 个汉字，或 3–8 个英文字母的短词/缩写。避免古风诗意称号、工作职责、带数字的角色名。参考随机 nonce 自由发挥，不把 nonce 或路由 ID 当作名字。"},
			{Role: llm.RoleUser, Content: "routing_id: " + id + "\nnonce: " + hex.EncodeToString(nonce[:])},
		}
		for attempt := 0; attempt < 3 && ctx.Err() == nil; attempt++ {
			text, err := client.Chat(ctx, messages)
			if err != nil || ctx.Err() != nil {
				return
			}
			var result struct {
				Name string `json:"name"`
			}
			candidate := strings.TrimSpace(removeThinkBlocks(text))
			if start := strings.Index(candidate, "{"); start >= 0 {
				if end := strings.LastIndex(candidate, "}"); end >= start {
					candidate = candidate[start : end+1]
				}
			}
			err = json.Unmarshal([]byte(candidate), &result)
			if err == nil && playfulName(result.Name) {
				err = board.RegisterName(id, result.Name)
				if err == nil {
					if emit != nil {
						emit(Event{Kind: "name", AgentID: id, Phase: phase, Content: board.Name(id)})
					}
					return
				}
			}
			messages = append(messages, llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("候选不符合格式或已被使用；请换一个不同的普通词，仅返回 JSON {\"name\":\"名字\"}。重试 %d。", attempt+1)})
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { cancel(); <-done }) }
}

func playfulName(name string) bool {
	if name != strings.TrimSpace(name) || !utf8.ValidString(name) {
		return false
	}
	n, english, han := 0, true, true
	for _, r := range name {
		n++
		english = english && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
		han = han && unicode.Is(unicode.Han, r)
	}
	return english && n >= 3 && n <= 8 || han && n >= 2 && n <= 4
}
