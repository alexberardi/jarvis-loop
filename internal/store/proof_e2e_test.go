package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// E2E checks decided by a regression run (#195, docs/decisions/2026-10-18-qa-proofs.md).

func regDoneData(t *testing.T, s *Store) map[string]any {
	t.Helper()
	var data string
	if err := s.r.QueryRow(`SELECT data FROM events WHERE kind = ? ORDER BY id DESC LIMIT 1`, KindRegDone).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// A web item's e2e check names the spec as coverage.json does; the run's report names it relative to
// the suite: it matches, the spec passed, and the loop verifies the item with the report's link.
func TestE2EProofVerifiedByARegression(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	it := proofQA(t, s, `[{"kind":"e2e","tests":["e2e/web/tests/inbox.spec.ts"]}]`)
	clk.Advance(time.Minute)
	r := queueReg(t, s, "web")
	claimReg(t, s)
	reportRun(t, s, r.ID)

	d, _ := s.GetItem(ctx, it.ID)
	if d.Item.Stage != pipeline.Done || d.Item.Proof.Checks[0].State != pipeline.CheckPassed ||
		!strings.Contains(d.Item.Proof.Checks[0].Evidence, "/board/regressions/1") {
		t.Fatalf("after the run: %s %+v", d.Item.Stage, d.Item.Proof.Checks[0])
	}
	ev := lastEvent(t, s, it.ID, "verified")
	if ev == nil || ev.Actor != pipeline.ActorProof || !strings.Contains(ev.Data["words"].(string), "Regression R1: /board/regressions/1") {
		t.Fatalf("event %+v", ev)
	}
	if got := regDoneData(t, s); !sameIDs(got["verified"], it.ID) {
		t.Fatalf("regression_done data %v", got)
	}
}

// A failed test reopens the item at once: the new item has the assertion and the report in its
// ask, the screenshot as evidence, and is the one 'File it' shows for that test.
func TestE2EProofReopenedWithTheScreenshot(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	it := proofQA(t, s, `[{"kind":"e2e","tests":["e2e/web/tests/login.spec.ts"]},{"kind":"manual","look":"the login page"}]`)
	clk.Advance(time.Minute)
	r := queueReg(t, s, "web")
	claimReg(t, s)
	reportRun(t, s, r.ID)

	ev := lastEvent(t, s, it.ID, "reopened")
	if ev == nil || ev.Actor != pipeline.ActorProof {
		t.Fatalf("event %+v", ev)
	}
	nid := numID(ev.Data["reopen_item"])
	n, err := s.GetItem(ctx, nid)
	if err != nil {
		t.Fatal(err)
	}
	if n.Item.ReopenOf != it.ID || !strings.Contains(n.Item.Ask, `toHaveText("Wrong password")`) || !strings.Contains(n.Item.Ask, "/board/regressions/1") {
		t.Fatalf("reopened item %+v", n.Item)
	}
	if len(n.Evidence) != 1 || n.Evidence[0].Name != "login-failed.png" {
		t.Fatalf("evidence %+v", n.Evidence)
	}
	full, _ := s.GetRegression(ctx, r.ID)
	if full.Tests[1].ItemID != nid {
		t.Fatalf("the failing test shows %d, want the reopened #%d", full.Tests[1].ItemID, nid)
	}
	if filed, existing, err := s.FileRegressionFailure(ctx, "slack:U1", r.ID, 2); err != nil || !existing || filed.ID != nid {
		t.Fatalf("file it: %v existing %v item %d", err, existing, filed.ID)
	}
	if got := regDoneData(t, s); !sameIDs(got["reopened"], it.ID) {
		t.Fatalf("regression_done data %v", got)
	}
}

// What a run cannot vouch for leaves the check waiting: a change merged after the run was claimed
// (it ran the older main), a suite that did not run, an item waiting for its deploy.
func TestE2EProofWaits(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	mobile := proofQA(t, s, `[{"kind":"e2e","tests":[".maestro/schedules.yaml"]}]`)
	clk.Advance(time.Minute)
	r := queueReg(t, s, "web")
	claimReg(t, s)
	clk.Advance(time.Minute)
	late := proofQA(t, s, `[{"kind":"e2e","tests":["e2e/web/tests/inbox.spec.ts"]}]`)
	clk.Advance(time.Minute)
	reportRun(t, s, r.ID)
	for _, id := range []int64{mobile.ID, late.ID} {
		d, _ := s.GetItem(ctx, id)
		if d.Item.Stage != pipeline.QA || d.Item.Proof.Status() != pipeline.CheckWaiting || d.Item.Proof.Checks[0].CheckedAt != nil {
			t.Fatalf("#%d: %s %+v", id, d.Item.Stage, d.Item.Proof.Checks[0])
		}
	}
	// The next run, claimed after the late merge, decides it.
	r2 := queueReg(t, s, "web")
	claimReg(t, s)
	reportRun(t, s, r2.ID)
	if d, _ := s.GetItem(ctx, late.ID); d.Item.Stage != pipeline.Done {
		t.Fatalf("the next run: %s %+v", d.Item.Stage, d.Item.Proof.Checks[0])
	}
}

func sameIDs(v any, ids ...int64) bool {
	l, ok := v.([]any)
	if !ok || len(l) != len(ids) {
		return false
	}
	for i := range l {
		if numID(l[i]) != ids[i] {
			return false
		}
	}
	return true
}

func numID(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}
