package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"code-review-agent/internal/llm"
	"code-review-agent/internal/tools"
)

// IRC state is independent of model history. IDs identify conversations; ChangeID
// is the cursor for reading delivery/reply transitions as well as new messages.
type IRCMessage struct {
	ID           int64  `json:"message_id"`
	ChangeID     int64  `json:"change_id"`
	AgentID      string `json:"agent_id"`
	Stage        string `json:"stage"`
	Content      string `json:"content"`
	State        string `json:"state"`
	CreatedAt    string `json:"created_at"`
	DeliveredAt  string `json:"delivered_at,omitempty"`
	RepliedAt    string `json:"replied_at,omitempty"`
	Reply        string `json:"reply,omitempty"`
	ForumID      int64  `json:"forum_id,omitempty"`
	ReplyForumID int64  `json:"reply_forum_id,omitempty"`
	Error        string `json:"error,omitempty"`
}

const ircCapacity = 256

func cloneGeneration(p *llm.GenerationProgress) *llm.GenerationProgress {
	if p == nil {
		return nil
	}
	copy := *p
	return &copy
}

func cloneWorkerStatus(s WorkerStatus) WorkerStatus {
	s.Generation = cloneGeneration(s.Generation)
	return s
}

func boundedExcerpt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

func (a *Agent) beginActivity(ctx context.Context) (context.Context, func()) {
	if a.activity != nil {
		return a.activity(ctx)
	}
	return ctx, func() {}
}

func (t *Team) signalIRCLocked() {
	if t.ircChanged != nil {
		close(t.ircChanged)
	}
	t.ircChanged = make(chan struct{})
}

func (t *Team) changeIRCLocked(m *IRCMessage) {
	t.ircNextID++
	m.ChangeID = t.ircNextID
	t.signalIRCLocked()
}

func (t *Team) pendingIRCLocked(id string) bool {
	for _, m := range t.ircMessages {
		if m.AgentID == id && (m.State == "queued" || m.State == "delivered") {
			return true
		}
	}
	return false
}

func (t *Team) stagePendingIRCLocked(stage string) bool {
	for _, m := range t.ircMessages {
		if m.Stage == stage && (m.State == "queued" || m.State == "delivered") {
			return true
		}
	}
	return false
}

func (t *Team) bindIRC(w *teamWorker) {
	a := w.agent
	a.ircTool = func(ctx context.Context, call ToolCall) string { return t.ircCall(ctx, a.id, call) }
	if a.phase == phaseModerator {
		return
	}
	a.reviewReport = func(ctx context.Context, raw json.RawMessage) string { return t.reviewFindingReport(ctx, w, raw) }
	a.ircPending = func() []IRCMessage {
		t.mu.Lock()
		defer t.mu.Unlock()
		var pending []IRCMessage
		for _, m := range t.ircMessages {
			if m.AgentID == a.id && m.State == "delivered" {
				pending = append(pending, m)
			}
		}
		return pending
	}
	a.activity = func(parent context.Context) (context.Context, func()) {
		ctx, cancel := context.WithCancel(parent)
		t.mu.Lock()
		w.activityCancel = cancel
		t.mu.Unlock()
		return ctx, func() {
			t.mu.Lock()
			w.activityCancel = nil
			t.mu.Unlock()
			cancel()
		}
	}
	a.deliverIRC = func(ctx context.Context) bool { return t.deliverWorkerIRC(ctx, w) }
}

func ircResult(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func ircError(err string) string { return ircResult(tools.Result{OK: false, Error: err}) }

func (t *Team) ircCall(ctx context.Context, caller string, call ToolCall) string {
	if err := ctx.Err(); err != nil {
		return ircError(err.Error())
	}
	switch call.Name {
	case "moderator_irc_send":
		if caller != phaseModerator {
			return ircError("moderator permission required")
		}
		var args moderatorAssignment
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return ircError(err.Error())
		}
		return t.sendIRC(ctx, args)
	case "moderator_irc_read":
		if caller != phaseModerator {
			return ircError("moderator permission required")
		}
		return t.readIRC(ctx, call.Arguments)
	case "worker_irc_reply":
		var args struct {
			MessageID int64  `json:"message_id"`
			Content   string `json:"content"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return ircError(err.Error())
		}
		return t.replyIRC(ctx, caller, args.MessageID, args.Content)
	}
	return ircError("unknown IRC tool")
}

func (t *Team) sendIRC(ctx context.Context, args moderatorAssignment) string {
	content := strings.TrimSpace(args.Content)
	if content == "" || len(content) > 4096 {
		return ircError("content must contain 1..4096 bytes")
	}
	t.ircMu.Lock()
	defer t.ircMu.Unlock()
	t.mu.Lock()
	if ctx.Err() != nil || !t.running || !t.stageOpen {
		t.mu.Unlock()
		return ircError("team paused or stage boundary closed; no message queued")
	}
	var target *teamWorker
	for _, w := range t.workers {
		if w.saved.Status.ID == args.AgentID && w.saved.Status.Phase == t.phase {
			target = w
			break
		}
	}
	if target == nil {
		t.mu.Unlock()
		return ircError("recipient must be a real member of the current stage")
	}
	pending := 0
	for _, m := range t.ircMessages {
		if m.AgentID == args.AgentID && (m.State == "queued" || m.State == "delivered") {
			pending++
		}
	}
	if pending >= 8 {
		t.mu.Unlock()
		return ircError("recipient mailbox full (8 pending messages)")
	}
	if len(t.ircMessages) >= ircCapacity {
		remove := -1
		for i, m := range t.ircMessages {
			if m.State == "replied" || m.State == "failed" {
				remove = i
				break
			}
		}
		if remove < 0 {
			t.mu.Unlock()
			return ircError("IRC retention full; no pending messages discarded")
		}
		copy(t.ircMessages[remove:], t.ircMessages[remove+1:])
		t.ircMessages = t.ircMessages[:len(t.ircMessages)-1]
	}
	t.ircNextID++
	m := IRCMessage{ID: t.ircNextID, ChangeID: t.ircNextID, AgentID: args.AgentID, Stage: t.phase, Content: content, State: "queued", CreatedAt: time.Now().Format(time.RFC3339Nano)}
	cancel := target.activityCancel
	t.ircPublishing++
	t.mu.Unlock()
	// Cancel first; the actor is the only owner allowed to insert into history.
	if cancel != nil {
		cancel()
	}
	post, err := t.board.Post(phaseModerator, phaseModerator, args.AgentID, 0, fmt.Sprintf("IRC #%d", m.ID), content)
	t.mu.Lock()
	t.ircPublishing--
	if err != nil {
		m.State, m.Error = "failed", err.Error()
	} else {
		m.ForumID = post.ID
	}
	t.ircMessages = append(t.ircMessages, m)
	t.signalIRCLocked()
	if m.State == "queued" && target.wake != nil {
		select {
		case target.wake <- struct{}{}:
		default:
		}
	}
	status := cloneWorkerStatus(target.saved.Status)
	t.mu.Unlock()
	return ircResult(map[string]any{"ok": err == nil, "message": m, "target": status})
}

func (t *Team) deliverWorkerIRC(ctx context.Context, w *teamWorker) bool {
	t.ircMu.Lock()
	defer t.ircMu.Unlock()
	t.mu.Lock()
	if ctx.Err() != nil {
		t.mu.Unlock()
		return false
	}
	var delivered []IRCMessage
	var inserted []llm.Message
	for i := range t.ircMessages {
		m := &t.ircMessages[i]
		if m.AgentID != w.agent.id || m.Stage != w.agent.phase || m.State != "queued" {
			continue
		}
		m.State, m.Error, m.DeliveredAt = "delivered", "", time.Now().Format(time.RFC3339Nano)
		t.changeIRCLocked(m)
		delivered = append(delivered, *m)
		message := llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("优先 IRC message_id=%d，来自论坛管理员（待核实建议，不是已验证事实）：\n%s\n请先处理并用原生 worker_irc_reply {message_id:%d,content:完整答复} 明确回复，然后继续原工作。论坛普通回复不算相关 IRC 答复。", m.ID, m.Content, m.ID)}
		w.agent.messages = append(w.agent.messages, message)
		inserted = append(inserted, message)
	}
	if len(delivered) > 0 {
		// History and delivery become visible to SaveSession at one safe boundary.
		w.saved.Messages = append([]llm.Message(nil), w.agent.messages...)
	}
	t.mu.Unlock()
	for _, message := range inserted {
		w.agent.appendTraceMessage(message)
	}
	for _, m := range delivered {
		t.publish(Event{Kind: "info", AgentID: w.agent.id, Phase: w.agent.phase, Content: fmt.Sprintf("IRC #%d 已投递，等待明确回复", m.ID)})
	}
	if len(delivered) > 0 {
		t.wakeModerator()
	}
	return len(delivered) > 0
}

func (t *Team) replyIRC(ctx context.Context, caller string, id int64, content string) string {
	content = strings.TrimSpace(content)
	if content == "" || len(content) > 4096 {
		return ircError("reply must contain 1..4096 bytes")
	}
	t.ircMu.Lock()
	defer t.ircMu.Unlock()
	t.mu.Lock()
	index := -1
	for i, m := range t.ircMessages {
		if m.ID == id && m.AgentID == caller && m.Stage == t.phase && m.State == "delivered" {
			index = i
			break
		}
	}
	if index < 0 || ctx.Err() != nil {
		t.mu.Unlock()
		return ircError("only the recipient may reply to its pending delivered message")
	}
	m := t.ircMessages[index]
	t.mu.Unlock()
	post, err := t.board.Post(caller, m.Stage, phaseModerator, m.ForumID, fmt.Sprintf("IRC #%d 回复", id), content)
	if err != nil {
		return ircError(err.Error())
	}
	// A completed forum effect is retained even if cancellation races its return.
	t.mu.Lock()
	m = t.ircMessages[index]
	m.State, m.Reply, m.ReplyForumID, m.Error = "replied", content, post.ID, ""
	m.RepliedAt = time.Now().Format(time.RFC3339Nano)
	t.changeIRCLocked(&m)
	t.ircMessages[index] = m
	t.mu.Unlock()
	t.wakeModerator()
	return ircResult(map[string]any{"ok": true, "message": m})
}

func (t *Team) readIRC(ctx context.Context, raw json.RawMessage) string {
	var args struct {
		MessageID int64 `json:"message_id"`
		AfterID   int64 `json:"after_id"`
		Limit     int   `json:"limit"`
		Timeout   int   `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return ircError(err.Error())
	}
	if args.MessageID < 0 || args.AfterID < 0 || args.Limit < 0 || args.Limit > 64 || args.Timeout < 0 || args.Timeout > 120 {
		return ircError("invalid cursor/limit/timeout (maximum 64 messages, 120 seconds)")
	}
	if args.Limit == 0 {
		args.Limit = 32
	}
	timer := time.NewTimer(time.Duration(args.Timeout) * time.Second)
	defer timer.Stop()
	for {
		t.mu.Lock()
		var rows []IRCMessage
		for _, m := range t.ircMessages {
			if (args.MessageID == 0 || m.ID == args.MessageID) && m.ChangeID > args.AfterID {
				rows = append(rows, m)
			}
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].ChangeID < rows[j].ChangeID })
		more := len(rows) > args.Limit
		if more {
			rows = rows[:args.Limit]
		}
		next := args.AfterID
		if len(rows) > 0 {
			next = rows[len(rows)-1].ChangeID
		}
		statuses := t.statusesLocked()
		if t.ircChanged == nil {
			t.ircChanged = make(chan struct{})
		}
		changed := t.ircChanged
		t.mu.Unlock()
		if len(rows) > 0 || args.Timeout == 0 {
			return ircResult(map[string]any{"ok": true, "messages": rows, "targets": statuses, "next_after_id": next, "has_more": more, "retention_limit": ircCapacity})
		}
		select {
		case <-ctx.Done():
			return ircError(ctx.Err().Error())
		case <-timer.C:
			return ircResult(map[string]any{"ok": true, "messages": []IRCMessage{}, "targets": statuses, "next_after_id": next, "timed_out": true})
		case <-changed:
		}
	}
}

// A completed worker may answer/read evidence without reopening votes or mutating
// its finished handoff. New audit work still uses the explicit assignment/reset path.
func (t *Team) runIRCOnly(ctx context.Context, w *teamWorker) {
	a := w.agent
	a.ircOnly = true
	defer func() { a.ircOnly = false }()
	emit := func(e Event) { t.receive(w, e) }
	for turn := 0; ctx.Err() == nil; turn++ {
		t.mu.Lock()
		pending := t.pendingIRCLocked(a.id)
		t.mu.Unlock()
		if !pending {
			return
		}
		if turn >= 16 {
			a.runErr = fmt.Errorf("IRC 未在16回合内明确回复")
			break
		}
		a.turn++
		emit(Event{Kind: "turn"})
		activityCtx, finish := a.beginActivity(ctx)
		a.deliverIRC(activityCtx)
		answer, err := a.chatStream(activityCtx, emit)
		if err != nil {
			finish()
			if ctx.Err() == nil && errors.Is(err, context.Canceled) {
				continue
			}
			a.runErr = err
			if !errors.Is(err, context.Canceled) {
				emit(Event{Kind: "error", Content: err.Error()})
			}
			break
		}
		call, _ := a.recordResponse(answer)
		emit(Event{Kind: "assistant_done"})
		a.executeNativeTool(activityCtx, emit, call)
		finish()
		a.emitState(emit)
	}
	if ctx.Err() == nil {
		t.failWorkerIRC(w, a.runErr)
	}
}

func (t *Team) failWorkerIRC(w *teamWorker, err error) {
	if err == nil {
		return
	}
	t.mu.Lock()
	for i := range t.ircMessages {
		m := &t.ircMessages[i]
		if m.AgentID == w.agent.id && (m.State == "queued" || m.State == "delivered") {
			m.State, m.Error = "failed", err.Error()
			t.changeIRCLocked(m)
		}
	}
	t.mu.Unlock()
	t.wakeModerator()
}

// One actor owns each Agent until the entire stage (including moderator review)
// drains. Completed actors sleep but remain addressable; there is no second Run.
func (t *Team) startStageActors(ctx context.Context, stage, input string) func() {
	stageCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	t.mu.Lock()
	t.stageOpen = true
	original := t.input
	var workers []*teamWorker
	for _, w := range t.workers {
		if w.saved.Status.Phase != stage {
			continue
		}
		w.wake = make(chan struct{}, 1)
		w.active = !w.saved.Completed || t.pendingIRCLocked(w.agent.id)
		workers = append(workers, w)
	}
	t.workerCancels[stage] = make(map[string]context.CancelFunc)
	t.mu.Unlock()
	for _, w := range workers {
		wg.Add(1)
		go func(w *teamWorker) {
			defer wg.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					w.agent.runErr = fmt.Errorf("worker panic: %v", recovered)
					t.failWorkerIRC(w, w.agent.runErr)
				}
				t.capture(w, w.agent)
				t.mu.Lock()
				if w.saved.Completed {
					w.saved.Status.Status, w.saved.Status.Activity = "completed", "阶段完成"
				}
				if ctx.Err() != nil {
					if !w.saved.Completed {
						w.saved.Status.Status, w.saved.Status.Activity = "cancelled", "已暂停，等待 go"
					}
					for i := range t.ircMessages {
						m := &t.ircMessages[i]
						if m.AgentID == w.agent.id && (m.State == "queued" || m.State == "delivered") {
							m.Error = "cancelled: team paused; delivery/reply retained for resume"
							t.changeIRCLocked(m)
						}
					}
				}
				w.active, w.wake, w.activityCancel = false, nil, nil
				t.signalIRCLocked()
				t.mu.Unlock()
			}()
			if !w.agent.completed && stageCtx.Err() == nil {
				workerCtx, cancel := context.WithCancel(stageCtx)
				t.mu.Lock()
				t.workerCancels[stage][w.agent.id] = cancel
				var assignments []string
				if stage == phaseAudit {
					assignments = t.pendingAssignments[w.agent.id]
					delete(t.pendingAssignments, w.agent.id)
				}
				t.mu.Unlock()
				instruction := "原始审计目标：\n" + original
				for _, work := range assignments {
					instruction += "\n\n管理员建议（自行判断并反馈证据）：\n" + work
				}
				if input != original {
					instruction += "\n\n本次补充要求：\n" + input
				}
				w.agent.Run(workerCtx, instruction, func(e Event) { t.receive(w, e) })
				cancel()
				t.mu.Lock()
				delete(t.workerCancels[stage], w.agent.id)
				t.mu.Unlock()
				if stage == phaseAudit && stageCtx.Err() == nil {
					if decision, approved := t.board.ConsensusApprovedForStage(stage); approved {
						w.agent.completed = true
						s := w.agent.tools.Snapshot()
						s.Audit = tools.AuditState{Ended: true, Summary: decision.Summary, NextSteps: decision.NextSteps}
						w.agent.tools.RestoreSnapshot(s)
					}
				}
			}
			for {
				if stageCtx.Err() != nil {
					return
				}
				t.mu.Lock()
				pending := t.pendingIRCLocked(w.agent.id)
				if pending {
					w.active = true
				}
				t.mu.Unlock()
				if pending {
					t.runIRCOnly(stageCtx, w)
				}
				t.capture(w, w.agent)
				t.mu.Lock()
				pending = t.pendingIRCLocked(w.agent.id)
				if pending && stageCtx.Err() == nil {
					t.mu.Unlock()
					continue
				}
				w.active = false
				status, activity := "completed", "阶段完成"
				if !w.saved.Completed {
					status, activity = "failed", "阶段未完成"
					if w.agent.runErr != nil {
						activity = w.agent.runErr.Error()
					}
				}
				w.saved.Status.Status, w.saved.Status.Activity = status, shortText(activity, 200)
				wake := w.wake
				t.signalIRCLocked()
				t.mu.Unlock()
				t.board.SetStatus(w.agent.id, status)
				t.emitState()
				select {
				case <-stageCtx.Done():
					return
				case <-wake:
				}
			}
		}(w)
	}
	return func() {
		stop()
		wg.Wait()
		t.mu.Lock()
		delete(t.workerCancels, stage)
		t.mu.Unlock()
	}
}

func (t *Team) waitStageIdle(ctx context.Context, stage string) bool {
	for {
		t.mu.Lock()
		busy := t.stagePendingIRCLocked(stage) || t.ircPublishing > 0
		allReconComplete := stage == phaseRecon
		for _, w := range t.workers {
			if w.saved.Status.Phase == stage && !w.saved.Completed {
				allReconComplete = false
			}
			if w.saved.Status.Phase == stage && w.active {
				busy = true
			}
		}
		if t.ircChanged == nil {
			t.ircChanged = make(chan struct{})
		}
		changed := t.ircChanged
		t.mu.Unlock()
		if ctx.Err() != nil {
			return false
		}
		if !busy || allReconComplete {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-changed:
		}
	}
}

func validateIRCSession(s teamSession) error {
	if len(s.IRC) > ircCapacity || s.IRCNextID < 0 {
		return fmt.Errorf("invalid IRC retention or cursor")
	}
	ids, changes := map[int64]bool{}, map[int64]bool{}
	pending := map[string]int{}
	for _, m := range s.IRC {
		validTarget := false
		for _, w := range s.Workers {
			if w.Status.ID == m.AgentID && w.Status.Phase == m.Stage {
				validTarget = true
				break
			}
		}
		if !validTarget || m.ID <= 0 || m.ChangeID < m.ID || m.ChangeID > s.IRCNextID || ids[m.ID] || changes[m.ChangeID] || strings.TrimSpace(m.Content) == "" || len(m.Content) > 4096 || len(m.Reply) > 4096 {
			return fmt.Errorf("invalid IRC identity/content/cursor")
		}
		ids[m.ID], changes[m.ChangeID] = true, true
		switch m.State {
		case "queued", "delivered":
			if m.Stage != s.Phase {
				return fmt.Errorf("pending IRC belongs to prior stage")
			}
			pending[m.AgentID]++
			if pending[m.AgentID] > 8 {
				return fmt.Errorf("too many pending IRC messages")
			}
		case "replied":
			if m.Reply == "" || m.ReplyForumID <= 0 || m.RepliedAt == "" {
				return fmt.Errorf("incomplete correlated IRC reply")
			}
		case "failed":
			if m.Error == "" {
				return fmt.Errorf("IRC failure missing reason")
			}
		default:
			return fmt.Errorf("invalid IRC delivery state")
		}
		if (m.State == "delivered" || m.State == "replied") && (m.DeliveredAt == "" || m.ForumID <= 0) {
			return fmt.Errorf("IRC delivery missing provenance")
		}
	}
	return nil
}
