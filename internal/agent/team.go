package agent

import (
	"context"
	"encoding/json"
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
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Phase    string `json:"phase"`
	Status   string `json:"status"`
	Activity string `json:"activity"`
	Turn     int    `json:"turn"`
}

type teamWorker struct {
	agent *Agent
	saved workerSession
}

// Workers own their models' histories and registries. Readers only see immutable
// checkpoints; no UI or sibling ever reads a running worker's mutable state.
type Team struct {
	mu             sync.Mutex
	emitMu         sync.Mutex
	saveMu         sync.Mutex
	cfg            config.Config
	prompts        prompt.Prompts
	client         llm.Client
	compressClient llm.Client
	nameClient     llm.Client
	registry       *tools.Registry
	board          *forum.Board
	workers        []*teamWorker
	phase          string
	input          string
	handoff        string
	snapshot       tools.Snapshot
	running        bool
	cancel         context.CancelFunc
	done           chan struct{}
	emitter        func(Event)
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
	for _, stage := range []struct {
		name  string
		count int
	}{{phaseRecon, t.cfg.Agent.ReconAgents}, {phaseAudit, t.cfg.Agent.AuditAgents}} {
		for i := 1; i <= stage.count; i++ {
			id := fmt.Sprintf("%s-%d", stage.name, i)
			t.board.Register(id, stage.name)
			t.board.SetStatus(id, "pending")
		}
	}
	t.board.SetOnPost(func(message forum.Message) {
		t.publish(Event{Kind: "forum", AgentID: message.AgentID, Phase: message.Stage, Forum: &message})
	})
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
		out = append(out, w.saved.Status)
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
	w.saved = cp
	t.snapshot = t.aggregateLocked()
	t.mu.Unlock()
}

func (t *Team) receive(w *teamWorker, e Event) {
	switch e.Kind {
	case "think_delta", "assistant_delta", "tool_call_delta", "assistant_done", "assistant":
		return
	}
	t.mu.Lock()
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
	t.publish(e)
}

var reconFocus = []string{"架构、入口、配置与信任边界", "认证、授权、会话与敏感资产", "外部输入、数据流、数据库与危险 sink", "文件操作、上传、插件、依赖及跨模块边界"}
var auditFocus = []string{"入口可达性和身份权限", "跨文件数据传播和输入校验", "危险 sink 与实际影响", "否定条件、边界条件和相邻同类路径"}

// Called only at a stage barrier, never while a worker is running.
func (t *Team) createStageLocked(stage string) {
	count, focus := t.cfg.Agent.ReconAgents, reconFocus
	if stage == phaseAudit {
		count, focus = t.cfg.Agent.AuditAgents, auditFocus
	}
	var paths []string
	if stage == phaseAudit {
		set := map[string]bool{}
		for _, w := range t.workers {
			if w.saved.Plan != nil {
				for _, p := range w.saved.Plan.AuditFiles {
					set[p] = true
				}
			}
		}
		for p := range set {
			paths = append(paths, p)
		}
		sort.Strings(paths)
	}
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%s-%d", stage, i+1)
		a := newWorker(t.cfg, t.prompts, t.client, t.compressClient, t.registry.Fork(), id, stage, t.board)
		a.nameClient = t.nameClient
		a.assignment = fmt.Sprintf("本阶段有 %d 个独立 Agent，你是第 %d 个；重点：%s。名字在后台单独选择，不要调用或等待命名。立即用 forum_roster 查看同伴，在 forum_post 声明负责范围，避免重复；跨范围证据必须主动交流。", count, i+1, focus[i%len(focus)])
		if stage == phaseAudit {
			a.handoff = t.handoff
			var assigned []string
			for j, p := range paths {
				if j%count == i {
					assigned = append(assigned, p)
				}
			}
			if len(assigned) > 0 {
				a.tools.ApplyAuditScope(assigned)
			}
			a.assignment += " 已将你的首要负责文件写入 review_state；必须审完，不得只检查重点方向。可跨文件读取和交叉复核；若未分配文件，独立复核其他人的链路并通过论坛反馈。read_handoff 包含所有侦察 Agent 的地图、笔记、待办和未决问题。"
		}
		w := &teamWorker{agent: a, saved: workerSession{Status: WorkerStatus{ID: id, Phase: stage, Status: "pending"}, Assignment: a.assignment, Snapshot: cloneSnapshot(a.tools.Snapshot())}}
		a.checkpoint = func(a *Agent) { t.capture(w, a) }
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
	if t.input == "" {
		t.input = input
	}
	if t.phase == "completed" {
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
	t.mu.Unlock()
	t.emitMu.Unlock()
	defer func() {
		t.emitMu.Lock()
		t.mu.Lock()
		cancelRun()
		t.cancel = nil
		t.running = false
		t.emitter = nil
		close(t.done)
		t.mu.Unlock()
		t.emitMu.Unlock()
	}()
	for {
		t.mu.Lock()
		stage := t.phase
		var active []*teamWorker
		for _, w := range t.workers {
			if w.saved.Status.Phase == stage && !w.saved.Completed {
				w.saved.Status.Status = "running"
				active = append(active, w)
			}
		}
		original := t.input
		t.mu.Unlock()
		for _, w := range active {
			t.board.SetStatus(w.agent.id, "running")
		}
		t.emitState()
		_, _ = t.board.Post("coordinator", "system", "*", 0, "阶段调度", fmt.Sprintf("%s 阶段启动，%d 个独立 Agent 并发工作。", stage, len(active)))
		var wg sync.WaitGroup
		for _, w := range active {
			wg.Add(1)
			go func(w *teamWorker) {
				defer wg.Done()
				defer func() {
					if recovered := recover(); recovered != nil {
						w.agent.runErr = fmt.Errorf("worker panic: %v", recovered)
					}
					t.capture(w, w.agent)
					status, activity := "completed", "阶段完成"
					if !w.agent.completed {
						status, activity = "failed", "阶段未完成"
						if w.agent.runErr != nil {
							activity = w.agent.runErr.Error()
						}
						if runCtx.Err() != nil {
							status = "cancelled"
						}
					}
					t.mu.Lock()
					w.saved.Status.Status = status
					w.saved.Status.Activity = shortText(activity, 200)
					t.snapshot = t.aggregateLocked()
					t.mu.Unlock()
					t.board.SetStatus(w.agent.id, status)
					t.emitState()
				}()
				instruction := "原始审计目标：\n" + original
				if input != original {
					instruction += "\n\n本次补充要求：\n" + input
				}
				w.agent.Run(runCtx, instruction, func(e Event) { t.receive(w, e) })
			}(w)
		}
		wg.Wait()
		t.mu.Lock()
		complete := true
		for _, w := range t.workers {
			if w.saved.Status.Phase == stage && !w.saved.Completed {
				complete = false
			}
		}
		if runCtx.Err() != nil || !complete {
			t.mu.Unlock()
			t.emitState()
			return
		}
		if stage == phaseRecon {
			t.handoff = t.buildHandoffLocked()
			t.phase = phaseAudit
			t.createStageLocked(phaseAudit)
			t.mu.Unlock()
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
		AgentID  string             `json:"agent_id"`
		Plan     *auditPlanDoneArgs `json:"plan"`
		Snapshot tools.Snapshot     `json:"snapshot"`
	}
	entries := make([]entry, 0, t.cfg.Agent.ReconAgents)
	for _, w := range t.workers {
		if w.saved.Status.Phase == phaseRecon {
			entries = append(entries, entry{w.saved.Status.ID, w.saved.Plan, w.saved.Snapshot})
		}
	}
	data, _ := json.Marshal(tools.Result{OK: true, Data: entries, Message: "完整侦察交接；候选风险需独立读取源码验证。论坛记录仍可通过 forum_read 分页读取。"})
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
	out.Audit = tools.AuditState{Ended: stage == phaseAudit && count > 0 && allDone, Summary: strings.Join(summaries, "\n\n"), NextSteps: strings.Join(next, "\n\n")}
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
