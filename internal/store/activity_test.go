package store

import (
	"context"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

func storedActivity(t *testing.T, s *Store, table, lease string) string {
	t.Helper()
	var a string
	if err := s.r.QueryRow(`SELECT activity FROM `+table+` WHERE id = ?`, lease).Scan(&a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestLeaseActivity(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	it := add(t, s, "Live card", "")
	c := claim(t, s, "a")
	act := &pipeline.Activity{Phase: "triage agent", ToolCalls: 3, Todos: []pipeline.Todo{{Text: "read the code", Status: pipeline.TodoInProgress}}}
	if stop, err := s.HeartbeatActivity(ctx, runner, c.Lease.ID, "agent running", act); err != nil || stop {
		t.Fatalf("heartbeat: %v %v", stop, err)
	}
	if _, err := s.HeartbeatActivity(ctx, "other-runner", c.Lease.ID, "", act); code(err) != CodeNotYourLease {
		t.Fatalf("another runner's heartbeat: %v", err)
	}
	// No activity: the previous one stays, as the note does.
	if _, err := s.Heartbeat(ctx, runner, c.Lease.ID, ""); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetItem(ctx, it.ID)
	if d.Lease == nil || d.Lease.Activity == nil || d.Lease.Activity.Todos[0].Text != "read the code" {
		t.Fatalf("lease activity: %+v", d.Lease)
	}
	w, err := s.Working(ctx, 0)
	if err != nil || len(w) != 1 || w[0].ItemID != it.ID || w[0].Title != "Live card" || w[0].Stage != "triage" ||
		w[0].Activity == nil || w[0].Activity.ToolCalls != 3 {
		t.Fatalf("working: %+v %v", w, err)
	}
	if w, _ := s.Working(ctx, it.ID+1); len(w) != 0 {
		t.Fatalf("another item's working: %+v", w)
	}

	// The lease expires: its activity goes with it.
	clk.Advance(LeaseTTL + time.Minute)
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if a := storedActivity(t, s, "leases", c.Lease.ID); a != "" {
		t.Fatalf("activity kept after the lease ended: %s", a)
	}
	if w, _ := s.Working(ctx, 0); len(w) != 0 {
		t.Fatalf("working after expiry: %+v", w)
	}

	// A lease the runner ends by advancing is cleared too.
	c = claim(t, s, "a")
	if _, err := s.HeartbeatActivity(ctx, runner, c.Lease.ID, "", act); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(ctx, runner, it.ID, pipeline.Question, map[string]string{"question": "which one?"}, nil); err != nil {
		t.Fatal(err)
	}
	if a := storedActivity(t, s, "leases", c.Lease.ID); a != "" {
		t.Fatalf("activity kept after advance: %s", a)
	}
}

func TestPlanLeaseActivity(t *testing.T) {
	s, _ := openPlans(t)
	ctx := context.Background()
	p := newPlan(t, s, "Plan card", "")
	c := claimTurn(t, s, "agent-1")
	act := &pipeline.Activity{Phase: "plan agent", Persona: pipeline.PersonaProduct, PlanID: p.ID}
	if _, err := s.HeartbeatActivity(ctx, runner, c.Lease.ID, "thinking", act); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Heartbeat(ctx, runner, c.Lease.ID, ""); err != nil {
		t.Fatal(err)
	}
	w, err := s.Working(ctx, 0)
	if err != nil || len(w) != 1 || w[0].PlanID != p.ID || w[0].ItemID != 0 || w[0].Title != "Plan card" ||
		w[0].Persona != pipeline.PersonaProduct || w[0].Activity == nil || w[0].Activity.Phase != "plan agent" {
		t.Fatalf("working: %+v %v", w, err)
	}
	result(t, s, c, pipeline.TurnResult{Reply: "Done."})
	if a := storedActivity(t, s, "plan_leases", c.Lease.ID); a != "" {
		t.Fatalf("activity kept after the turn: %s", a)
	}
	if w, _ := s.Working(ctx, 0); len(w) != 0 {
		t.Fatalf("working after the turn: %+v", w)
	}
}
