package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"code-review-agent/internal/tools"
)

// FindingRevocation retains the exact evidence withdrawn from the active report.
type FindingRevocation struct {
	Key       string        `json:"finding_key"`
	Reason    string        `json:"reason"`
	Evidence  string        `json:"evidence"`
	RevokedAt string        `json:"revoked_at"`
	Finding   tools.Finding `json:"finding"`
}

func findingKey(f tools.Finding) string {
	f.Key = ""
	data, _ := json.Marshal(f)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (t *Team) Revocations() []FindingRevocation {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]FindingRevocation(nil), t.revocations...)
}

func (t *Team) applyRevocationsLocked(snapshot *tools.Snapshot) {
	if len(t.revocations) == 0 {
		return
	}
	revoked := make(map[string]bool, len(t.revocations))
	for _, r := range t.revocations {
		revoked[r.Key] = true
	}
	kept := snapshot.Findings[:0]
	for _, f := range snapshot.Findings {
		if !revoked[f.Key] {
			kept = append(kept, f)
		}
	}
	clearFindings := snapshot.Findings[len(kept):]
	for i := range clearFindings {
		clearFindings[i] = tools.Finding{}
	}
	snapshot.Findings = kept
}

func (t *Team) moderatorTool(ctx context.Context, call ToolCall) string {
	encode := func(r tools.Result) string { b, _ := json.Marshal(r); return string(b) }
	if err := ctx.Err(); err != nil {
		return encode(tools.Result{Error: err.Error()})
	}
	switch call.Name {
	case "moderator_review_state":
		type findingEvidence struct {
			AgentID    string        `json:"agent_id"`
			FindingKey string        `json:"finding_key"`
			Finding    tools.Finding `json:"finding"`
		}
		t.mu.Lock()
		findings := []findingEvidence{}
		for _, w := range t.workers {
			for _, f := range w.saved.Snapshot.Findings {
				findings = append(findings, findingEvidence{w.saved.Status.ID, findingKey(f), f})
			}
		}
		state := struct {
			Phase       string              `json:"phase"`
			Workers     []WorkerStatus      `json:"workers"`
			Snapshot    tools.Snapshot      `json:"snapshot"`
			Findings    []findingEvidence   `json:"source_findings"`
			Revocations []FindingRevocation `json:"revocations"`
		}{t.phase, t.statusesLocked(), cloneSnapshot(t.snapshot), findings, append([]FindingRevocation(nil), t.revocations...)}
		t.mu.Unlock()
		return encode(tools.Result{OK: true, Data: state})
	case "moderator_revoke_finding":
		var args struct {
			Key      string `json:"finding_key"`
			Reason   string `json:"reason"`
			Evidence string `json:"evidence"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return encode(tools.Result{Error: err.Error()})
		}
		if strings.TrimSpace(args.Key) == "" || strings.TrimSpace(args.Reason) == "" || strings.TrimSpace(args.Evidence) == "" || len(args.Reason) > 4096 || len(args.Evidence) > 16384 {
			return encode(tools.Result{Error: "finding_key, source-backed reason and evidence are required (reason <=4096 bytes; evidence <=16384 bytes)"})
		}
		t.mu.Lock()
		for _, r := range t.revocations {
			if r.Key == args.Key {
				t.mu.Unlock()
				return encode(tools.Result{OK: true, Data: r, Message: "already revoked"})
			}
		}
		var found tools.Finding
		exists := false
		for _, w := range t.workers {
			for _, f := range w.saved.Snapshot.Findings {
				if findingKey(f) == args.Key {
					found = f
					exists = true
					break
				}
			}
			if exists {
				break
			}
		}
		if !exists {
			t.mu.Unlock()
			return encode(tools.Result{Error: "finding missing or evidence changed; reread moderator_review_state"})
		}
		record := FindingRevocation{args.Key, strings.TrimSpace(args.Reason), strings.TrimSpace(args.Evidence), time.Now().UTC().Format(time.RFC3339Nano), found}
		t.revocations = append(t.revocations, record)
		t.snapshot = t.aggregateLocked()
		t.mu.Unlock()
		t.emitState()
		_, _ = t.board.Post("moderator", "moderator", "*", 0, "漏洞撤销："+found.Title, "@全体成员\n撤销理由："+record.Reason+"\n复核证据："+record.Evidence)
		return encode(tools.Result{OK: true, Data: record, Message: "withdrawn from active report; original finding and review evidence retained"})
	default:
		return encode(tools.Result{Error: "unknown moderator tool"})
	}
}
