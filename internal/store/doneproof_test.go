package store

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Done proofs (docs/decisions/2026-10-18-done-proofs.md).

func item(t *testing.T, s *Store, id int64) pipeline.Item {
	t.Helper()
	d, err := s.GetItem(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return d.Item
}

func exec(t *testing.T, s *Store, q string, args ...any) {
	t.Helper()
	if _, err := s.w.Exec(q, args...); err != nil {
		t.Fatal(q, err)
	}
}

// The done items before this get a status from their history: verified or reopened in qa, else
// unproven; an item not done gets none.
func TestDoneVerifyBackfill(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(t0)
	s, err := Open(dir, clk)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plain := toMerged(t, s, runner, nil, nil)
	checked := toMerged(t, s, runner, nil, map[string]string{"needs_qa": "true"})
	if _, err := s.Verified(ctx, user, checked.ID, "works"); err != nil {
		t.Fatal(err)
	}
	bad := toMerged(t, s, runner, nil, map[string]string{"needs_qa": "true"})
	_, filed, err := s.Reopen(ctx, user, bad.ID, "broken on the phone")
	if err != nil {
		t.Fatal(err)
	}
	open := add(t, s, "not done", "")
	exec(t, s, `UPDATE items SET verify = '', verify_reason = ''`)
	exec(t, s, `DELETE FROM schema_migrations WHERE version = 33`)
	exec(t, s, `DROP INDEX items_verify`)
	exec(t, s, `ALTER TABLE items DROP COLUMN verify`)
	exec(t, s, `ALTER TABLE items DROP COLUMN verify_reason`)
	exec(t, s, `ALTER TABLE items DROP COLUMN verify_asked`)
	s.Close()
	s, err = Open(dir, clk)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, c := range []struct {
		id             int64
		verify, reason string
	}{
		{plain.ID, pipeline.DoneUnproven, ""},
		{checked.ID, pipeline.DoneVerified, "verified in qa"},
		{bad.ID, pipeline.DoneFailed, "reopened in qa as #" + strconv.FormatInt(filed.ID, 10)},
		{open.ID, "", ""},
		{filed.ID, "", ""},
	} {
		if it := item(t, s, c.id); it.Verify != c.verify || it.VerifyReason != c.reason {
			t.Errorf("#%d: %q %q, want %q %q", c.id, it.Verify, it.VerifyReason, c.verify, c.reason)
		}
	}
}

// The sweep: main-red items verified by CI at once; then batches of DoneProofBatch, lowest risk
// first, the next only once the last one's proofs are written and checked (or the wait passed).
func TestDoneSweepBatches(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	var ids []int64
	for range DoneProofBatch + 2 {
		ids = append(ids, toMerged(t, s, runner, nil, nil).ID)
	}
	risky, large := ids[0], ids[1]
	exec(t, s, `UPDATE items SET risk = 'hold' WHERE id = ?`, risky)
	exec(t, s, `UPDATE items SET size = 'L' WHERE id = ?`, large)
	red := toMerged(t, s, runner, nil, nil)
	exec(t, s, `UPDATE items SET source = 'finding', title = 'main red after #3: ci' WHERE id = ?`, red.ID)
	qa := toMerged(t, s, runner, nil, map[string]string{"needs_qa": "true"})

	if err := s.OrchTick(ctx); err != nil {
		t.Fatal(err)
	}
	if it := item(t, s, red.ID); it.Stage != pipeline.Done || it.Verify != pipeline.DoneVerified || it.VerifyReason != pipeline.VerifiedByCI {
		t.Fatalf("main red: %s %s %q", it.Stage, it.Verify, it.VerifyReason)
	}
	if ev := lastEvent(t, s, red.ID, pipeline.KindDoneVerified); ev == nil || ev.Actor != pipeline.ActorProof {
		t.Fatalf("main red event: %+v", ev)
	}
	var asked []int64
	for _, tr := range queuedTriggers(t, s) {
		if tr.Reason == pipeline.TriggerDoneProof {
			asked = append(asked, tr.ItemID)
		}
	}
	if len(asked) != DoneProofBatch || slices.Contains(asked, risky) || slices.Contains(asked, large) ||
		slices.Contains(asked, red.ID) || slices.Contains(asked, qa.ID) {
		t.Fatalf("first batch %v (risky #%d, large #%d, red #%d, qa #%d)", asked, risky, large, red.ID, qa.ID)
	}
	// The orchestrator writes proofs for all but one: a manual one, the rest commands.
	for i, id := range asked[:len(asked)-1] {
		checks := `[{"kind":"command","run":"go test ./x"}]`
		if i == 0 {
			checks = `[{"kind":"manual","look":"the board on a phone"}]`
		}
		got, err := s.OrchProof(ctx, "orchestrator:loop", id, json.RawMessage(checks), "from the PR")
		if err != nil || got.Stage != pipeline.Done {
			t.Fatalf("orch proof #%d: %+v %v", id, got, err)
		}
	}
	if it := item(t, s, asked[0]); it.Verify != pipeline.DoneNeedsYou {
		t.Fatalf("manual proof: %s", it.Verify)
	}
	if _, err := s.OrchProof(ctx, "orchestrator:loop", red.ID, json.RawMessage(`[{"kind":"manual","look":"x"}]`), "again"); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("a proof for a verified item: %v", err)
	}
	// Proving items unchecked and one proof unwritten: no next batch yet.
	clk.Advance(time.Minute)
	if err := s.OrchTick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countReason(queuedTriggers(t, s), pipeline.TriggerDoneProof); n != DoneProofBatch {
		t.Fatalf("asked %d while the batch is open", n)
	}
	// A pass checks at most DoneProofsPerPass done items (and every due qa item).
	due := dueIDs(t, s)
	if len(due) != DoneProofsPerPass {
		t.Fatalf("due %v", due)
	}
	// After the wait, the next batch: the two riskier items; the one without a proof is noted.
	clk.Advance(DoneProofAskWait)
	if err := s.OrchTick(ctx); err != nil {
		t.Fatal(err)
	}
	var next []int64
	for _, tr := range queuedTriggers(t, s) {
		if tr.Reason == pipeline.TriggerDoneProof && !slices.Contains(asked, tr.ItemID) {
			next = append(next, tr.ItemID)
		}
	}
	if !slices.Equal(next, []int64{large, risky}) {
		t.Fatalf("second batch %v, want large #%d then risky #%d", next, large, risky)
	}
	if it := item(t, s, asked[len(asked)-1]); it.Verify != pipeline.DoneUnproven || it.VerifyReason != DoneNoProof {
		t.Fatalf("no proof written: %s %q", it.Verify, it.VerifyReason)
	}
}

func countReason(trs []pipeline.Trigger, reason string) int {
	n := 0
	for _, tr := range trs {
		if tr.Reason == reason {
			n++
		}
	}
	return n
}

// A done item's command failing twice files a P1 item (reopen_of) and leaves the item done and
// failed; passing verifies it, still done.
func TestDoneProofChecked(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	bad := toMerged(t, s, runner, nil, nil)
	good := toMerged(t, s, runner, nil, nil)
	for _, id := range []int64{bad.ID, good.ID} {
		if _, err := s.OrchProof(ctx, "orchestrator:loop", id, json.RawMessage(`[{"kind":"command","run":"go test ./x"}]`), "r"); err != nil {
			t.Fatal(err)
		}
	}
	fail := map[string]any{"results": []any{map[string]any{"check": 0, "state": "failed", "evidence": "exit 1: FAIL TestX"}}}
	pass := map[string]any{"results": []any{map[string]any{"check": 0, "state": "passed", "evidence": "exit 0"}}}
	ev, err := s.ProofChecked(ctx, runner, bad.ID, fail)
	if err != nil || ev.Kind != pipeline.KindDoneProofChecked {
		t.Fatalf("first failure: %+v %v", ev, err)
	}
	if ev, err := s.ProofChecked(ctx, runner, good.ID, pass); err != nil || ev.Kind != pipeline.KindDoneVerified || ev.Actor != pipeline.ActorProof {
		t.Fatalf("pass: %+v %v", ev, err)
	}
	if it := item(t, s, good.ID); it.Stage != pipeline.Done || it.Verify != pipeline.DoneVerified {
		t.Fatalf("good: %s %s", it.Stage, it.Verify)
	}
	clk.Advance(pipeline.ProofRecheck)
	ev, err = s.ProofChecked(ctx, runner, bad.ID, fail)
	if err != nil || ev.Kind != pipeline.KindDoneFailed || ev.Actor != pipeline.ActorProof {
		t.Fatalf("second failure: %+v %v", ev, err)
	}
	it := item(t, s, bad.ID)
	nid, _ := ev.Data["reopen_item"].(int64)
	if f, ok := ev.Data["reopen_item"].(float64); ok {
		nid = int64(f)
	}
	n := item(t, s, nid)
	if it.Stage != pipeline.Done || it.Verify != pipeline.DoneFailed || !strings.HasSuffix(it.VerifyReason, "(filed #"+strconv.FormatInt(n.ID, 10)+")") {
		t.Fatalf("bad: %s %s %q", it.Stage, it.Verify, it.VerifyReason)
	}
	if n.ReopenOf != bad.ID || n.Priority != pipeline.DoneProofReopenPriority || n.Stage != pipeline.Triage || !strings.Contains(n.Ask, "FAIL TestX") {
		t.Fatalf("filed: %+v", n)
	}
	if _, err := s.ProofChecked(ctx, runner, bad.ID, fail); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("a failed item is not checked again: %v", err)
	}

	r, err := s.DoneReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 2 || r.Counts[pipeline.DoneVerified] != 1 || r.Counts[pipeline.DoneFailed] != 1 ||
		len(r.Failed) != 1 || r.Failed[0].Reopen != n.ID || r.Line() != "2 done: 1 verified, 1 failed" {
		t.Fatalf("report %+v %q", r, r.Line())
	}
}

// The user's check of a done item the sweep could not prove: verified, still done.
func TestDoneVerifiedByTheUser(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := toMerged(t, s, runner, nil, nil)
	if _, err := s.OrchProof(ctx, "orchestrator:loop", it.ID, json.RawMessage(`[{"kind":"manual","look":"the colours"}]`), "r"); err != nil {
		t.Fatal(err)
	}
	r, _ := s.DoneReport(ctx)
	if len(r.NeedsYou) != 1 || !slices.Equal(r.NeedsYou[0].Looks, []string{"the colours"}) {
		t.Fatalf("report %+v", r)
	}
	got, err := s.Verified(ctx, user, it.ID, "looks right")
	if err != nil || got.Stage != pipeline.Done || got.Verify != pipeline.DoneVerified {
		t.Fatalf("verified: %+v %v", got, err)
	}
}

// failDone is a done item whose proof failed twice, and the reopen that filed (#415's setup).
func failDone(t *testing.T, s *Store, clk *clock.Fake) (bad, filed pipeline.Item) {
	t.Helper()
	ctx := context.Background()
	bad = toMerged(t, s, runner, nil, nil)
	if _, err := s.OrchProof(ctx, "orchestrator:loop", bad.ID, json.RawMessage(`[{"kind":"command","run":"go test ./x"}]`), "r"); err != nil {
		t.Fatal(err)
	}
	exec(t, s, `UPDATE items SET verify_asked = 1 WHERE id = ?`, bad.ID)
	fail := map[string]any{"results": []any{map[string]any{"check": 0, "state": "failed", "evidence": "exit 127: pytest: not found"}}}
	if _, err := s.ProofChecked(ctx, runner, bad.ID, fail); err != nil {
		t.Fatal(err)
	}
	clk.Advance(pipeline.ProofRecheck)
	ev, err := s.ProofChecked(ctx, runner, bad.ID, fail)
	if err != nil || ev.Kind != pipeline.KindDoneFailed {
		t.Fatalf("second failure: %+v %v", ev, err)
	}
	var nid int64
	if err := s.w.QueryRow(`SELECT MAX(id) FROM items WHERE reopen_of = ?`, bad.ID).Scan(&nid); err != nil {
		t.Fatal(err)
	}
	return item(t, s, bad.ID), item(t, s, nid)
}

func closeNotReproducible(t *testing.T, s *Store, id int64, summary string) {
	t.Helper()
	if c := claim(t, s, "triager"); c == nil || c.Item.ID != id {
		t.Fatalf("claim of #%d: %+v", id, c)
	}
	evidence(t, s, id)
	if _, err := s.Advance(context.Background(), runner, id, pipeline.NotReproducible, map[string]string{"summary": summary}, nil); err != nil {
		t.Fatal(err)
	}
}

func verifyAsked(t *testing.T, s *Store, id int64) int64 {
	t.Helper()
	var n int64
	if err := s.w.QueryRow(`SELECT verify_asked FROM items WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A failed done item whose reopen triage closes not reproducible goes back to unproven, and the
// done sweep asks for a new proof (#415): the failure was the proof's environment, not the change.
func TestReopenNotReproducibleSendsDoneBackToUnproven(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	bad, filed := failDone(t, s, clk)
	closeNotReproducible(t, s, filed.ID, "works here; the proof had no pytest")

	it := item(t, s, bad.ID)
	want := "reopen #" + strconv.FormatInt(filed.ID, 10) + " closed not reproducible: works here; the proof had no pytest"
	if it.Stage != pipeline.Done || it.Verify != pipeline.DoneUnproven || it.VerifyReason != want || it.Proof != nil {
		t.Fatalf("original: %s %s %q %+v", it.Stage, it.Verify, it.VerifyReason, it.Proof)
	}
	if n := verifyAsked(t, s, bad.ID); n != 0 {
		t.Fatalf("verify_asked %d, want 0", n)
	}
	d, err := s.GetItem(ctx, bad.ID)
	if err != nil {
		t.Fatal(err)
	}
	if last := d.Events[len(d.Events)-1]; last.Kind != pipeline.KindDoneReproof || !strings.HasSuffix(last.Actor, runner) {
		t.Fatalf("last event: %+v", last)
	}
	r, err := s.DoneReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Counts[pipeline.DoneFailed] != 0 || r.Counts[pipeline.DoneUnproven] != 1 {
		t.Fatalf("report %+v", r.Counts)
	}
	tx, err := s.w.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.doneSweep(tx, clk.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := verifyAsked(t, s, bad.ID); n == 0 {
		t.Fatal("the sweep did not ask for a new proof")
	}
}

// A reopen closed as a duplicate is a real failure tracked elsewhere: the original stays failed.
// So does one closed while a newer reopen of the same item is still open.
func TestReopenNotReproducibleKeepsRealFailures(t *testing.T) {
	s, clk := open(t)
	bad, filed := failDone(t, s, clk)
	closeNotReproducible(t, s, filed.ID, "duplicate of #3")
	if it := item(t, s, bad.ID); it.Verify != pipeline.DoneFailed {
		t.Fatalf("duplicate: %s %q", it.Verify, it.VerifyReason)
	}

	s, clk = open(t)
	bad, filed = failDone(t, s, clk)
	newer := add(t, s, "newer reopen", "P3")
	exec(t, s, `UPDATE items SET reopen_of = ? WHERE id = ?`, bad.ID, newer.ID)
	closeNotReproducible(t, s, filed.ID, "works")
	if it := item(t, s, bad.ID); it.Verify != pipeline.DoneFailed {
		t.Fatalf("newer reopen open: %s %q", it.Verify, it.VerifyReason)
	}
}

// The user's reopen from qa counts too: closed not reproducible, the item is unproven again.
func TestUserReopenNotReproducible(t *testing.T) {
	s, _ := open(t)
	bad := toMerged(t, s, runner, nil, map[string]string{"needs_qa": "true"})
	_, filed, err := s.Reopen(context.Background(), user, bad.ID, "broken on the phone")
	if err != nil {
		t.Fatal(err)
	}
	closeNotReproducible(t, s, filed.ID, "works on the phone")
	if it := item(t, s, bad.ID); it.Verify != pipeline.DoneUnproven {
		t.Fatalf("original: %s %q", it.Verify, it.VerifyReason)
	}
}

// The items that failed before #415: one whose newest reopen was closed not reproducible goes back
// to unproven; one closed as a duplicate, and one whose reopen is still open, stay failed.
func TestReopenNotReproducibleBackfill(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(t0)
	s, err := Open(dir, clk)
	if err != nil {
		t.Fatal(err)
	}
	gone, goneR := failDone(t, s, clk)
	closeNotReproducible(t, s, goneR.ID, "pytest: not found\nmore")
	dup, dupR := failDone(t, s, clk)
	closeNotReproducible(t, s, dupR.ID, "Duplicate of #3")
	open, _ := failDone(t, s, clk)
	exec(t, s, `UPDATE items SET verify = 'failed', verify_reason = 'x', verify_asked = 5 WHERE id = ?`, gone.ID)
	exec(t, s, `DELETE FROM schema_migrations WHERE version = 38`)
	s.Close()
	s, err = Open(dir, clk)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := "reopen #" + strconv.FormatInt(goneR.ID, 10) + " closed not reproducible: pytest: not found"
	if it := item(t, s, gone.ID); it.Verify != pipeline.DoneUnproven || it.VerifyReason != want || it.Proof != nil || verifyAsked(t, s, gone.ID) != 0 {
		t.Errorf("not reproducible: %s %q %+v", it.Verify, it.VerifyReason, it.Proof)
	}
	for _, id := range []int64{dup.ID, open.ID} {
		if it := item(t, s, id); it.Verify != pipeline.DoneFailed {
			t.Errorf("#%d: %s %q, want failed", id, it.Verify, it.VerifyReason)
		}
	}
}
