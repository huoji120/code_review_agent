package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-review-agent/internal/llm"
)

func TestPresetNamesImmediateAndStableAcrossRestore(t *testing.T) {
	client := &noticeClient{call: func(context.Context, []llm.Message) (string, error) {
		t.Fatal("naming made a model request")
		return "", nil
	}}
	team := newTestTeam(t, client)
	// Existing historical identities reserve their original names before allocation.
	if err := team.board.RegisterName("recon-1", "旧名字"); err != nil {
		t.Fatal(err)
	}
	team.createStageLocked(phaseRecon)
	team.createStageLocked(phaseAudit)
	names := map[string]string{}
	for _, s := range team.Statuses() {
		if s.Name == "" {
			t.Fatal("worker created without a name")
		}
		names[s.ID] = s.Name
	}
	if names["recon-1"] != "旧名字" {
		t.Fatal("historical name changed")
	}
	a := team.workers[1].agent
	a.sanitizeMessages()
	if err := a.prepareRequest(context.Background(), func(Event) {}); err != nil {
		t.Fatal(err)
	}
	count := len(a.messages)
	if a.announcedName != names[a.id] {
		t.Fatal("fixed name not delivered to worker")
	}
	if err := a.prepareRequest(context.Background(), func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(a.messages) != count {
		t.Fatal("name announcement repeated")
	}
	team.capture(team.workers[1], a)
	path := filepath.Join(t.TempDir(), "names.json")
	if err := team.SaveSession(path); err != nil {
		t.Fatal(err)
	}
	restored := newTestTeam(t, client)
	if err := restored.LoadSession(path); err != nil {
		t.Fatal(err)
	}
	for _, s := range restored.Statuses() {
		if names[s.ID] != s.Name {
			t.Fatalf("restored name changed: %+v", s)
		}
	}
	// Child verification uses the same local allocator and cannot remain unnamed.
	child := newWorker(team.cfg, team.prompts, client, client, team.registry.Fork(), "audit-1-verify-1", phaseAudit, team.board)
	defer child.tools.Close()
	if name := team.board.Name(child.id); name == "" || strings.Contains(name, "audit-") {
		t.Fatalf("bad verifier name %q", name)
	}
}

func TestRestoreFillsUnnamedWorkersWithoutRenamingExisting(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.createStageLocked(phaseRecon)
	path := filepath.Join(t.TempDir(), "legacy.json")
	if err := team.SaveSession(path); err != nil {
		t.Fatal(err)
	}
	// Model legacy snapshots via their real session representation.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s teamSession
	if err = json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	delete(s.ForumNames, "recon-2")
	s.Workers[1].Status.Name = ""
	data, err = json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	restored := newTestTeam(t, &noticeClient{})
	if err := restored.LoadSession(path); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, w := range restored.Statuses() {
		if w.Name == "" || seen[w.Name] {
			t.Fatalf("missing/duplicate name: %+v", w)
		}
		seen[w.Name] = true
		if old := s.ForumNames[w.ID]; old != "" && old != w.Name {
			t.Fatal("existing name changed")
		}
	}
}
