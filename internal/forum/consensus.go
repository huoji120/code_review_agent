package forum

import (
	"fmt"
	"strings"
	"time"
)

const closeConsensusTimeout = 180 * time.Second

const continueAuditWork = "关闭尚未获准；继续自己的审计：自行选择尚未覆盖的其他文件、入口或模块，建立具体待办并读取源码、追踪新的调用链。不要等待或催促其他成员，不要反复请求关闭，也不要围绕其他 Agent 的已有结论重复复核来消磨时间；没有新的独立审计进展，不重复提交同一关闭请求。"

type CloseDecision struct {
	Status    string    `json:"status"`
	RequestID uint64    `json:"request_id,omitempty"`
	Required  int       `json:"required"`
	Approved  int       `json:"approved"`
	Deadline  time.Time `json:"deadline,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	NextSteps string    `json:"next_steps,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// SetConsensusMembers defines the stage workers that must explicitly approve
// a close request. Child verification workers are intentionally not implicit
// voters; the Team supplies only its primary workers.
func (b *Board) SetConsensusMembers(stage string, ids []string) {
	members := make(map[string]bool, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			members[id] = true
		}
	}
	b.mu.Lock()
	b.consensusMembers[stage] = members
	b.mu.Unlock()
}

func (b *Board) SetOnConsensus(fn func(stage string)) {
	b.mu.Lock()
	b.onConsensus = fn
	b.mu.Unlock()
}

func (b *Board) ResetConsensus(stage string) {
	b.mu.Lock()
	delete(b.consensus, stage)
	b.signalLocked()
	b.mu.Unlock()
}

// RequestClose records one worker's explicit close request or vote. Approval
// is unanimous within 180 seconds; any rejection immediately cancels the round
// and tells workers to continue useful work.
func (b *Board) RequestClose(id, stage, summary, nextSteps, vote string) CloseDecision {
	return b.requestCloseAt(id, stage, summary, nextSteps, vote, time.Now())
}

func (b *Board) requestCloseAt(id, stage, summary, nextSteps, vote string, now time.Time) CloseDecision {
	vote = strings.ToLower(strings.TrimSpace(vote))
	approve, reject := closeVote(vote)
	if !approve && !reject {
		return CloseDecision{Status: "invalid", Reason: "vote must be approve or reject"}
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		summary = "审计团队已完成当前审计。"
	}
	nextSteps = strings.TrimSpace(nextSteps)

	var (
		decision CloseDecision
		announce string
		callback func(string)
		startID  uint64
	)
	b.mu.Lock()
	a, ok := b.agents[id]
	if !ok || a.Stage != stage || id == "moderator" || stage == "moderator" {
		b.mu.Unlock()
		return CloseDecision{Status: "invalid", Reason: "未注册的 Agent 或阶段不匹配"}
	}
	members := b.membersLocked(stage)
	if !members[id] {
		b.mu.Unlock()
		return CloseDecision{Status: "invalid", Reason: "该身份不是本阶段的审计 Agent"}
	}
	req := b.consensus[stage]
	if req != nil && req.Status == "pending" && !now.Before(req.Deadline) {
		req.Status = "timed_out"
		decision = closeDecision(req, "其他 Agent 未在 60 秒内全部明确同意，关闭请求已超时，本轮请求不获准。"+continueAuditWork)
		announce = closeAnnouncement("审计关闭请求超时", a.Name, decision)
		b.signalLocked()
		b.mu.Unlock()
		b.announceConsensus(announce)
		return decision
	}
	if req != nil && req.Status == "approved" {
		decision = closeDecision(req, "本阶段关闭请求已经一致通过")
		b.mu.Unlock()
		return decision
	}

	if req == nil || req.Status != "pending" {
		if reject {
			b.mu.Unlock()
			return CloseDecision{Status: "invalid", Reason: "当前没有可否决的关闭请求；先发起新的 end_audit 请求"}
		}
		b.nextConsensusID++
		req = &closeRequest{
			ID:        b.nextConsensusID,
			Stage:     stage,
			Summary:   summary,
			NextSteps: nextSteps,
			Deadline:  now.Add(closeConsensusTimeout),
			Members:   cloneMembers(members),
			Votes:     make(map[string]bool),
			Status:    "pending",
		}
		b.consensus[stage] = req
		req.Votes[id] = true
		startID = req.ID
		decision = closeDecision(req, "关闭请求 pending，尚需本阶段所有 Agent 明确同意。"+continueAuditWork)
		announce = closeAnnouncement("审计关闭请求", a.Name, decision)
		if decision.Approved == decision.Required {
			req.Status = "approved"
			decision = closeDecision(req, "本阶段所有 Agent 已一致同意关闭")
			callback = b.onConsensus
		}
	} else {
		if reject {
			req.Status = "rejected"
			decision = closeDecision(req, fmt.Sprintf("Agent %s 已否决关闭请求，本轮请求已拒绝。%s", a.Name, continueAuditWork))
			announce = closeAnnouncement("审计关闭请求被否决", a.Name, decision)
			b.signalLocked()
			b.mu.Unlock()
			b.announceConsensus(announce)
			return decision
		}
		req.Votes[id] = true
		decision = closeDecision(req, "已记录同意票，关闭请求仍 pending，尚需其他 Agent 明确同意。"+continueAuditWork)
		announce = closeAnnouncement("审计关闭投票", a.Name, decision)
		if decision.Approved == decision.Required {
			req.Status = "approved"
			decision = closeDecision(req, "本阶段所有 Agent 已一致同意关闭")
			callback = b.onConsensus
		}
	}
	b.signalLocked()
	b.mu.Unlock()

	if startID != 0 && decision.Status == "pending" {
		time.AfterFunc(time.Until(now.Add(closeConsensusTimeout)), func() {
			b.expireClose(stage, startID)
		})
	}
	b.announceConsensus(announce)
	if callback != nil {
		callback(stage)
	}
	return decision
}

func closeVote(vote string) (approve, reject bool) {
	switch vote {
	case "approve", "approved", "yes", "同意":
		return true, false
	case "reject", "rejected", "no", "不同意", "否决":
		return false, true
	default:
		return false, false
	}
}

func (b *Board) membersLocked(stage string) map[string]bool {
	members := make(map[string]bool)
	configured := b.consensusMembers[stage]
	for id, a := range b.agents {
		if id != "moderator" && a.Stage != "moderator" && a.Stage == stage && (len(configured) == 0 || configured[id]) {
			members[id] = true
		}
	}
	return members
}

func cloneMembers(members map[string]bool) map[string]bool {
	out := make(map[string]bool, len(members))
	for id, member := range members {
		out[id] = member
	}
	return out
}

func closeDecision(req *closeRequest, reason string) CloseDecision {
	approved := 0
	for id, vote := range req.Votes {
		if vote && req.Members[id] {
			approved++
		}
	}
	return CloseDecision{
		Status:    req.Status,
		RequestID: req.ID,
		Required:  len(req.Members),
		Approved:  approved,
		Deadline:  req.Deadline,
		Summary:   req.Summary,
		NextSteps: req.NextSteps,
		Reason:    reason,
	}
}

func (b *Board) expireClose(stage string, requestID uint64) {
	var announce string
	b.mu.Lock()
	req := b.consensus[stage]
	if req == nil || req.ID != requestID || req.Status != "pending" || time.Now().Before(req.Deadline) {
		b.mu.Unlock()
		return
	}
	req.Status = "timed_out"
	decision := closeDecision(req, "其他 Agent 未在 60 秒内全部明确同意，关闭请求已超时，本轮请求不获准。"+continueAuditWork)
	announce = closeAnnouncement("审计关闭请求超时", "系统", decision)
	b.signalLocked()
	b.mu.Unlock()
	b.announceConsensus(announce)
}

func (b *Board) ConsensusApproved(id, stage string) (CloseDecision, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	req := b.consensus[stage]
	if req == nil || req.Status != "approved" || !req.Members[id] {
		return CloseDecision{}, false
	}
	return closeDecision(req, "本阶段所有 Agent 已一致同意关闭"), true
}

func (b *Board) ConsensusApprovedForStage(stage string) (CloseDecision, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	req := b.consensus[stage]
	if req == nil || req.Status != "approved" {
		return CloseDecision{}, false
	}
	return closeDecision(req, "本阶段所有 Agent 已一致同意关闭"), true
}

func (b *Board) announceConsensus(content string) {
	if strings.TrimSpace(content) == "" {
		return
	}
	_, _ = b.Post("coordinator", "system", "*", 0, "审计协商", content)
}

func closeAnnouncement(topic, actor string, decision CloseDecision) string {
	content := fmt.Sprintf("%s：%s（明确同意 %d/%d，截止 %s）。", actor, decision.Reason, decision.Approved, decision.Required, decision.Deadline.Format("15:04:05"))
	if decision.Summary != "" && topic == "审计关闭请求" {
		content += " 请求摘要：" + excerpt(decision.Summary, 768)
	}
	return content
}
