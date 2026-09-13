package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"code-review-agent/internal/forum"
	"code-review-agent/internal/llm"
	"code-review-agent/internal/tools"
)

type workerSession struct {
	Status               WorkerStatus       `json:"status"`
	Assignment           string             `json:"assignment"`
	Messages             []llm.Message      `json:"messages"`
	Skills               []string           `json:"skills,omitempty"`
	TracePath            string             `json:"trace_path,omitempty"`
	Snapshot             tools.Snapshot     `json:"snapshot"`
	Plan                 *auditPlanDoneArgs `json:"plan,omitempty"`
	ModeratorSuggestions []string           `json:"moderator_suggestions,omitempty"`
	Completed            bool               `json:"completed"`
	ForumCursor          int64              `json:"forum_cursor,omitempty"`
	ForumPending         string             `json:"forum_pending,omitempty"`
	AnnouncedName        string             `json:"announced_name,omitempty"`
}

type teamSession struct {
	Version            int                        `json:"version"`
	SavedAt            string                     `json:"saved_at"`
	Workspace          string                     `json:"workspace"`
	Phase              string                     `json:"phase"`
	Input              string                     `json:"input"`
	ReconAgents        int                        `json:"recon_agents"`
	AuditAgents        int                        `json:"audit_agents"`
	Workers            []workerSession            `json:"workers"`
	Forum              []forum.Message            `json:"forum"`
	ForumNames         map[string]string          `json:"forum_names,omitempty"`
	ForumParticipants  map[int64]map[string]int64 `json:"forum_participants,omitempty"`
	Moderator          *workerSession             `json:"moderator,omitempty"`
	Revocations        []FindingRevocation        `json:"revocations,omitempty"`
	PendingAssignments map[string][]string        `json:"pending_assignments,omitempty"`
	IRC                []IRCMessage               `json:"irc,omitempty"`
	IRCNextID          int64                      `json:"irc_next_id,omitempty"`
}

// Saves immutable worker boundaries, including cursor+pending notification pairs.
func (t *Team) SaveSession(path string) error {
	t.saveMu.Lock()
	defer t.saveMu.Unlock()
	t.ircMu.Lock()
	t.mu.Lock()
	s := teamSession{Version: 2, SavedAt: time.Now().Format(time.RFC3339), Workspace: t.registry.Workspace(), Phase: t.phase, Input: t.input, ReconAgents: t.cfg.Agent.ReconAgents, AuditAgents: t.cfg.Agent.AuditAgents}
	for _, w := range t.workers {
		s.Workers = append(s.Workers, w.saved)
	}
	if t.moderator != nil {
		saved := t.moderator.saved
		s.Moderator = &saved
	}
	s.Revocations = append([]FindingRevocation(nil), t.revocations...)
	s.PendingAssignments = make(map[string][]string, len(t.pendingAssignments))
	for id, assignments := range t.pendingAssignments {
		s.PendingAssignments[id] = append([]string(nil), assignments...)
	}
	s.IRC = append([]IRCMessage(nil), t.ircMessages...)
	s.IRCNextID = t.ircNextID
	s.Forum, s.ForumNames, s.ForumParticipants = t.board.Checkpoint()
	for i := range s.Workers {
		s.Workers[i].Status.Name = s.ForumNames[s.Workers[i].Status.ID]
	}
	t.mu.Unlock()
	t.ircMu.Unlock()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".audit-session-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (t *Team) LoadSession(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var s teamSession
	if err = json.Unmarshal(data, &s); err != nil {
		return err
	}
	legacy := s.Version == 0
	if legacy {
		// Data migration only: never clone an old model transcript into teammates.
		var old struct {
			Workspace string         `json:"workspace"`
			Snapshot  tools.Snapshot `json:"snapshot"`
		}
		if err = json.Unmarshal(data, &old); err != nil {
			return err
		}
		if old.Workspace == "" {
			return fmt.Errorf("不是有效的审计会话")
		}
		var files []string
		for _, f := range old.Snapshot.Files {
			files = append(files, f.Path)
		}
		s = teamSession{Version: 2, Workspace: old.Workspace, Phase: phaseAudit, Input: "恢复历史审计，独立复核遗留结论并继续未完成范围。", ReconAgents: 4, AuditAgents: 4, Workers: []workerSession{{Status: WorkerStatus{ID: "legacy", Phase: phaseRecon, Status: "completed"}, Snapshot: old.Snapshot, Completed: true, Plan: &auditPlanDoneArgs{Summary: "旧版审计会话迁移", AuditMap: old.Snapshot.Project.Note, AuditFiles: files}}}}
	}
	if s.Version != 2 {
		return fmt.Errorf("不支持的会话版本 %d", s.Version)
	}
	if s.Phase != phaseRecon && s.Phase != phaseAudit && s.Phase != "completed" {
		return fmt.Errorf("无效的会话阶段 %q", s.Phase)
	}
	if s.ReconAgents < 1 || s.ReconAgents > 32 || s.AuditAgents < 1 || s.AuditAgents > 32 {
		return fmt.Errorf("无效的会话团队规模")
	}
	for _, revocation := range s.Revocations {
		if revocation.Key == "" || revocation.Reason == "" || revocation.Evidence == "" {
			return fmt.Errorf("invalid finding revocation")
		}
	}
	if s.Moderator != nil && (s.Moderator.Status.ID != phaseModerator || s.Moderator.Status.Phase != phaseModerator || s.Moderator.Status.Name != "论坛管理员") {
		return fmt.Errorf("invalid moderator identity")
	}
	for id, assignments := range s.PendingAssignments {
		valid := false
		for _, worker := range s.Workers {
			if worker.Status.ID == id && worker.Status.Phase == s.Phase {
				valid = true
				break
			}
		}
		if !valid || len(assignments) > 8 {
			return fmt.Errorf("invalid pending moderator assignments")
		}
		for _, content := range assignments {
			if len(content) == 0 || len(content) > 4096 {
				return fmt.Errorf("invalid moderator assignment content")
			}
		}
	}
	seen := map[string]bool{}
	for _, w := range s.Workers {
		if w.Status.ID == "" || seen[w.Status.ID] || (w.Status.Phase != phaseRecon && w.Status.Phase != phaseAudit) {
			return fmt.Errorf("无效或重复的 worker 身份")
		}
		seen[w.Status.ID] = true
		if w.Status.Phase == phaseRecon && w.Completed && w.Plan == nil {
			return fmt.Errorf("已完成侦察缺少交接资料")
		}
		if w.Status.Phase == phaseAudit && w.Completed && !w.Snapshot.Audit.Ended {
			return fmt.Errorf("已完成审计缺少 end_audit 状态")
		}
	}
	if len(s.Workers) > 64 {
		return fmt.Errorf("会话 worker 过多")
	}
	if err := validateIRCSession(s); err != nil {
		return err
	}
	newRegistry, err := tools.NewRegistry(s.Workspace, t.cfg.Agent.MaxToolResultChars)
	if err != nil {
		return fmt.Errorf("恢复工作区: %w", err)
	}
	checkBoard := forum.New(time.Second)
	if err = checkBoard.Restore(s.Forum); err != nil {
		newRegistry.Close()
		return err
	}
	checkBoard.Register("user", "user")
	checkBoard.Register("coordinator", "system")
	checkBoard.Register(phaseModerator, phaseModerator)
	for _, stage := range []struct {
		name  string
		count int
	}{{phaseRecon, s.ReconAgents}, {phaseAudit, s.AuditAgents}} {
		for i := 1; i <= stage.count; i++ {
			checkBoard.Register(fmt.Sprintf("%s-%d", stage.name, i), stage.name)
		}
	}
	if s.ForumNames == nil {
		s.ForumNames = make(map[string]string)
	}
	allSaved := append([]workerSession(nil), s.Workers...)
	if s.Moderator != nil {
		allSaved = append(allSaved, *s.Moderator)
	}
	for _, saved := range allSaved {
		if err := validateNativeHistory(saved.Messages); err != nil {
			newRegistry.Close()
			return fmt.Errorf("invalid native history for %s: %w", saved.Status.ID, err)
		}
		checkBoard.Register(saved.Status.ID, saved.Status.Phase)
		if saved.Status.Name != "" {
			if name := s.ForumNames[saved.Status.ID]; name != "" && name != saved.Status.Name {
				newRegistry.Close()
				return fmt.Errorf("worker name conflicts with forum identity")
			}
			s.ForumNames[saved.Status.ID] = saved.Status.Name
		}
		latest := int64(0)
		if len(s.Forum) > 0 {
			latest = s.Forum[len(s.Forum)-1].ID
		}
		if saved.ForumCursor < 0 || saved.ForumCursor > latest {
			newRegistry.Close()
			return fmt.Errorf("invalid worker forum cursor")
		}
		if saved.ForumPending != "" {
			found := false
			for _, message := range saved.Messages {
				if message.Role == llm.RoleUser && message.Content == saved.ForumPending {
					found = true
					break
				}
			}
			if !found {
				newRegistry.Close()
				return fmt.Errorf("pending forum notification missing from worker history")
			}
		}
	}
	if err = checkBoard.RestoreNames(s.ForumNames); err != nil {
		newRegistry.Close()
		return err
	}
	if err = checkBoard.RestoreParticipants(s.ForumParticipants); err != nil {
		newRegistry.Close()
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running {
		newRegistry.Close()
		return fmt.Errorf("请等待当前审计完全停止再恢复会话")
	}
	for _, w := range t.workers {
		_ = w.agent.tools.Close()
	}
	if t.moderator != nil {
		_ = t.moderator.agent.tools.Close()
	}
	_ = t.registry.Close()
	t.registry = newRegistry
	t.cfg.Agent.ReconAgents = s.ReconAgents
	t.cfg.Agent.AuditAgents = s.AuditAgents
	t.phase, t.input, t.handoff = s.Phase, s.Input, ""
	t.workers = nil
	t.moderator = nil
	t.revocations = append([]FindingRevocation(nil), s.Revocations...)
	t.pendingAssignments = s.PendingAssignments
	t.ircMessages = append([]IRCMessage(nil), s.IRC...)
	t.ircNextID = s.IRCNextID
	t.ircChanged = make(chan struct{})
	t.resetBoard()
	_ = t.board.Restore(s.Forum)
	for _, saved := range allSaved {
		t.board.Register(saved.Status.ID, saved.Status.Phase)
	}
	_ = t.board.RestoreNames(s.ForumNames)
	_ = t.board.RestoreParticipants(s.ForumParticipants)
	for _, saved := range allSaved {
		a := newWorker(t.cfg, t.prompts, t.client, t.compressClient, t.registry.Fork(), saved.Status.ID, saved.Status.Phase, t.board)
		a.nameClient = t.nameClient
		a.onDisconnect = t.modelDisconnected
		a.announcedName = saved.AnnouncedName
		count := s.ReconAgents
		if saved.Status.Phase == phaseAudit {
			count = s.AuditAgents
		}
		a.assignment = stageAssignment(saved.Status.Phase, count)
		saved.Assignment = a.assignment
		a.turn = saved.Status.Turn
		a.completed = saved.Completed
		a.plan = saved.Plan
		a.forumCursor, a.forumPending = saved.ForumCursor, saved.ForumPending
		saved.Status.Name = t.board.Name(saved.Status.ID)
		a.tracePath = saved.TracePath
		a.prompts.SetLoadedSkills(saved.Skills)
		a.tools.RestoreSnapshot(saved.Snapshot)
		a.messages = append([]llm.Message(nil), saved.Messages...)
		a.sanitizeMessages()
		// Buffer IDs are process-local; restore exposes a fresh bounded state and
		// explicitly requires re-reading prior tool references.
		a.messages = append(a.messages, llm.Message{Role: llm.RoleUser, Content: "会话已恢复。旧工具 buffer 已过期；不要沿用旧 buffer_id。需要时重新调用原工具、read_handoff 或 review_state。"})
		if !saved.Completed {
			saved.Status.Status = "pending"
			saved.Status.Activity = "已恢复，等待 go"
		}
		w := &teamWorker{agent: a, saved: saved}
		a.checkpoint = func(a *Agent) { t.capture(w, a) }
		t.bindIRC(w)
		t.workers = append(t.workers, w)
		if saved.Status.Phase == phaseModerator {
			a.nameClient = nil
			a.assignment = ""
			a.completed = false
			a.moderateTool = t.moderatorTool
			w.saved.Completed = false
			w.saved.Status.Status = "idle"
			w.saved.Status.Activity = "已恢复，等待 go"
			t.moderator = w
			t.workers = t.workers[:len(t.workers)-1]
		}
		t.board.Register(a.id, a.phase)
		t.board.SetStatus(a.id, saved.Status.Status)
	}
	t.handoff = t.buildHandoffLocked()
	if legacy {
		t.createStageLocked(phaseAudit)
		for _, w := range t.workers {
			if w.agent.id == "audit-1" {
				prior := s.Workers[0].Snapshot
				prior.Audit = tools.AuditState{}
				w.agent.tools.RestoreSnapshot(prior)
				w.saved.Snapshot = cloneSnapshot(prior)
			}
		}
	}
	for _, w := range t.workers {
		if w.agent.phase == phaseAudit {
			w.agent.handoff = t.handoff
		}
	}
	t.snapshot = t.aggregateLocked()
	return nil
}
