package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"code-review-agent/internal/llm"
)

func awaitNaming(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("naming lifecycle did not advance")
	}
}

func TestNamingRunsBesideToolsAndAnnouncesOnceAcrossRestore(t *testing.T) {
	client := &noticeClient{}
	team := newTestTeam(t, client)
	started, release, named := make(chan struct{}), make(chan struct{}), make(chan struct{})
	team.nameClient = &noticeClient{call: func(ctx context.Context, _ []llm.Message) (string, error) {
		close(started)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-release:
			return `{"name":"桃子"}`, nil
		}
	}}
	team.createStageLocked(phaseRecon)
	w := team.workers[0]
	a := w.agent
	a.cfg.Agent.MaxTurns = 4
	initialPrompt := a.collaborationPrompt()
	var before []llm.Message
	step := 0
	client.tools = func(ctx context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		step++
		switch step {
		case 1:
			awaitNaming(t, started)
			return toolReply("read_file", map[string]any{"path": "entry1.go", "limit": 1}), nil
		case 2:
			last := messages[len(messages)-1].Content
			assertNamedWorkerFileRead(t, last)
			if messages[len(messages)-1].Type != "function_call_output" || a.board.Name(a.id) != "" {
				t.Fatal("file tool waited for naming or failed")
			}
			return toolReply("forum_post", map[string]any{"topic": "scope", "content": "unaltered evidence"}), nil
		case 3:
			posts := a.board.Messages()
			if len(posts) != 1 || posts[0].AgentName != "" || posts[0].Content != "unaltered evidence" {
				t.Fatal("unnamed worker could not post while naming was held")
			}
			before = append([]llm.Message(nil), a.messages...)
			close(release)
			awaitNaming(t, named)
			if !reflect.DeepEqual(before, a.messages) || a.collaborationPrompt() != initialPrompt {
				t.Fatal("background naming rewrote audit history or cache prefix")
			}
			posts = a.board.Messages()
			if posts[0].AgentName != "桃子" || posts[0].Content != "unaltered evidence" {
				t.Fatal("name did not backfill retained post")
			}
			return toolReply("read_file", map[string]any{"path": "entry1.go", "limit": 1}), nil
		case 4:
			if a.announcedName != "桃子" {
				t.Fatal("next request missed bounded name announcement")
			}
			return toolReply("forum_roster", map[string]any{}), nil
		}
		return llm.ToolResponse{}, errors.New("unexpected audit turn")
	}
	a.Run(context.Background(), "inspect", func(e Event) {
		if e.Kind == "name" {
			team.mu.Lock()
			activity, status := w.saved.Status.Activity, w.saved.Status.Status
			team.mu.Unlock()
			team.receive(w, e)
			team.mu.Lock()
			unchanged := activity == w.saved.Status.Activity && status == w.saved.Status.Status
			team.mu.Unlock()
			if !unchanged {
				t.Error("name arrival replaced running activity")
			}
			close(named)
			return
		}
		team.receive(w, e)
	})
	if step != 4 || w.saved.Status.Name != "桃子" {
		t.Fatal("worker did not continue through naming")
	}
	path := filepath.Join(t.TempDir(), "named.json")
	if err := team.SaveSession(path); err != nil {
		t.Fatal(err)
	}
	restored := newTestTeam(t, &noticeClient{})
	var calls atomic.Int32
	restored.nameClient = &noticeClient{call: func(context.Context, []llm.Message) (string, error) {
		calls.Add(1)
		return `{"name":"Melon"}`, nil
	}}
	if err := restored.LoadSession(path); err != nil {
		t.Fatal(err)
	}
	a = restored.workers[0].agent
	stop := a.startNaming(context.Background(), func(Event) { t.Error("restored worker renamed") })
	stop()
	before = append([]llm.Message(nil), a.messages...)
	if err := a.prepareRequest(context.Background(), func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || a.board.Name(a.id) != "桃子" || !reflect.DeepEqual(before, a.messages) {
		t.Fatal("resume renamed worker or repeated name announcement")
	}
}

func TestNamingCancellationDrainsWithoutLateBinding(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	started, exited := make(chan struct{}), make(chan struct{})
	team.nameClient = &noticeClient{call: func(ctx context.Context, _ []llm.Message) (string, error) {
		close(started)
		<-ctx.Done()
		defer close(exited)
		// A cancelled transport may still produce a buffered final response.
		return `{"name":"Melon"}`, nil
	}}
	team.createStageLocked(phaseRecon)
	a := team.workers[0].agent
	stop := a.startNaming(context.Background(), func(Event) { t.Error("cancelled name emitted") })
	awaitNaming(t, started)
	stop()
	stop()
	awaitNaming(t, exited)
	if a.board.Name(a.id) != "" {
		t.Fatal("cancelled buffered response bound a name")
	}
	if err := team.SetWorkspace(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestNamingRejectsCollisionAndInvalidOutputWithBoundedRetries(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	if err := team.board.RegisterName("recon-2", "Melon"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	named := make(chan struct{})
	team.nameClient = &noticeClient{call: func(context.Context, []llm.Message) (string, error) {
		switch calls.Add(1) {
		case 1:
			return `{"name":"audit-1"}`, nil
		case 2:
			return `{"name":"mELON"}`, nil
		default:
			return `{"name":"Plum"}`, nil
		}
	}}
	team.createStageLocked(phaseRecon)
	a := team.workers[0].agent
	stop := a.startNaming(context.Background(), func(Event) { close(named) })
	awaitNaming(t, named)
	stop()
	if calls.Load() != 3 || a.board.Name(a.id) != "Plum" || a.board.Name("recon-2") != "Melon" {
		t.Fatal("invalid/duplicate candidate escaped bounded retry")
	}
	// Failed candidates never become an audit-tool call or contaminate history.
	if len(a.messages) != 0 {
		t.Fatal("naming inserted its own model exchange into audit history")
	}
	result := a.board.Call(context.Background(), a.id, a.phase, "forum_roster", json.RawMessage(`{}`))
	if !strings.Contains(result, "Plum") || !strings.Contains(result, "Melon") {
		t.Fatal("roster lost immutable names")
	}
}

func TestNamingFailureNeverInventsFallback(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	var calls atomic.Int32
	attempted := make(chan struct{})
	team.nameClient = &noticeClient{call: func(context.Context, []llm.Message) (string, error) {
		if calls.Add(1) == 3 {
			close(attempted)
		}
		return `{"name":"invalid role 1"}`, nil
	}}
	team.createStageLocked(phaseRecon)
	a := team.workers[0].agent
	stop := a.startNaming(context.Background(), func(Event) { t.Error("invalid name emitted") })
	awaitNaming(t, attempted)
	stop()
	if calls.Load() != 3 || a.board.Name(a.id) != "" {
		t.Fatal("failed naming retried indefinitely or invented fallback")
	}
	result, _ := a.callTool(context.Background(), func(Event) {}, ToolCall{Name: "read_file", Arguments: json.RawMessage(`{"path":"entry1.go","limit":1}`)})
	assertNamedWorkerFileRead(t, result)
}

func assertNamedWorkerFileRead(t *testing.T, text string) {
	t.Helper()
	var result struct {
		OK   bool `json:"ok"`
		Data struct {
			Lines []struct {
				Text string `json:"text"`
			} `json:"lines"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &result); err != nil || !result.OK || len(result.Data.Lines) != 1 || result.Data.Lines[0].Text != "package fixture" {
		t.Fatalf("file tool could not read real source while independently naming: %s", text)
	}
}
