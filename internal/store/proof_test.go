package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// QA proofs (docs/decisions/2026-10-18-qa-proofs.md).

func proofQA(t *testing.T, s *Store, proof string) pipeline.Item {
	t.Helper()
	it := toMergedBuild(t, s, runner, nil, map[string]string{"proof": proof}, map[string]string{"needs_qa": "true"})
	d, err := s.GetItem(context.Background(), it.ID)
	if err != nil || d.Item.Stage != pipeline.QA || d.Item.Proof == nil {
		t.Fatalf("not in qa with a proof: %+v %v", d.Item, err)
	}
	return d.Item
}

func dueIDs(t *testing.T, s *Store) []int64 {
	t.Helper()
	rows, err := s.ProofsDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// A command and a usage check: the usage counts only events after the merge; with both passed
// the loop verifies the item.
func TestProofVerifiedByTheLoop(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	// The builder's own claims (before the merge) do not count as use.
	it := proofQA(t, s, `[{"kind":"usage","event":"claimed","actor":["runner:mini"]},{"kind":"command","run":"scripts/check"}]`)
	if got := dueIDs(t, s); len(got) != 1 || got[0] != it.ID {
		t.Fatalf("due = %v", got)
	}
	ok := map[string]any{"results": []any{map[string]any{"check": 1, "state": "passed", "evidence": "exit 0"}}}
	ev, err := s.ProofChecked(ctx, runner, it.ID, ok)
	if err != nil || ev.Kind != KindProofChecked || ev.Actor != "runner:"+runner {
		t.Fatalf("first check: %+v %v", ev, err)
	}
	d, _ := s.GetItem(ctx, it.ID)
	if d.Item.Stage != pipeline.QA || d.Item.Proof.Checks[0].State != pipeline.CheckPending || d.Item.Proof.Checks[1].State != pipeline.CheckPassed {
		t.Fatalf("after the first check: %+v", d.Item.Proof)
	}
	// Checked: not due again for 30 minutes, and a second report is wrong_stage.
	if got := dueIDs(t, s); len(got) != 0 {
		t.Fatalf("due right after a check: %v", got)
	}
	if _, err := s.ProofChecked(ctx, runner, it.ID, ok); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("a check before it is due: %v", err)
	}
	// The mini runner claims another item: the usage the check waits for.
	other := add(t, s, "other work", "")
	if c, err := s.Claim(ctx, "mini", pipeline.ClaimRequest{Runner: "mini", Agent: "a"}); err != nil || c == nil || c.Item.ID != other.ID {
		t.Fatalf("mini's claim: %+v %v", c, err)
	}
	clk.Advance(pipeline.ProofRecheck)
	ev, err = s.ProofChecked(ctx, runner, it.ID, map[string]any{})
	if err != nil || ev.Kind != "verified" || ev.Actor != pipeline.ActorProof || ev.To != "done" {
		t.Fatalf("second check: %+v %v", ev, err)
	}
	if w, _ := ev.Data["words"].(string); !strings.HasPrefix(w, "Proof passed: usage: `claimed` by runner:mini (passed); command: `scripts/check` (passed)") {
		t.Fatalf("words %q", w)
	}
	d, _ = s.GetItem(ctx, it.ID)
	if d.Item.Stage != pipeline.Done || !strings.Contains(d.Item.Proof.Checks[0].Evidence, "seen 1 time") {
		t.Fatalf("after: %s %+v", d.Item.Stage, d.Item.Proof.Checks[0])
	}
}

// A command check failing twice in a row reopens the item as the user's Reopen does: a P2 item
// with the failure as its ask.
func TestProofFailedTwiceReopens(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	it := proofQA(t, s, `[{"kind":"command","run":"curl -fsS localhost:7781/healthz"}]`)
	bad := map[string]any{"results": []any{map[string]any{"check": 0, "state": "failed", "evidence": "exit 7: connection refused"}}}
	ev, err := s.ProofChecked(ctx, runner, it.ID, bad)
	if err != nil || ev.Kind != KindProofChecked || ev.Data["status"] != pipeline.CheckFailed || ev.Data["changed"] != true {
		t.Fatalf("first failure: %+v %v", ev, err)
	}
	clk.Advance(pipeline.ProofRecheck)
	ev, err = s.ProofChecked(ctx, runner, it.ID, bad)
	if err != nil || ev.Kind != "reopened" || ev.Actor != pipeline.ActorProof {
		t.Fatalf("second failure: %+v %v", ev, err)
	}
	re := lastEvent(t, s, it.ID, "reopened")
	if re == nil || re.Actor != pipeline.ActorProof || re.Data["reopen_item"] == nil {
		t.Fatalf("reopened event: %+v", re)
	}
	n := int64(re.Data["reopen_item"].(float64))
	nd, err := s.GetItem(ctx, n)
	if err != nil || nd.Item.ReopenOf != it.ID || nd.Item.Priority != pipeline.ReopenPriority ||
		!strings.Contains(nd.Item.Ask, "exit 7: connection refused") {
		t.Fatalf("filed: %+v %v", nd.Item, err)
	}
}

// A proof with only manual checks, or waiting ones, is never due: the item stays for the user.
func TestProofManualAndWaitingStay(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := proofQA(t, s, `[{"kind":"manual","look":"the QA column on a phone"},{"kind":"e2e","tests":["TestQA"]}]`)
	if got := dueIDs(t, s); len(got) != 0 {
		t.Fatalf("due = %v", got)
	}
	if _, err := s.ProofChecked(ctx, runner, it.ID, nil); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("check of a manual proof: %v", err)
	}
	// The user can always verify by hand.
	if got, err := s.Verified(ctx, user, it.ID, "looks right"); err != nil || got.Stage != pipeline.Done {
		t.Fatalf("verified by hand: %+v %v", got, err)
	}
}

// Data other than results is refused, as is a result for a usage check.
func TestProofCheckedRefusals(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := proofQA(t, s, `[{"kind":"usage","event":"rebuild"},{"kind":"command","run":"true"}]`)
	if _, err := s.ProofChecked(ctx, runner, it.ID, map[string]any{"verdict": "passed"}); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("unknown data: %v", err)
	}
	if _, err := s.ProofChecked(ctx, runner, it.ID, map[string]any{"results": []any{map[string]any{"check": 0, "state": "passed"}}}); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("a runner result for a usage check: %v", err)
	}
	if _, err := s.ProofChecked(ctx, runner, it.ID, map[string]any{"results": "all good"}); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("results not a list: %v", err)
	}
}

// A proof is checked only once its deploy is reported (the loop's own repo).
func TestProofWaitsForTheDeploy(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := toMergedBuild(t, s, runner, nil, map[string]string{"proof": `[{"kind":"usage","event":"rebuild","match":{"source":["needs_you"]}}]`},
		map[string]string{"qa_after_deploy": "true"})
	if got := dueIDs(t, s); len(got) != 0 {
		t.Fatalf("due before the deploy: %v", got)
	}
	// A matching event before the deploy does not count.
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		return s.event(tx, Event{ItemID: it.ID, Actor: user, Kind: "rebuild", Data: map[string]any{"source": "needs_you"}})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Deployed(ctx, runner, it.ID, map[string]any{"revision": strings.Repeat("c", 40)}); err != nil {
		t.Fatal(err)
	}
	if got := dueIDs(t, s); len(got) != 1 {
		t.Fatalf("due after the deploy: %v", got)
	}
	ev, err := s.ProofChecked(ctx, runner, it.ID, nil)
	if err != nil || ev.Kind != KindProofChecked || ev.Data["status"] != pipeline.CheckPending {
		t.Fatalf("check: %+v %v", ev, err)
	}
}

// The sweep asks the orchestrator once per qa item without a proof; its proof action sets one.
func TestProofSweep(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	bare := toMerged(t, s, runner, nil, map[string]string{"needs_qa": "true"})
	proved := proofQA(t, s, `[{"kind":"manual","look":"x"}]`)
	if err := s.OrchTick(ctx); err != nil {
		t.Fatal(err)
	}
	q := queuedTriggers(t, s)
	if len(q) != 1 || q[0].Reason != pipeline.TriggerProof || q[0].ItemID != bare.ID {
		t.Fatalf("queued %+v (proved #%d has its own)", q, proved.ID)
	}
	clk.Advance(time.Minute)
	if err := s.OrchTick(ctx); err != nil {
		t.Fatal(err)
	}
	if q := queuedTriggers(t, s); len(q) != 1 {
		t.Fatalf("asked twice: %+v", q)
	}
	// A turn that ends without a proof does not ask again: no turn every tick, forever.
	cl := claimKinds(t, s, "o", pipeline.WorkOrchestrate)
	if cl == nil || cl.Orch == nil {
		t.Fatalf("the sweep's turn: %+v", cl)
	}
	finish(t, s, cl, "sess-1")
	clk.Advance(time.Minute)
	if err := s.OrchTick(ctx); err != nil {
		t.Fatal(err)
	}
	if q := queuedTriggers(t, s); len(q) != 0 {
		t.Fatalf("asked again after a turn without a proof: %+v", q)
	}
	got, err := s.OrchProof(ctx, "orchestrator:loop", bare.ID, json.RawMessage(`[{"kind":"usage","event":"verified"}]`), "from the PR's test plan")
	if err != nil || got.Proof == nil || got.Proof.By != pipeline.ProofBySweep {
		t.Fatalf("orch proof: %+v %v", got.Proof, err)
	}
	if ev := lastEvent(t, s, bare.ID, "proof_set"); ev == nil || ev.Actor != "orchestrator:loop" || ev.Data["reason"] != "from the PR's test plan" {
		t.Fatalf("proof_set event: %+v", ev)
	}
	if _, err := s.OrchProof(ctx, "orchestrator:loop", proved.ID, json.RawMessage(`[{"kind":"manual","look":"y"}]`), "again"); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("a second proof: %v", err)
	}
	if _, err := s.OrchProof(ctx, "orchestrator:loop", bare.ID, json.RawMessage(`[{"kind":"what"}]`), "bad"); code(err) == "" {
		t.Fatalf("a bad proof: %v", err)
	}
}

// recordEvent writes an event as the loop would, for usage checks to see.
func recordEvent(t *testing.T, s *Store, e Event) {
	t.Helper()
	if err := s.tx(context.Background(), func(tx *sql.Tx) error { return s.event(tx, e) }); err != nil {
		t.Fatal(err)
	}
}

// checkProof is a runner's check of a proof with only usage checks; it returns the event kind and
// the proof's status.
func checkProof(t *testing.T, s *Store, clk interface{ Advance(time.Duration) }, id int64) Event {
	t.Helper()
	clk.Advance(pipeline.ProofRecheck)
	ev, err := s.ProofChecked(context.Background(), runner, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// A usage check with actors counts only their events: a claim by another runner is not use.
func TestProofUsageActors(t *testing.T) {
	s, clk := open(t)
	it := proofQA(t, s, `[{"kind":"usage","event":"claimed","actor":["runner:mbp","runner:mini"]}]`)
	other := add(t, s, "other work", "")
	recordEvent(t, s, Event{ItemID: other.ID, Actor: "runner:other", Kind: "claimed"})
	if ev := checkProof(t, s, clk, it.ID); ev.Kind != KindProofChecked || ev.Data["status"] != pipeline.CheckPending {
		t.Fatalf("a claim by runner:other: %+v", ev)
	}
	recordEvent(t, s, Event{ItemID: other.ID, Actor: "runner:mbp", Kind: "claimed"})
	if ev := checkProof(t, s, clk, it.ID); ev.Kind != "verified" {
		t.Fatalf("a claim by runner:mbp: %+v", ev)
	}
}

// A usage check's match compares values: a missing key or another value is not use; numbers and
// bools compare as their text.
func TestProofUsageMatch(t *testing.T) {
	s, clk := open(t)
	it := proofQA(t, s, `[{"kind":"usage","event":"rebuild","match":{"source":["needs_you"],"forced":["true"],"n":["2"]}}]`)
	other := add(t, s, "other work", "")
	for _, d := range []map[string]any{
		{"source": "other", "forced": true, "n": 2},
		{"forced": true, "n": 2},
		{"source": "needs_you", "forced": false, "n": 2},
		{"source": "needs_you", "forced": true, "n": 3},
	} {
		recordEvent(t, s, Event{ItemID: other.ID, Actor: user, Kind: "rebuild", Data: d})
		if ev := checkProof(t, s, clk, it.ID); ev.Kind != KindProofChecked || ev.Data["status"] != pipeline.CheckPending {
			t.Fatalf("rebuild %v: %+v", d, ev)
		}
	}
	recordEvent(t, s, Event{ItemID: other.ID, Actor: user, Kind: "rebuild", Data: map[string]any{"source": "needs_you", "forced": true, "n": 2}})
	if ev := checkProof(t, s, clk, it.ID); ev.Kind != "verified" {
		t.Fatalf("the matching rebuild: %+v", ev)
	}
}

// A proof cannot pass on its own records: the item's own proof_checked and proof_set events are
// not use; another item's are.
func TestProofUsageNotItsOwnChecks(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	own := proofQA(t, s, `[{"kind":"usage","event":"proof_checked"}]`)
	for i := 0; i < 2; i++ {
		if ev := checkProof(t, s, clk, own.ID); ev.Kind != KindProofChecked || ev.Data["status"] != pipeline.CheckPending {
			t.Fatalf("check %d on its own proof_checked: %+v", i+1, ev)
		}
	}
	// A sweep's proof is set after the merge: its own proof_set is not use either.
	swept := toMerged(t, s, runner, nil, map[string]string{"needs_qa": "true"})
	if _, err := s.OrchProof(ctx, "orchestrator:loop", swept.ID, json.RawMessage(`[{"kind":"usage","event":"proof_set"}]`), "sweep"); err != nil {
		t.Fatal(err)
	}
	if ev := checkProof(t, s, clk, swept.ID); ev.Kind != KindProofChecked || ev.Data["status"] != pipeline.CheckPending {
		t.Fatalf("a check on its own proof_set: %+v", ev)
	}
	other := add(t, s, "other work", "")
	recordEvent(t, s, Event{ItemID: other.ID, Actor: "runner:" + runner, Kind: KindProofChecked})
	recordEvent(t, s, Event{ItemID: other.ID, Actor: "orchestrator:loop", Kind: "proof_set"})
	for _, id := range []int64{own.ID, swept.ID} {
		if ev := checkProof(t, s, clk, id); ev.Kind != "verified" {
			t.Fatalf("#%d on another item's proof records: %+v", id, ev)
		}
	}
}

func queuedTriggers(t *testing.T, s *Store) []pipeline.Trigger {
	t.Helper()
	q, err := orchTurnWith(s.r, OrchQueued)
	if err != nil {
		t.Fatal(err)
	}
	if q == nil {
		return nil
	}
	return q.Triggers
}
