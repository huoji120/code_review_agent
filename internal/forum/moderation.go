package forum

import (
	"encoding/json"
	"fmt"
	"strings"
)

const maxModerationReason = 512

func copyModeration(dst *Message, src Message) {
	dst.Closed = src.Closed
	dst.Pinned = src.Pinned
	dst.Announcement = src.Announcement
	dst.ModerationReason = src.ModerationReason
}

func sameModeration(a, b Message) bool {
	return a.Closed == b.Closed && a.Pinned == b.Pinned && a.Announcement == b.Announcement && a.ModerationReason == b.ModerationReason
}

// Validate the complete snapshot before changing the board. Legacy messages
// without thread IDs are normalized, but contradictory thread state is rejected
// rather than allowing a retained reply to silently reopen a closed thread.
func validateRestoredMessages(messages []Message) ([]Message, int64, error) {
	normalized := make([]Message, 0, len(messages))
	parents := make(map[int64]int64, len(messages))
	states := make(map[int64]Message)
	var previous int64
	for _, m := range messages {
		if m.ID <= previous || m.AgentID == "" || m.Stage == "" || strings.TrimSpace(m.Content) == "" || len(m.Content) > maxContent || len(m.Topic) > maxTopic || m.ReplyTo < 0 || m.ReplyTo >= m.ID || m.ThreadID < 0 || m.ThreadID > m.ID {
			return nil, 0, fmt.Errorf("无效的论坛会话记录")
		}
		if m.AgentName != "" && !validName(m.AgentName) {
			return nil, 0, fmt.Errorf("无效的论坛作者名字")
		}
		if len(m.ModerationReason) > maxModerationReason || (m.ModerationReason != "" && strings.TrimSpace(m.ModerationReason) == "") || (m.Closed && m.ModerationReason == "") {
			return nil, 0, fmt.Errorf("无效的线程管理理由")
		}
		parent := parents[m.ReplyTo]
		if m.ThreadID == 0 {
			m.ThreadID = m.ID
			if m.ReplyTo > 0 {
				m.ThreadID = parent
				if m.ThreadID == 0 {
					m.ThreadID = m.ReplyTo
				}
			}
		}
		if (m.ReplyTo == 0 && m.ThreadID != m.ID) || (m.ReplyTo > 0 && (m.ThreadID > m.ReplyTo || (parent != 0 && parent != m.ThreadID))) {
			return nil, 0, fmt.Errorf("无效的论坛线程关系")
		}
		if state, ok := states[m.ThreadID]; ok && !sameModeration(state, m) {
			return nil, 0, fmt.Errorf("论坛线程管理状态不一致")
		}
		if m.ID == m.ThreadID && m.Announcement && (m.AgentID != "moderator" || m.Stage != "moderator") {
			return nil, 0, fmt.Errorf("公告作者不是论坛管理员")
		}
		states[m.ThreadID] = m
		parents[m.ID] = m.ThreadID
		normalized = append(normalized, m)
		previous = m.ID
	}
	return normalized, previous, nil
}

func (b *Board) moderatorCall(id, stage, name string, raw json.RawMessage) string {
	if name == "forum_announce" {
		var args struct {
			Topic   string `json:"topic"`
			Content string `json:"content"`
			Pinned  bool   `json:"pinned"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return failure("无效公告参数")
		}
		m, err := b.post(id, stage, "*", 0, args.Topic, args.Content, true, args.Pinned)
		if err != nil {
			return failure(err.Error())
		}
		return marshal(struct {
			OK      bool    `json:"ok"`
			Message Message `json:"message"`
		}{true, m})
	}
	var args struct {
		ThreadID int64  `json:"thread_id"`
		Action   string `json:"action"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || args.ThreadID <= 0 {
		return failure("无效线程管理参数")
	}
	args.Reason = strings.TrimSpace(args.Reason)
	if args.Reason == "" || len(args.Reason) > maxModerationReason {
		return failure("reason 必须非空且不超过 512 字节")
	}
	if args.Action != "close" && args.Action != "reopen" && args.Action != "pin" && args.Action != "unpin" {
		return failure("action 必须为 close/reopen/pin/unpin")
	}
	b.mu.Lock()
	var state Message
	for _, m := range b.msgs {
		if m.ID == args.ThreadID || m.ThreadID == args.ThreadID {
			state = m
			break
		}
	}
	if state.ID == 0 {
		b.mu.Unlock()
		return failure("线程不存在或已淘汰")
	}
	switch args.Action {
	case "close":
		state.Closed = true
	case "reopen":
		state.Closed = false
	case "pin":
		state.Pinned = true
	case "unpin":
		state.Pinned = false
	}
	state.ModerationReason = args.Reason
	for i := range b.msgs {
		if b.msgs[i].ThreadID == state.ThreadID {
			b.bytes -= messageBytes(b.msgs[i])
			copyModeration(&b.msgs[i], state)
			b.bytes += messageBytes(b.msgs[i])
		}
	}
	b.trimLocked()
	b.signalLocked()
	b.mu.Unlock()
	// The retained action record is a separate root: closing never needs a
	// privileged reply bypass. Post invokes its hook only after releasing mu.
	log, err := b.Post(id, stage, "*", 0, "论坛管理记录", fmt.Sprintf("@全体成员 线程 #%d：%s。理由：%s", state.ThreadID, args.Action, args.Reason))
	if err != nil {
		return failure(fmt.Sprintf("管理动作已应用，但记录失败：%v", err))
	}
	return marshal(struct {
		OK               bool   `json:"ok"`
		ThreadID         int64  `json:"thread_id"`
		Closed           bool   `json:"closed"`
		Pinned           bool   `json:"pinned"`
		Announcement     bool   `json:"announcement"`
		ModerationReason string `json:"moderation_reason"`
		LogMessageID     int64  `json:"log_message_id"`
	}{true, state.ThreadID, state.Closed, state.Pinned, state.Announcement, state.ModerationReason, log.ID})
}
