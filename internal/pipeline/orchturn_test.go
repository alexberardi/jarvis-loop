package pipeline

import (
	"testing"
	"time"
)

func TestChooseWorkOrchestrate(t *testing.T) {
	ctl := DefaultControl()
	all := ClaimRequest{Runner: "devbox", Agent: "a", Kinds: []string{WorkItem, WorkPlan, WorkOrchestrate}}
	orch := &OrchCandidate{TurnID: 9, CreatedAt: t0}
	two := DefaultControl()
	two.ActiveAgents = 2
	running := LiveLease{Stage: OrchestrateStage}
	build := LiveLease{ItemID: 3, Stage: Build}
	cases := []struct {
		name  string
		ctl   Control
		items []Item
		turns []TurnCandidate
		orch  *OrchCandidate
		live  []LiveLease
		req   ClaimRequest
		item  int64
		turn  int64
		otrn  int64
	}{
		{"a queued turn beats a P1 item", ctl, []Item{waiting(1, Land, "P1", time.Hour)}, nil, orch, nil, all, 0, 0, 9},
		{"and a P1 plan turn", ctl, nil, []TurnCandidate{{TurnID: 5, PlanID: 5, Priority: "P1", CreatedAt: t0}}, orch, nil, all, 0, 0, 9},
		{"one at a time", ctl, []Item{waiting(1, Build, "P3", 0)}, nil, orch, []LiveLease{running}, all, 1, 0, 0},
		{"it shares the active-agents limit", two, []Item{waiting(1, Build, "P3", 0)}, nil, orch, []LiveLease{build, {ItemID: 4, Stage: Review}}, all, 0, 0, 0},
		{"the next free slot is its", two, []Item{waiting(1, Build, "P1", 0)}, nil, orch, []LiveLease{build}, all, 0, 0, 9},
		{"a running turn counts against the limit", two, []Item{waiting(1, Build, "P1", 0)}, nil, nil, []LiveLease{build, running}, all, 0, 0, 0},
		{"not for a runner that does not ask", ctl, []Item{waiting(1, Build, "P3", 0)}, nil, orch, nil,
			ClaimRequest{Agent: "a", Kinds: []string{WorkItem, WorkPlan}}, 1, 0, 0},
		{"orchestrate only", ctl, []Item{waiting(1, Build, "P1", 0)}, nil, nil, nil,
			ClaimRequest{Agent: "a", Kinds: []string{WorkOrchestrate}}, 0, 0, 0},
		{"stopped", Control{Stopped: true, ActiveAgents: 4}, nil, nil, orch, nil, all, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, ok := ChooseWork(tc.ctl, tc.items, nil, tc.live, tc.turns, tc.orch, tc.req, nil)
			var gotItem, gotTurn, gotOrch int64
			if ok && w.Item != nil {
				gotItem = w.Item.ID
			}
			if ok && w.Turn != nil {
				gotTurn = w.Turn.TurnID
			}
			if ok && w.Orch != nil {
				gotOrch = w.Orch.TurnID
			}
			want := tc.item != 0 || tc.turn != 0 || tc.otrn != 0
			if ok != want || gotItem != tc.item || gotTurn != tc.turn || gotOrch != tc.otrn {
				t.Fatalf("got item %d turn %d orchestrate %d (ok %v), want %d %d %d", gotItem, gotTurn, gotOrch, ok, tc.item, tc.turn, tc.otrn)
			}
		})
	}
}

func TestOrchTriggerFor(t *testing.T) {
	cases := []struct {
		kind     string
		from, to Stage
		outcome  Outcome
		want     string
	}{
		{"advanced", Build, NeedsYou, Question, TriggerNeedsYou},
		{"advanced", Build, NeedsYou, Failed, TriggerRunFailed},
		{"run_overtime", Build, NeedsYou, "", TriggerRunFailed},
		{"lease_expired", Build, Build, "", TriggerLeaseExpired},
		{"advanced", Triage, Build, Reproduced, ""},
		{"answered", NeedsYou, Build, "", ""},
		// a note on an item already in needs_you is not a new entry
		{"event", NeedsYou, NeedsYou, "", ""},
	}
	for _, tc := range cases {
		if got := OrchTriggerFor(tc.kind, tc.from, tc.to, tc.outcome); got != tc.want {
			t.Errorf("%s %s→%s %s: got %q, want %q", tc.kind, tc.from, tc.to, tc.outcome, got, tc.want)
		}
	}
}

func TestAddTrigger(t *testing.T) {
	var ts []Trigger
	ts = AddTrigger(ts, Trigger{Reason: TriggerRunFailed, ItemID: 3, At: t0})
	ts = AddTrigger(ts, Trigger{Reason: TriggerRunFailed, ItemID: 3, At: t0.Add(time.Minute)})
	ts = AddTrigger(ts, Trigger{Reason: TriggerNeedsYou, ItemID: 3, At: t0})
	ts = AddTrigger(ts, Trigger{Reason: TriggerSweep, At: t0})
	ts = AddTrigger(ts, Trigger{Reason: TriggerSweep, At: t0.Add(15 * time.Minute)})
	ts = AddTrigger(ts, Trigger{Reason: TriggerMessage, Text: "what's stuck?", At: t0})
	ts = AddTrigger(ts, Trigger{Reason: TriggerMessage, Text: "and #35?", At: t0})
	if len(ts) != 5 {
		t.Fatalf("got %d triggers, want 5 (the same reason on the same item joins; messages each count): %+v", len(ts), ts)
	}
	if !ts[0].At.Equal(t0) {
		t.Fatalf("a repeated trigger keeps the first time, got %v", ts[0].At)
	}
	for range MaxTriggers + 10 {
		ts = AddTrigger(ts, Trigger{Reason: TriggerMessage, Text: "x", At: t0})
	}
	if len(ts) != MaxTriggers {
		t.Fatalf("got %d triggers, want them capped at %d", len(ts), MaxTriggers)
	}
}

func TestIdlePast(t *testing.T) {
	cases := []struct {
		stage Stage
		idle  time.Duration
		want  bool
	}{
		{Triage, 59 * time.Minute, false},
		{Triage, time.Hour, true},
		{Build, 90 * time.Minute, false},
		{Build, 2 * time.Hour, true},
		{Review, time.Hour, true},
		{Land, 61 * time.Minute, true},
		// The user's stages: they wait for the user, which needs_you and the escalations cover.
		{NeedsYou, 48 * time.Hour, false},
		{Held, 48 * time.Hour, false},
		{QA, 48 * time.Hour, false},
		{Done, 48 * time.Hour, false},
	}
	for _, tc := range cases {
		if got := IdlePast(tc.stage, t0.Add(-tc.idle), t0); got != tc.want {
			t.Errorf("%s idle %s: got %v, want %v", tc.stage, tc.idle, got, tc.want)
		}
	}
}

func TestOrchSweep(t *testing.T) {
	cases := []struct {
		name      string
		last      time.Time
		open      int
		fp, oldFP string
		due, run  bool
	}{
		{"not yet", t0.Add(-14 * time.Minute), 3, "9", "5", false, false},
		{"something changed", t0.Add(-15 * time.Minute), 3, "9", "5", true, true},
		{"nothing changed: no model call", t0.Add(-15 * time.Minute), 3, "9", "9", true, false},
		{"nothing open", t0.Add(-time.Hour), 0, "9", "5", true, false},
		{"the first sweep", time.Time{}, 1, "9", "", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			due, run := OrchSweep(tc.last, t0, tc.open, tc.fp, tc.oldFP)
			if due != tc.due || run != tc.run {
				t.Fatalf("got due %v run %v, want %v %v", due, run, tc.due, tc.run)
			}
		})
	}
}

func TestOrchCapAndDay(t *testing.T) {
	if !OrchUnderCap(150, 149) || OrchUnderCap(150, 150) || OrchUnderCap(0, 0) {
		t.Fatal("the cap: 149 of 150 may start, the 151st may not; 0 turns a day is off")
	}
	ny, _ := time.LoadLocation("America/New_York")
	// 03:30 UTC is still the day before in New York.
	if got := OrchDay(time.Date(2026, 10, 17, 3, 30, 0, 0, time.UTC), ny); got != "2026-10-16" {
		t.Fatalf("got day %s", got)
	}
	if OrchResume("s1", "2026-10-16", "2026-10-16") != "s1" || OrchResume("s1", "2026-10-16", "2026-10-17") != "" {
		t.Fatal("the session resumes on its own day and rotates on the next")
	}
}

func TestControlValidateOrchTurns(t *testing.T) {
	c := DefaultControl()
	if c.OrchTurnsPerDay != DefaultOrchTurnsPerDay {
		t.Fatalf("default %d", c.OrchTurnsPerDay)
	}
	for _, n := range []int{-1, MaxOrchTurnsPerDay + 1} {
		c.OrchTurnsPerDay = n
		if err := c.Validate(); err == nil {
			t.Fatalf("%d turns a day validated", n)
		}
	}
	c.OrchTurnsPerDay = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("0 (off): %v", err)
	}
}
