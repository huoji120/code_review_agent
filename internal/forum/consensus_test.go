package forum

import (
	"testing"
	"time"
)

func consensusBoard() *Board {
	b := testBoard()
	b.Register("coordinator", "system")
	b.SetConsensusMembers("audit", []string{"a", "b", "c"})
	return b
}

func TestCloseConsensusRequiresUnanimousApproval(t *testing.T) {
	b := consensusBoard()
	invalid := b.RequestClose("a", "audit", "摘要", "后续", "")
	if invalid.Status != "invalid" {
		t.Fatalf("missing vote accepted: %+v", invalid)
	}
	first := b.RequestClose("a", "audit", "摘要", "后续", "approve")
	if first.Status != "pending" || first.Required != 3 || first.Approved != 1 {
		t.Fatalf("first close request = %+v", first)
	}
	second := b.RequestClose("b", "audit", "摘要", "后续", "approve")
	if second.Status != "pending" || second.Approved != 2 {
		t.Fatalf("second close vote = %+v", second)
	}
	rejected := b.RequestClose("c", "audit", "摘要", "后续", "reject")
	if rejected.Status != "rejected" || rejected.Approved != 2 {
		t.Fatalf("rejected close vote = %+v", rejected)
	}
	if _, ok := b.ConsensusApproved("a", "audit"); ok {
		t.Fatal("rejected request was treated as approved")
	}

	fresh := b.RequestClose("a", "audit", "新摘要", "新后续", "approve")
	if fresh.Status != "pending" || fresh.RequestID == first.RequestID || fresh.Approved != 1 {
		t.Fatalf("new request after rejection = %+v", fresh)
	}
	timedOut := b.requestCloseAt("b", "audit", "新摘要", "新后续", "approve", time.Now().Add(closeConsensusTimeout+time.Second))
	if timedOut.Status != "timed_out" || timedOut.Approved != 1 {
		t.Fatalf("timed out request = %+v", timedOut)
	}
}

func TestCloseConsensusCancelsOnlyAfterAllVotes(t *testing.T) {
	b := consensusBoard()
	callback := make(chan string, 1)
	b.SetOnConsensus(func(stage string) { callback <- stage })
	for _, id := range []string{"a", "b"} {
		decision := b.RequestClose(id, "audit", "摘要", "后续", "approve")
		if decision.Status != "pending" {
			t.Fatalf("vote from %s = %+v", id, decision)
		}
	}
	select {
	case stage := <-callback:
		t.Fatalf("consensus callback fired before unanimity for %s", stage)
	default:
	}
	final := b.RequestClose("c", "audit", "摘要", "后续", "approve")
	if final.Status != "approved" || final.Approved != 3 {
		t.Fatalf("final close vote = %+v", final)
	}
	if decision, ok := b.ConsensusApproved("a", "audit"); !ok || decision.Status != "approved" {
		t.Fatalf("approved decision not visible: %+v, %v", decision, ok)
	}
	select {
	case stage := <-callback:
		if stage != "audit" {
			t.Fatalf("callback stage = %q", stage)
		}
	case <-time.After(time.Second):
		t.Fatal("consensus callback did not fire")
	}
	b.ResetConsensus("audit")
	next := b.RequestClose("a", "audit", "新摘要", "新后续", "approve")
	if next.Status != "pending" || next.RequestID == final.RequestID || next.Approved != 1 {
		t.Fatalf("reset did not open a fresh consensus round: %+v", next)
	}
}

func TestCompletedReconAndModeratorDoNotBlockAuditConsensus(t *testing.T) {
	b := testBoard()
	b.Register("coordinator", "system")
	b.Register("recon-1", "recon")
	b.SetStatus("recon-1", "completed")
	b.Register("moderator", "moderator")
	if decision := b.RequestClose("moderator", "moderator", "summary", "", "approve"); decision.Status != "invalid" {
		t.Fatal("moderator became a consensus voter")
	}
	for _, id := range []string{"recon-1", "moderator"} {
		stage := "recon"
		if id == "moderator" {
			stage = "moderator"
		}
		// Neither identity may cast a vote as an audit-stage member.
		if decision := b.RequestClose(id, "audit", "summary", "", "approve"); decision.Status != "invalid" {
			t.Fatalf("%s identity voted in audit: %+v", stage, decision)
		}
	}
	for i, id := range []string{"a", "b", "c"} {
		decision := b.RequestClose(id, "audit", "summary", "", "approve")
		want := "pending"
		if i == 2 {
			want = "approved"
		}
		if decision.Status != want || decision.Required != 3 || decision.Approved != i+1 {
			t.Fatalf("cross-stage identity blocked audit completion: %+v", decision)
		}
	}
}
