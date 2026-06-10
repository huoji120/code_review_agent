package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"code-review-agent/internal/llm"
	"code-review-agent/internal/tools"
)

type Session struct {
	SavedAt   string         `json:"saved_at"`
	Workspace string         `json:"workspace"`
	Phase     string         `json:"phase,omitempty"`
	Skills    []string       `json:"skills,omitempty"`
	TracePath string         `json:"trace_path,omitempty"`
	Messages  []llm.Message  `json:"messages"`
	Snapshot  tools.Snapshot `json:"snapshot"`
}

func (a *Agent) SaveSession(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	session := Session{
		SavedAt:   time.Now().Format(time.RFC3339),
		Workspace: a.tools.Workspace(),
		Phase:     a.phase,
		Skills:    a.prompts.LoadedSkillNames(),
		TracePath: a.TracePath(),
		Messages:  a.messages,
		Snapshot:  a.tools.Snapshot(),
	}
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func (a *Agent) LoadSession(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return err
	}
	if session.Workspace != "" {
		if err := a.tools.SetWorkspace(session.Workspace); err != nil {
			return fmt.Errorf("restore workspace %q: %w", session.Workspace, err)
		}
	}
	if session.Phase == "" {
		session.Phase = phaseExecute
	}
	a.phase = session.Phase
	a.prompts.SetLoadedSkills(session.Skills)
	a.messages = append([]llm.Message(nil), session.Messages...)
	a.sanitizeMessages()
	a.tools.RestoreSnapshot(session.Snapshot)
	a.pendingEndAudit = false
	a.trace = nil
	a.tracePath = session.TracePath
	a.traceErr = nil
	a.traceBootstrapped = false
	return nil
}
