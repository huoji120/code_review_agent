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

	"code-review-agent/internal/config"
	"code-review-agent/internal/forum"
	"code-review-agent/internal/llm"
	"code-review-agent/internal/prompt"
	"code-review-agent/internal/tools"
)

type WorkerStatus struct {
	ID                string                  `json:"id"`
	Name              string                  `json:"name,omitempty"`
	Phase             string                  `json:"phase"`
	Status            string                  `json:"status"`
	Activity          string                  `json:"activity"`
	Turn              int                     `json:"turn"`
	LastModelActivity string                  `json:"last_model_activity,omitempty"`
	LastToolActivity  string                  `json:"last_tool_activity,omitempty"`
	Generation        *llm.GenerationProgress `json:"generation,omitempty"`
	LastTool          string                  `json:"last_tool,omitempty"`
	OutputExcerpt     string                  `json:"output_excerpt,omitempty"`
	OutputPartial     bool                    `json:"output_partial,omitempty"`
}

type teamWorker struct {
	agent          *Agent
	saved          workerSession
	wake           chan struct{}
	active         bool
	activityCancel context.CancelFunc
}

// Workers own their models' histories and registries. Readers only see immutable
// checkpoints; no UI or sibling ever reads a running worker's mutable state.
type Team struct {
	mu                 sync.Mutex
	emitMu             sync.Mutex
	saveMu             sync.Mutex
	cfg                config.Config
	prompts            prompt.Prompts
	client             llm.Client
	compressClient     llm.Client
	nameClient         llm.Client
	registry           *tools.Registry
	board              *forum.Board
	workers            []*teamWorker
	moderator          *teamWorker
	moderatorWake      chan struct{}
	moderatorReviews   chan moderatorReview
	moderatorTicks     <-chan time.Time
	revocations        []FindingRevocation
	pendingAssignments map[string][]string
	workerCancels      map[string]map[string]context.CancelFunc
	phase              string
	input              string
	handoff            string
	snapshot           tools.Snapshot
	running            bool
	cancel             context.CancelFunc
	done               chan struct{}
	emitter            func(Event)
	ircMessages        []IRCMessage
	ircMu              sync.Mutex
	ircPublishing      int
	ircNextID          int64
	ircChanged         chan struct{}
	stageOpen          bool
}

func NewTeam(cfg config.Config, prompts prompt.Prompts, client, compressClient llm.Client, registry *tools.Registry) *Team {
	if cfg.Agent.ReconAgents == 0 {
		cfg.Agent.ReconAgents = 4
	}
	if cfg.Agent.AuditAgents == 0 {
		cfg.Agent.AuditAgents = 4
	}
	if compressClient == nil {
		compressClient = client
	}
	t := &Team{cfg: cfg, prompts: prompts, client: client, compressClient: compressClient, registry: registry, phase: phaseRecon}
	nameConfig := cfg.OpenAI
	nameConfig.Temperature, nameConfig.TopP = 1, 1
	nameConfig.MaxOutputTokens, nameConfig.TimeoutSeconds = 256, 30
	t.nameClient = llm.NewOpenAIClient(nameConfig)
	t.resetBoard()
	return t
}

func (t *Team) resetBoard() {
	t.board = forum.New(time.Duration(t.cfg.Agent.ForumWaitSeconds) * time.Second)
	t.board.Register("user", "user")
	t.board.Register("coordinator", "system")
	if t.cfg.Agent.ModeratorIsEnabled() {
		t.board.Register(phaseModerator, phaseModerator)
		_ = t.board.RegisterName(phaseModerator, "论坛管理员")
	}
	for _, stage := range []struct {
		name  string
		count int
	}{{phaseRecon, t.cfg.Agent.ReconAgents}, {phaseAudit, t.cfg.Agent.AuditAgents}} {
		ids := make([]string, 0, stage.count)
		for i := 1; i <= stage.count; i++ {
			id := fmt.Sprintf("%s-%d", stage.name, i)
			t.board.Register(id, stage.name)
			t.board.SetStatus(id, "pending")
			ids = append(ids, id)
		}
		t.board.SetConsensusMembers(stage.name, ids)
	}
	t.board.SetOnPost(func(message forum.Message) {
		t.publish(Event{Kind: "forum", AgentID: message.AgentID, Phase: message.Stage, Forum: &message})
		if message.AgentID != phaseModerator {
			t.wakeModerator()
		}
	})
	t.board.SetOnConsensus(func(stage string) {
		t.cancelStageWorkers(stage)
	})
}

func (t *Team) cancelStageWorkers(stage string) {
	t.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(t.workerCancels[stage]))
	for _, cancel := range t.workerCancels[stage] {
		cancels = append(cancels, cancel)
	}
	t.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (t *Team) publish(e Event) {
	t.emitMu.Lock()
	defer t.emitMu.Unlock()
	t.mu.Lock()
	emit := t.emitter
	t.mu.Unlock()
	if emit != nil {
		emit(e)
	}
}

func (t *Team) SetWorkspace(workspace string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running {
		return fmt.Errorf("审计尚未停止，不能切换工作区")
	}
	if err := t.registry.SetWorkspace(workspace); err != nil {
		return err
	}
	for _, w := range t.workers {
		_ = w.agent.tools.Close()
	}
	if t.moderator != nil {
		_ = t.moderator.agent.tools.Close()
		t.moderator = nil
	}
	t.revocations = nil
	t.pendingAssignments = nil
	t.ircMessages, t.ircNextID = nil, 0
	t.stageOpen = false
	t.signalIRCLocked()
	t.workers = nil
	t.phase, t.input, t.handoff = phaseRecon, "", ""
	t.snapshot = tools.Snapshot{}
	t.resetBoard()
	return nil
}

func (t *Team) Phase() string { t.mu.Lock(); defer t.mu.Unlock(); return t.phase }
func (t *Team) Snapshot() tools.Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return cloneSnapshot(t.snapshot)
}
func (t *Team) Statuses() []WorkerStatus { t.mu.Lock(); defer t.mu.Unlock(); return t.statusesLocked() }
func (t *Team) statusesLocked() []WorkerStatus {
	out := make([]WorkerStatus, 0, len(t.workers))
	for _, w := range t.workers {
		out = append(out, cloneWorkerStatus(w.saved.Status))
	}
	if t.moderator != nil {
		out = append(out, cloneWorkerStatus(t.moderator.saved.Status))
	}
	return out
}
func (t *Team) LoadedSkills() []string { t.mu.Lock(); defer t.mu.Unlock(); return t.skillsLocked() }
func (t *Team) skillsLocked() []string {
	set := map[string]bool{}
	for _, w := range t.workers {
		for _, s := range w.saved.Skills {
			set[s] = true
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
func (t *Team) ForumMessages() []forum.Message {
	t.mu.Lock()
	b := t.board
	t.mu.Unlock()
	return b.Messages()
}
func (t *Team) PostMessage(content string) error {
	t.mu.Lock()
	b := t.board
	t.mu.Unlock()
	_, err := b.Post("user", "user", "*", 0, "用户补充", strings.TrimSpace(content))
	return err
}

func (t *Team) ReplyMessage(replyTo int64, content string) error {
	t.mu.Lock()
	b := t.board
	t.mu.Unlock()
	_, err := b.Post("user", "user", "*", replyTo, "用户回复", strings.TrimSpace(content))
	return err
}

func (t *Team) ForumPosts(page, pageSize int, search string) forum.PostPage {
	t.mu.Lock()
	board := t.board
	t.mu.Unlock()
	return board.ListPosts(page, pageSize, search)
}

func (t *Team) stateEventLocked() Event {
	s := t.snapshot
	return Event{Kind: "state", Phase: t.phase, Workers: t.statusesLocked(), Skills: t.skillsLocked(), Todos: s.Todos, Findings: s.Findings, Project: s.Project, Files: s.Files, Variables: s.Variables, Flows: s.Flows, Audit: s.Audit}
}
func (t *Team) emitState() { t.mu.Lock(); e := t.stateEventLocked(); t.mu.Unlock(); t.publish(e) }

func (t *Team) capture(w *teamWorker, a *Agent) {
	cp := workerSession{Status: WorkerStatus{ID: a.id, Phase: a.phase, Turn: a.turn}, Assignment: a.assignment, Messages: append([]llm.Message(nil), a.messages...), Skills: a.prompts.LoadedSkillNames(), Snapshot: cloneSnapshot(a.tools.Snapshot()), Plan: a.plan, TracePath: a.TracePath(), Completed: a.completed, ForumCursor: a.forumCursor, ForumPending: a.forumPending, AnnouncedName: a.announcedName}
	t.mu.Lock()
	cp.Status.Name = a.board.Name(a.id)
	cp.Status.Status, cp.Status.Activity = w.saved.Status.Status, w.saved.Status.Activity
	cp.Status.Generation = cloneGeneration(w.saved.Status.Generation)
	cp.Status.LastModelActivity = w.saved.Status.LastModelActivity
	cp.Status.LastToolActivity = w.saved.Status.LastToolActivity
	cp.Status.LastTool = w.saved.Status.LastTool
	cp.Status.OutputExcerpt = w.saved.Status.OutputExcerpt
	cp.Status.OutputPartial = w.saved.Status.OutputPartial
	cp.ModeratorSuggestions = w.saved.ModeratorSuggestions
	w.saved = cp
	if cp.Completed {
		t.signalIRCLocked()
	}
	t.snapshot = t.aggregateLocked()
	t.mu.Unlock()
}

func (t *Team) receive(w *teamWorker, e Event) {
	if e.Kind == "think_delta" || e.Kind == "assistant_delta" || e.Kind == "tool_call_delta" || e.Kind == "assistant_done" || e.Kind == "assistant" {
		t.mu.Lock()
		if e.Content != "" {
			w.saved.Status.OutputExcerpt = boundedExcerpt(w.saved.Status.OutputExcerpt+e.Content, 2048)
			w.saved.Status.OutputPartial = true
		}
		if e.Kind == "assistant_done" || e.Kind == "assistant" {
			w.saved.Status.OutputPartial = false
		}
		t.mu.Unlock()
		return
	}
	protocolFailure := e.Kind == "error" && errors.Is(w.agent.runErr, ErrToolProtocolFailures)
	t.mu.Lock()
	if e.Kind == "model_progress" {
		w.saved.Status.Generation = cloneGeneration(e.Generation)
		if e.Generation != nil {
			if e.Generation.ReceivedAt != "" {
				w.saved.Status.LastModelActivity = e.Generation.ReceivedAt
			} else {
				w.saved.Status.OutputExcerpt = ""
				w.saved.Status.OutputPartial = true
			}
		}
		e.AgentID, e.Phase = w.saved.Status.ID, w.saved.Status.Phase
		e.Workers = t.statusesLocked()
		t.mu.Unlock()
		t.publish(e)
		return
	}
	var cancel context.CancelFunc
	if protocolFailure {
		cancel = t.cancel
		e.Content += "；整个团队已暂停（包括版主）。请检查模型与所选 API 的原生工具适配，修正后输入 go 继续。"
	}
	if e.Kind == "turn" {
		w.saved.Status.Turn = w.agent.turn
		w.saved.Status.Activity = "思考中"
		e.Kind, e.Content = "worker", ""
	}
	if e.Kind != "name" {
		e.AgentID = w.saved.Status.ID
	}
	e.Phase = w.saved.Status.Phase
	if e.Kind == "state" {
		e = t.stateEventLocked()
	} else if e.Kind == "name" {
		w.saved.Status.Name = w.agent.board.Name(w.saved.Status.ID)
		e.Workers = t.statusesLocked()
	} else {
		if e.Kind == "waiting" {
			w.saved.Status.Status = "waiting"
		} else if e.Kind == "worker" || e.Kind == "tool" {
			w.saved.Status.Status = "running"
			if e.Kind == "tool" && strings.HasPrefix(e.Content, "calling ") {
				w.saved.Status.LastToolActivity = time.Now().Format(time.RFC3339Nano)
				w.saved.Status.LastTool = strings.TrimPrefix(e.Content, "calling ")
			}
		}
		activity := e.Content
		if e.Kind == "verify_progress" || e.Kind == "verify_done" {
			activity = fmt.Sprintf("独立复核 %d/%d", e.VerifyTurn, e.VerifyLimit)
			e.VerifyStatus = activity
		}
		if e.Kind == "ui_compact" {
			activity = "上下文已压缩"
			e.Content = activity
		}
		if activity != "" {
			w.saved.Status.Activity = shortText(activity, 200)
		}
		e.Workers = t.statusesLocked()
	}
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	t.publish(e)
}

func stageAssignment(stage string, count int) string {
	assignment := fmt.Sprintf("本阶段有 %d 个独立 Agent。系统不预设角色、主题或文件范围。先调用 forum_roster 和 forum_threads 查看同伴，再通过 forum_post 提出候选分工；需要强提醒所有 Agent 时在正文加入 @全体成员（也支持 @all），收到提醒的 Agent 自主决定是否回应；随后用 forum_wait 等待同伴回复（等待有界，超时后继续），根据实际回复自行协商认领范围，避免重复并主动覆盖空白。不要把内部路由 ID 或启动顺序当作分工。名字在后台单独选择，不要调用或等待命名。", count)
	if stage == phaseAudit {
		assignment += "这是全新的审计团队：先调用 read_handoff 按需读取结构化侦察资料，再根据论坛协商结果用 file_review_update 自行选择需要审计的文件；不要假设系统预先分配了任何文件。"
	} else {
		assignment += "当前是侦察阶段：自行选择建图重点，并把跨范围问题发到论坛；只提交结构化侦察交接，不提交漏洞结论。"
	}
	return assignment
}

// Called only at a stage barrier, never while a worker is running.
func (t *Team) createStageLocked(stage string) {
	count := t.cfg.Agent.ReconAgents
	if stage == phaseAudit {
		count = t.cfg.Agent.AuditAgents
	}
	assignment := stageAssignment(stage, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%s-%d", stage, i+1)
		a := newWorker(t.cfg, t.prompts, t.client, t.compressClient, t.registry.Fork(), id, stage, t.board)
		a.nameClient = t.nameClient
		a.onDisconnect = t.modelDisconnected
		a.assignment = assignment
		if stage == phaseAudit {
			a.handoff = t.handoff
		}
		w := &teamWorker{agent: a, saved: workerSession{Status: WorkerStatus{ID: id, Phase: stage, Status: "pending"}, Assignment: a.assignment, Snapshot: cloneSnapshot(a.tools.Snapshot())}}
		a.checkpoint = func(a *Agent) { t.capture(w, a) }
		t.bindIRC(w)
		t.workers = append(t.workers, w)
	}
	t.snapshot = t.aggregateLocked()
}

func (t *Team) Run(ctx context.Context, input string, emit func(Event)) {
	t.emitMu.Lock()
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		t.emitMu.Unlock()
		if emit != nil {
			emit(Event{Kind: "error", Content: "审计团队已经在运行"})
		}
		return
	}
	if t.cfg.Agent.ReconAgents < 1 || t.cfg.Agent.ReconAgents > 32 || t.cfg.Agent.AuditAgents < 1 || t.cfg.Agent.AuditAgents > 32 {
		t.mu.Unlock()
		t.emitMu.Unlock()
		if emit != nil {
			emit(Event{Kind: "error", Content: "Agent 数量必须在 1 到 32 之间"})
		}
		return
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	t.cancel = cancelRun
	t.running = true
	t.done = make(chan struct{})
	t.emitter = emit
	if t.workerCancels == nil {
		t.workerCancels = make(map[string]map[string]context.CancelFunc)
	}
	if t.input == "" {
		t.input = input
	}
	if t.phase == "completed" {
		t.board.ResetConsensus(phaseAudit)
		t.phase = phaseAudit
		for _, w := range t.workers {
			if w.saved.Status.Phase == phaseAudit {
				w.saved.Completed = false
				w.saved.Status.Status = "pending"
				w.saved.Snapshot.Audit = tools.AuditState{}
				w.agent.completed = false
				w.agent.tools.RestoreSnapshot(w.saved.Snapshot)
			}
		}
	}
	if len(t.workers) == 0 {
		t.createStageLocked(phaseRecon)
	}
	t.ensureModeratorLocked()
	t.mu.Unlock()
	t.emitMu.Unlock()
	defer func() {
		t.emitMu.Lock()
		t.mu.Lock()
		// Moderator is already drained by the later-registered defer below.
		cancelRun()
		t.cancel = nil
		t.running = false
		t.stageOpen = false
		for i := range t.ircMessages {
			m := &t.ircMessages[i]
			if m.State == "queued" || m.State == "delivered" {
				m.Error = "cancelled: team paused; delivery/reply retained for resume"
				t.changeIRCLocked(m)
			}
		}
		t.emitter = nil
		close(t.done)
		t.mu.Unlock()
		t.emitMu.Unlock()
	}()
	stopModerator := t.startModerator(runCtx)
	defer func() { stopModerator() }()
	for {
		t.mu.Lock()
		stage := t.phase
		t.mu.Unlock()
		stopActors := t.startStageActors(runCtx, stage, input)
		_, _ = t.board.Post("coordinator", "system", "*", 0, "阶段调度", stage+" 阶段启动；完成成员在阶段屏障前仍可回答 IRC。")
		approved, complete := false, false
		for {
			if !t.waitStageIdle(runCtx, stage) {
				break
			}
			t.mu.Lock()
			complete = true
			for _, w := range t.workers {
				if w.saved.Status.Phase == stage && !w.saved.Completed {
					complete = false
				}
			}
			if complete && stage == phaseRecon {
				// Reconnaissance hands off candidates; it does not prove them first.
				approved = true
				t.stageOpen = false
				t.mu.Unlock()
				break
			}
			t.mu.Unlock()
			if complete {
				approved = t.reviewStage(runCtx, stage)
			}
			if !t.waitStageIdle(runCtx, stage) {
				break
			}
			t.mu.Lock()
			busy := t.stagePendingIRCLocked(stage) || t.ircPublishing > 0
			for _, w := range t.workers {
				if w.saved.Status.Phase == stage && w.active {
					busy = true
				}
			}
			if busy {
				t.mu.Unlock()
				continue
			}
			for _, assignments := range t.pendingAssignments {
				if len(assignments) > 0 {
					approved = false
				}
			}
			// Atomic admission boundary: a send is either drained in this stage or rejected.
			t.stageOpen = false
			t.mu.Unlock()
			break
		}
		t.mu.Lock()
		t.stageOpen = false
		t.mu.Unlock()
		if stage == phaseRecon && complete {
			stopModerator()
		}
		stopActors()
		t.mu.Lock()
		if runCtx.Err() != nil || !complete {
			t.mu.Unlock()
			t.emitState()
			return
		}
		if !approved {
			reopened := t.applyModeratorAssignmentsLocked(stage)
			t.mu.Unlock()
			if reopened {
				t.board.ResetConsensus(stage)
				continue
			}
			t.emitState()
			return
		}
		if stage == phaseRecon {
			for i := range t.ircMessages {
				m := &t.ircMessages[i]
				if m.Stage == phaseRecon && (m.State == "queued" || m.State == "delivered") {
					m.State, m.Error = "failed", "侦察交接已完成；未回复 IRC 已转入审计交接，不再等待侦察成员"
					t.changeIRCLocked(m)
				}
			}
			for _, w := range t.workers {
				if w.saved.Status.Phase == phaseRecon {
					w.saved.ModeratorSuggestions = append(w.saved.ModeratorSuggestions, t.pendingAssignments[w.saved.Status.ID]...)
					delete(t.pendingAssignments, w.saved.Status.ID)
				}
			}
			t.handoff = t.buildHandoffLocked()
			t.phase = phaseAudit
			t.createStageLocked(phaseAudit)
			t.mu.Unlock()
			stopModerator = t.startModerator(runCtx)
			_, _ = t.board.Post("coordinator", "system", "*", 0, "阶段交接", "全部侦察 Agent 已完成；新建审计 Agent，按需继承结构化地图、笔记、待办与论坛，不继承原始对话。")
			continue
		}
		t.phase = "completed"
		t.snapshot = t.aggregateLocked()
		t.mu.Unlock()
		t.emitState()
		return
	}
}

func (t *Team) buildHandoffLocked() string {
	type entry struct {
		AgentID              string             `json:"agent_id"`
		Plan                 *auditPlanDoneArgs `json:"plan"`
		Snapshot             tools.Snapshot     `json:"snapshot"`
		ModeratorSuggestions []string           `json:"moderator_suggestions,omitempty"`
		IRC                  []IRCMessage       `json:"irc,omitempty"`
	}
	entries := make([]entry, 0, t.cfg.Agent.ReconAgents)
	for _, w := range t.workers {
		if w.saved.Status.Phase == phaseRecon {
			e := entry{AgentID: w.saved.Status.ID, Plan: w.saved.Plan, Snapshot: w.saved.Snapshot, ModeratorSuggestions: w.saved.ModeratorSuggestions}
			if pending := t.pendingAssignments[w.saved.Status.ID]; len(pending) > 0 {
				e.ModeratorSuggestions = append(append([]string(nil), e.ModeratorSuggestions...), pending...)
			}
			for _, m := range t.ircMessages {
				if m.Stage == phaseRecon && m.AgentID == e.AgentID {
					e.IRC = append(e.IRC, m)
				}
			}
			entries = append(entries, e)
		}
	}
	data, _ := json.Marshal(tools.Result{OK: true, Data: entries, Message: "完整侦察交接；候选风险、管理员建议与未回复 IRC 都是待核实材料，由审计成员自行协商认领，不是已验证事实或固定分工。论坛记录仍可通过 forum_read 分页读取。"})
	return string(data)
}

func (t *Team) Close() error {
	t.mu.Lock()
	cancel, done := t.cancel, t.done
	t.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var first error
	for _, w := range t.workers {
		if err := w.agent.tools.Close(); err != nil && first == nil {
			first = err
		}
	}
	if t.moderator != nil {
		if err := t.moderator.agent.tools.Close(); err != nil && first == nil {
			first = err
		}
	}
	if err := t.registry.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

func cloneSnapshot(s tools.Snapshot) tools.Snapshot {
	s.Todos = append([]tools.Todo(nil), s.Todos...)
	s.Findings = append([]tools.Finding(nil), s.Findings...)
	s.Files = append([]tools.FileReview(nil), s.Files...)
	s.Variables = append([]tools.VariableReview(nil), s.Variables...)
	s.Flows = append([]tools.FlowReview(nil), s.Flows...)
	for i := range s.Flows {
		s.Flows[i].Files = append([]string(nil), s.Flows[i].Files...)
		s.Flows[i].Variables = append([]string(nil), s.Flows[i].Variables...)
	}
	return s
}

func (t *Team) aggregateLocked() tools.Snapshot {
	var out tools.Snapshot
	stage := t.phase
	if stage == "completed" {
		stage = phaseAudit
	}
	files := map[string]int{}
	findings := map[string]int{}
	var notes, summaries, next []string
	allDone := true
	count := 0
	for _, w := range t.workers {
		s := w.saved
		if s.Status.Phase != stage {
			continue
		}
		count++
		if !s.Completed {
			allDone = false
		}
		id := s.Status.Name
		if id == "" {
			id = "未命名 Agent"
		}
		for _, todo := range s.Snapshot.Todos {
			todo.ID = len(out.Todos) + 1
			todo.Title = "[" + id + "] " + todo.Title
			out.Todos = append(out.Todos, todo)
		}
		for _, f := range s.Snapshot.Findings {
			key := fmt.Sprintf("%s:%d:%s:%s", f.Path, f.Line, strings.ToLower(f.Title), f.CWE)
			if _, ok := findings[key]; !ok {
				f.Key = findingKey(f)
				f.ID = len(out.Findings) + 1
				f.Evidence = "[" + id + "] " + f.Evidence
				findings[key] = len(out.Findings)
				out.Findings = append(out.Findings, f)
			}
		}
		for _, f := range s.Snapshot.Files {
			f.Note = "[" + id + "] " + f.Note
			if at, ok := files[f.Path]; ok {
				previous := &out.Files[at]
				previous.Note += "\n" + f.Note
				if fileStatusRank(f.Status) < fileStatusRank(previous.Status) {
					previous.Status = f.Status
				}
			} else {
				files[f.Path] = len(out.Files)
				out.Files = append(out.Files, f)
			}
		}
		for _, v := range s.Snapshot.Variables {
			v.Note = "[" + id + "] " + v.Note
			out.Variables = append(out.Variables, v)
		}
		for _, f := range s.Snapshot.Flows {
			f.Name = "[" + id + "] " + f.Name
			out.Flows = append(out.Flows, f)
		}
		if s.Snapshot.Project.Note != "" {
			notes = append(notes, "["+id+"]\n"+s.Snapshot.Project.Note)
		}
		if s.Snapshot.Audit.Summary != "" {
			summaries = append(summaries, "["+id+"] "+s.Snapshot.Audit.Summary)
		}
		if s.Snapshot.Audit.NextSteps != "" {
			next = append(next, "["+id+"] "+s.Snapshot.Audit.NextSteps)
		}
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	out.Project.Note = strings.Join(notes, "\n\n")
	out.Audit = tools.AuditState{Ended: t.phase == "completed" && stage == phaseAudit && count > 0 && allDone, Summary: strings.Join(summaries, "\n\n"), NextSteps: strings.Join(next, "\n\n")}
	t.applyRevocationsLocked(&out)
	return out
}
func fileStatusRank(s string) int {
	switch s {
	case "reviewed":
		return 3
	case "skipped":
		return 2
	case "reviewing":
		return 1
	}
	return 0
}
func shortText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xc0) == 0x80 {
		n--
	}
	return s[:n] + "…"
}
