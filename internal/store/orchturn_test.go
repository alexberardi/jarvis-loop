package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

var allKinds = []string{pipeline.WorkItem, pipeline.WorkPlan, pipeline.WorkOrchestrate}

func claimKinds(t *testing.T, s *Store, agent string, kinds ...string) *Claim {
	t.Helper()
	c, err := s.Claim(context.Background(), runner, pipeline.ClaimRequest{Runner: runner, Agent: agent, Kinds: kinds})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func orchTurns(t *testing.T, s *Store) []OrchTurn {
	t.Helper()
	ts, err := s.OrchTurns(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func queued(t *testing.T, s *Store) *OrchTurn {
	t.Helper()
	q, err := orchTurnWith(s.r, OrchQueued)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func reasons(ts []pipeline.Trigger) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Reason)
	}
	return out
}

func finish(t *testing.T, s *Store, c *Claim, session string, actions ...OrchActionResult) OrchTurn {
	t.Helper()
	ot, err := s.OrchestrateResult(context.Background(), runner, c.Orch.ID, OrchTurnResult{SessionID: session, Actions: actions})
	if err != nil {
		t.Fatal(err)
	}
	return ot
}

func TestOrchTriggersJoinOneTurn(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	a, b, c := add(t, s, "a", "P1"), add(t, s, "b", "P1"), add(t, s, "c", "P1")
	if q := queued(t, s); q != nil {
		t.Fatalf("an added item is no trigger: %+v", q)
	}
	stuck(t, s, a.ID, pipeline.Failed)
	stuck(t, s, b.ID, pipeline.Question)
	q := queued(t, s)
	if q == nil || len(q.Triggers) != 2 || q.Triggers[0].Reason != pipeline.TriggerRunFailed || q.Triggers[0].ItemID != a.ID ||
		q.Triggers[1].Reason != pipeline.TriggerNeedsYou || q.Triggers[1].ItemID != b.ID {
		t.Fatalf("queued: %+v", q)
	}
	// Only a runner that asks gets it; one that does gets it ahead of a waiting P1 item.
	if cl := claimKinds(t, s, "x", pipeline.WorkItem); cl == nil || cl.Kind != pipeline.WorkItem || cl.Item.ID != c.ID {
		t.Fatalf("items-only claim: %+v", cl)
	}
	if _, err := s.Advance(ctx, runner, c.ID, pipeline.Question, map[string]string{"question": "which repo?"}, nil); err != nil {
		t.Fatal(err)
	}
	cl := claimKinds(t, s, "o", allKinds...)
	if cl == nil || cl.Kind != pipeline.WorkOrchestrate || cl.Orch == nil || cl.Stage != pipeline.OrchestrateStage {
		t.Fatalf("claim: %+v", cl)
	}
	if got := reasons(cl.Orch.Triggers); len(got) != 3 {
		t.Fatalf("the turn's triggers: %v", got)
	}
	if cl.Orch.Resume != "" || cl.Lease.ID == "" {
		t.Fatalf("first turn: %+v", cl.Orch)
	}
	// While it runs, a trigger queues the next turn, and no second turn starts.
	if _, err := s.Answer(ctx, user, c.ID, "jarvis-server"); err != nil {
		t.Fatal(err)
	}
	stuck(t, s, c.ID, pipeline.Failed)
	if q := queued(t, s); q == nil || q.ID == cl.Orch.ID || len(q.Triggers) != 1 || q.Triggers[0].ItemID != c.ID {
		t.Fatalf("the next turn: %+v", q)
	}
	if again := claimKinds(t, s, "o2", pipeline.WorkOrchestrate); again != nil {
		t.Fatalf("a second turn started while one runs: %+v", again)
	}
	if stop, err := s.Heartbeat(ctx, runner, cl.Lease.ID, "agent running"); err != nil || stop {
		t.Fatalf("heartbeat: %v %v", stop, err)
	}
	ot := finish(t, s, cl, "sess-1", OrchActionResult{Action: "retry", ItemID: a.ID, Status: 429, Code: pipeline.CodeTooSoon, Message: "retry after"})
	if ot.Status != OrchDone || len(ot.Actions) != 1 || ot.Actions[0].OK {
		t.Fatalf("result: %+v", ot)
	}
	if _, err := s.OrchestrateResult(ctx, runner, cl.Orch.ID, OrchTurnResult{}); code(err) != CodeNoLease {
		t.Fatalf("a second result: %v", err)
	}
	if next := claimKinds(t, s, "o3", pipeline.WorkOrchestrate); next == nil || next.Orch.Resume != "sess-1" {
		t.Fatalf("the next turn resumes the day's session: %+v", next)
	}
}

func TestOrchSessionRotatesDaily(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	if err := s.QueueOrchestrate(ctx, pipeline.TriggerMessage, "what's stuck?"); err != nil {
		t.Fatal(err)
	}
	cl := claimKinds(t, s, "o", pipeline.WorkOrchestrate)
	if cl == nil || cl.Orch.Triggers[0].Text != "what's stuck?" {
		t.Fatalf("claim: %+v", cl)
	}
	finish(t, s, cl, "sess-day1")
	s.QueueOrchestrate(ctx, pipeline.TriggerMessage, "and now?")
	cl = claimKinds(t, s, "o", pipeline.WorkOrchestrate)
	if cl.Orch.Resume != "sess-day1" {
		t.Fatalf("same day: %+v", cl.Orch)
	}
	finish(t, s, cl, "sess-day1")
	// t0 is 08:00 in New York; 16 hours on is the next day there.
	clk.Advance(16 * time.Hour)
	s.QueueOrchestrate(ctx, pipeline.TriggerMessage, "good morning")
	cl = claimKinds(t, s, "o", pipeline.WorkOrchestrate)
	if cl.Orch.Resume != "" || cl.Orch.Day != "2026-10-15" {
		t.Fatalf("a new day starts a new session: %+v", cl.Orch)
	}
	finish(t, s, cl, "sess-day2")
	var sess, day string
	s.r.QueryRow(`SELECT session, session_day FROM orch_state`).Scan(&sess, &day)
	if sess != "sess-day2" || day != "2026-10-15" {
		t.Fatalf("state: %s %s", sess, day)
	}
}

func TestOrchDailyCap(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	two := 2
	if _, err := s.SetControl(ctx, user, ControlPatch{OrchTurnsPerDay: &two}); err != nil {
		t.Fatal(err)
	}
	it := add(t, s, "x", "P3")
	for i := range 2 {
		s.QueueOrchestrate(ctx, pipeline.TriggerMessage, "turn")
		cl := claimKinds(t, s, "o", allKinds...)
		if cl == nil || cl.Kind != pipeline.WorkOrchestrate {
			t.Fatalf("turn %d: %+v", i+1, cl)
		}
		finish(t, s, cl, "s")
	}
	s.QueueOrchestrate(ctx, pipeline.TriggerMessage, "the third")
	cl := claimKinds(t, s, "o", allKinds...)
	if cl == nil || cl.Kind != pipeline.WorkItem || cl.Item.ID != it.ID {
		t.Fatalf("over the cap the slot goes to the item: %+v", cl)
	}
	if cl := claimKinds(t, s, "o", pipeline.WorkOrchestrate); cl != nil {
		t.Fatalf("over the cap: %+v", cl)
	}
	var n int
	s.r.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ?`, KindOrchCapped).Scan(&n)
	if n != 1 {
		t.Fatalf("capped events: %d, want one a day", n)
	}
	clk.Advance(16 * time.Hour)
	if cl := claimKinds(t, s, "o", pipeline.WorkOrchestrate); cl == nil || cl.Orch.Triggers[0].Text != "the third" {
		t.Fatalf("the next day the waiting turn starts: %+v", cl)
	}
	zero := 0
	if _, err := s.SetControl(ctx, user, ControlPatch{OrchTurnsPerDay: &zero}); err != nil {
		t.Fatal(err)
	}
	if ctl, _ := s.Control(ctx); ctl.OrchTurnsPerDay != 0 {
		t.Fatalf("control: %+v", ctl)
	}
}

func TestOrchSweepSkipsWhenNothingChanged(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	if err := s.OrchTick(ctx); err != nil {
		t.Fatal(err)
	}
	if q := queued(t, s); q != nil {
		t.Fatalf("nothing open, no sweep: %+v", q)
	}
	it := add(t, s, "x", "P3")
	clk.Advance(pipeline.OrchSweepEvery)
	s.OrchTick(ctx)
	q := queued(t, s)
	if q == nil || reasons(q.Triggers)[0] != pipeline.TriggerSweep {
		t.Fatalf("sweep: %+v", q)
	}
	cl := claimKinds(t, s, "o", pipeline.WorkOrchestrate)
	// The turn's own note and actions do not count as a change: the priority's event kind is not
	// orchestrat*, so only its actor leaves it out.
	if _, err := s.OrchNote(ctx, orch, "watching #1", "first look"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OrchPriority(ctx, orch, it.ID, "P1", "urgent"); err != nil {
		t.Fatal(err)
	}
	finish(t, s, cl, "s")
	clk.Advance(pipeline.OrchSweepEvery)
	s.OrchTick(ctx)
	if q := queued(t, s); q != nil {
		t.Fatalf("nothing changed, yet a turn was queued: %+v", q)
	}
	p := "P2"
	if _, err := s.PatchItem(ctx, user, it.ID, &p, nil); err != nil {
		t.Fatal(err)
	}
	clk.Advance(10 * time.Minute)
	s.OrchTick(ctx)
	if q := queued(t, s); q != nil {
		t.Fatalf("before the 15 minutes: %+v", q)
	}
	clk.Advance(5 * time.Minute)
	s.OrchTick(ctx)
	if q := queued(t, s); q == nil {
		t.Fatal("the board changed: the sweep queues a turn")
	}
}

func TestOrchIdleOncePerWait(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	it := add(t, s, "x", "P3")
	dep := add(t, s, "y", "P3")
	if _, err := s.OrchLink(ctx, orch, dep.ID, it.ID, "after x"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(59 * time.Minute)
	s.OrchTick(ctx)
	if q := queued(t, s); q != nil && len(q.Triggers) > 0 && q.Triggers[0].Reason == pipeline.TriggerIdle {
		t.Fatalf("idle too soon: %+v", q)
	}
	clk.Advance(time.Minute)
	s.OrchTick(ctx)
	q := queued(t, s)
	var idle []int64
	for _, tr := range q.Triggers {
		if tr.Reason == pipeline.TriggerIdle {
			idle = append(idle, tr.ItemID)
		}
	}
	if len(idle) != 1 || idle[0] != it.ID {
		t.Fatalf("idle triggers %v, want only #%d (#%d waits on it)", idle, it.ID, dep.ID)
	}
	cl := claimKinds(t, s, "o", pipeline.WorkOrchestrate)
	finish(t, s, cl, "s")
	clk.Advance(time.Hour)
	s.OrchTick(ctx)
	if q := queued(t, s); q != nil {
		for _, tr := range q.Triggers {
			if tr.Reason == pipeline.TriggerIdle {
				t.Fatalf("the same wait triggered twice: %+v", q)
			}
		}
	}
}

func TestOrchLeaseExpiryRequeues(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	s.QueueOrchestrate(ctx, pipeline.TriggerMessage, "hello")
	cl := claimKinds(t, s, "o", pipeline.WorkOrchestrate)
	clk.Advance(LeaseTTL + time.Second)
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	q := queued(t, s)
	if q == nil || q.ID != cl.Orch.ID || len(q.Triggers) != 1 {
		t.Fatalf("the expired turn waits again: %+v", q)
	}
	var n int
	s.r.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ?`, KindOrchExpired).Scan(&n)
	if n != 1 {
		t.Fatalf("expired events: %d", n)
	}
	if _, err := s.Heartbeat(ctx, runner, cl.Lease.ID, ""); code(err) != CodeNotFound {
		t.Fatalf("the old lease: %v", err)
	}
	if _, err := s.OrchestrateResult(ctx, runner, cl.Orch.ID, OrchTurnResult{}); code(err) != CodeNoLease {
		t.Fatalf("a result after expiry: %v", err)
	}
	// Stopping the loop puts a running turn back too.
	cl = claimKinds(t, s, "o", pipeline.WorkOrchestrate)
	stopped := true
	s.SetControl(ctx, user, ControlPatch{Stopped: &stopped})
	if stop, err := s.Heartbeat(ctx, runner, cl.Lease.ID, ""); err != nil || !stop {
		t.Fatalf("heartbeat when stopped: %v %v", stop, err)
	}
	if q := queued(t, s); q == nil || q.ID != cl.Orch.ID {
		t.Fatalf("stopped: %+v", q)
	}
}

func TestOrchRetryBackoffIsDue(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	it := add(t, s, "x", "P3")
	stuck(t, s, it.ID, pipeline.Failed)
	cl := claimKinds(t, s, "o", pipeline.WorkOrchestrate)
	if _, err := s.OrchRetry(ctx, orch, it.ID, "git lock"); code(err) != pipeline.CodeTooSoon {
		t.Fatalf("retry at once: %v", err)
	}
	finish(t, s, cl, "s")
	clk.Advance(time.Minute)
	s.OrchTick(ctx)
	if q := queued(t, s); q != nil {
		t.Fatalf("before the backoff: %+v", q)
	}
	clk.Advance(time.Minute)
	s.OrchTick(ctx)
	if q := queued(t, s); q == nil || reasons(q.Triggers)[0] != pipeline.TriggerDue {
		t.Fatalf("the backoff ended: %+v", q)
	}
}

func TestOrchLoopMergeTriggers(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	s.SetLoopRepo("sandbox/loop")
	loop, err := s.AddItem(ctx, user, NewItem{Title: "loop change", Repo: "sandbox/loop"})
	if err != nil {
		t.Fatal(err)
	}
	other := add(t, s, "server change", "P3")
	merged := func(id int64) {
		err := s.tx(ctx, func(tx *sql.Tx) error {
			return s.event(tx, Event{ItemID: id, Actor: "runner:" + runner, Kind: "advanced", From: "land", To: "done", Outcome: string(pipeline.Merged)})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	merged(other.ID)
	if q := queued(t, s); q != nil {
		t.Fatalf("a merge elsewhere: %+v", q)
	}
	merged(loop.ID)
	if q := queued(t, s); q == nil || q.Triggers[0].Reason != pipeline.TriggerLoopMerged || q.Triggers[0].ItemID != loop.ID {
		t.Fatalf("a merge to the loop: %+v", q)
	}
}

// The snapshot's items say what each needs from a runner, so "ready but unclaimed" can be told
// from the runners' capabilities.
func TestOrchSnapshotItemResources(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := add(t, s, "x", "P3")
	if c := claim(t, s, "a"); c == nil || c.Item.ID != it.ID {
		t.Fatalf("claim: %+v", c)
	}
	evidence(t, s, it.ID)
	f := map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": "emulator"}
	if _, err := s.Advance(ctx, runner, it.ID, pipeline.Reproduced, f, nil); err != nil {
		t.Fatal(err)
	}
	snap, err := s.OrchSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Items) != 1 || len(snap.Items[0].Resources) != 1 || snap.Items[0].Resources[0] != "emulator" {
		t.Fatalf("snapshot items: %+v", snap.Items)
	}
}
