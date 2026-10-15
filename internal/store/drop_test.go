package store

import (
	"context"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// A drop closes the item and ends its live lease the way overtime does: the running agent's next
// heartbeat is told to stop, and its report cannot advance the closed item.
func TestDropEndsTheLiveLease(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := add(t, s, "x", "")
	c := claim(t, s, "a")
	got, err := s.Drop(ctx, user, it.ID, "no hardware")
	if err != nil || got.Stage != pipeline.Closed {
		t.Fatalf("drop: %+v %v", got, err)
	}
	if stop, err := s.Heartbeat(ctx, runner, c.Lease.ID, ""); err != nil || !stop {
		t.Fatalf("heartbeat after drop: stop=%v %v", stop, err)
	}
	if _, err := s.Advance(ctx, runner, it.ID, pipeline.Question, map[string]string{"question": "q"}, nil); code(err) != CodeNoLease {
		t.Fatalf("advance after drop: %v", err)
	}
	d, _ := s.GetItem(ctx, it.ID)
	if d.Lease != nil {
		t.Fatalf("live lease after drop: %+v", d.Lease)
	}
	var ev *Event
	for i := range d.Events {
		if d.Events[i].Kind == "dropped" {
			ev = &d.Events[i]
		}
	}
	if ev == nil || ev.Actor != user || ev.From != "triage" || ev.To != "closed" ||
		ev.Data["reason"] != pipeline.DropReason || ev.Data["words"] != "no hardware" || ev.Data["lease"] != c.Lease.ID {
		t.Fatalf("dropped event: %+v", ev)
	}
	var reason string
	if err := s.r.QueryRow(`SELECT end_reason FROM leases WHERE id = ?`, c.Lease.ID).Scan(&reason); err != nil || reason != "dropped" {
		t.Fatalf("lease end reason %q %v", reason, err)
	}
	if _, err := s.Drop(ctx, user, it.ID, ""); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("second drop: %v", err)
	}
}

// The runner finds dropped items whose PR it has not closed yet, and records pr_closed once.
func TestDroppedPRToClose(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	toDone(t, s, runner) // a done item with a PR: never listed
	held := add(t, s, "held", "")
	claim(t, s, "t")
	if _, err := s.Advance(ctx, runner, held.ID, pipeline.Question, map[string]string{"question": "q"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`UPDATE items SET pr = 42, repo = 'o/r' WHERE id = ?`, held.ID); err != nil {
		t.Fatal(err)
	}
	noPR := add(t, s, "no pr", "")

	list := func() []int64 {
		t.Helper()
		rows, err := s.ListItemsFilter(ctx, ListFilter{PRToClose: true})
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		return ids
	}
	if ids := list(); len(ids) != 0 {
		t.Fatalf("before any drop: %v", ids)
	}
	if _, err := s.PRClosed(ctx, runner, held.ID, map[string]any{"pr": float64(42), "state": "closed"}); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("pr_closed on an open item: %v", err)
	}
	for _, id := range []int64{held.ID, noPR.ID} {
		if _, err := s.Drop(ctx, user, id, ""); err != nil {
			t.Fatal(err)
		}
	}
	if ids := list(); len(ids) != 1 || ids[0] != held.ID {
		t.Fatalf("to close: %v, want [%d]", ids, held.ID)
	}
	if _, err := s.PRClosed(ctx, runner, held.ID, map[string]any{"pr": float64(42), "state": "purple"}); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("bad state: %v", err)
	}
	if _, err := s.PRClosed(ctx, runner, noPR.ID, map[string]any{"pr": float64(1), "state": "closed"}); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("pr_closed without a PR: %v", err)
	}
	if _, err := s.PRClosed(ctx, runner, held.ID, map[string]any{"pr": float64(3), "state": "closed"}); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("pr_closed for another PR: %v", err)
	}
	ev, err := s.PRClosed(ctx, runner, held.ID, map[string]any{"pr": float64(42), "state": "already_closed"})
	if err != nil || ev.Kind != "pr_closed" || ev.Actor != "runner:"+runner || ev.From != "closed" {
		t.Fatalf("pr_closed: %+v %v", ev, err)
	}
	if ids := list(); len(ids) != 0 {
		t.Fatalf("after pr_closed: %v", ids)
	}
	if _, err := s.PRClosed(ctx, runner, held.ID, map[string]any{"pr": float64(42), "state": "closed"}); code(err) != CodeAlreadyRecorded {
		t.Fatalf("second pr_closed: %v", err)
	}
}
