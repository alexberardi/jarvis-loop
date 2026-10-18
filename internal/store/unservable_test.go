package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

func kindsOf(t *testing.T, s *Store, id int64, keep ...string) []string {
	t.Helper()
	d, err := s.GetItem(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range d.Events {
		if slices.Contains(keep, e.Kind) {
			out = append(out, e.Kind)
		}
	}
	return out
}

func boardItem(t *testing.T, s *Store, id int64) BoardItem {
	t.Helper()
	items, err := s.BoardItems(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range items {
		if b.ID == id {
			return b
		}
	}
	t.Fatalf("no #%d on the board", id)
	return BoardItem{}
}

// triagedWith takes it through triage on devbox (caps) with resources res.
func triagedWith(t *testing.T, s *Store, it pipeline.Item, res string, caps ...string) {
	t.Helper()
	if c := claim(t, s, "agent-1", caps...); c == nil || c.Item.ID != it.ID {
		t.Fatalf("triage claim: %+v", c)
	}
	evidence(t, s, it.ID)
	f := map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": res}
	if _, err := s.Advance(context.Background(), runner, it.ID, pipeline.Reproduced, f, nil); err != nil {
		t.Fatal(err)
	}
}

func TestUnservableFlaggedAtOnceAndClears(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	it := add(t, s, "mac app", "P4")
	dev := []string{"emulator", "gpu", "voice"}
	triagedWith(t, s, it, "macos", dev...)

	// At once, with no clock advance: the event, the flag on the board and the chain root.
	if got := kindsOf(t, s, it.ID, KindUnservable, KindServable); !slices.Equal(got, []string{KindUnservable}) {
		t.Fatalf("events %v, want one unservable", got)
	}
	d, _ := s.GetItem(ctx, it.ID)
	if e := d.Events[len(d.Events)-1]; e.Kind != KindUnservable || e.Actor != "loopd" || e.To != "build" || len(e.Data["missing"].([]any)) != 1 {
		t.Fatalf("event %+v", e)
	}
	if b := boardItem(t, s, it.ID); !slices.Equal(b.Unservable, []string{"macos"}) {
		t.Fatalf("board unservable %v", b.Unservable)
	}
	after := add(t, s, "after it", "P4")
	if _, err := s.OrchLink(ctx, orch, after.ID, it.ID, "after"); err != nil {
		t.Fatal(err)
	}
	if w := boardItem(t, s, after.ID).Waits; len(w) != 1 || w[0].String() != "#1 → no runner offers macos" {
		t.Fatalf("waits %v", w)
	}

	// devbox polls again and again, the clock runs: still the one event.
	for range 3 {
		if c := claim(t, s, "agent-1", dev...); c != nil && c.Item.ID == it.ID {
			t.Fatalf("devbox claimed a macos item")
		}
		clk.Advance(2 * time.Minute)
		if err := s.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := kindsOf(t, s, it.ID, KindUnservable, KindServable); len(got) != 1 {
		t.Fatalf("events %v, want one unservable", got)
	}

	// A runner with macos shows up: the flag clears and that runner claims the item.
	c, err := s.Claim(ctx, "mini", pipeline.ClaimRequest{Runner: "mini", Agent: "agent-1", Capabilities: []string{"macos"}, Stages: []pipeline.Stage{pipeline.Build}})
	if err != nil || c == nil || c.Item.ID != it.ID {
		t.Fatalf("mini claim: %+v %v", c, err)
	}
	if got := kindsOf(t, s, it.ID, KindUnservable, KindServable); !slices.Equal(got, []string{KindUnservable, KindServable}) {
		t.Fatalf("events %v", got)
	}
	if b := boardItem(t, s, it.ID); b.Unservable != nil {
		t.Fatalf("still flagged: %v", b.Unservable)
	}

	// The mini goes quiet past the window: flagged again (by the minute tick).
	if _, err := s.w.Exec(`UPDATE leases SET ended_at = ?, end_reason = 'test' WHERE ended_at IS NULL`, clk.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	clk.Advance(pipeline.RunnerSeenWindow + time.Minute)
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := kindsOf(t, s, it.ID, KindUnservable, KindServable); !slices.Equal(got, []string{KindUnservable, KindServable, KindUnservable}) {
		t.Fatalf("events %v", got)
	}
}

func TestUnservableNotForServableItems(t *testing.T) {
	s, _ := open(t)
	it := add(t, s, "gpu thing", "P3")
	triagedWith(t, s, it, "gpu", "emulator", "gpu", "voice")
	if got := kindsOf(t, s, it.ID, KindUnservable, KindServable); got != nil {
		t.Fatalf("events %v", got)
	}
}

func TestOrchIdleSkipsUnservable(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	it := add(t, s, "mac app", "P4")
	triagedWith(t, s, it, "macos", "gpu")
	clk.Advance(pipeline.IdleLimits[pipeline.Build] + time.Minute)
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if q := queued(t, s); q != nil {
		for _, tr := range q.Triggers {
			if tr.Reason == pipeline.TriggerIdle && tr.ItemID == it.ID {
				t.Fatalf("idle trigger for a flagged item: %+v", q)
			}
		}
	}
}

// Land needs none of the item's resources (#209): the dev box lands a macos item, its lease holds
// no macos, and with no Mac around it is not flagged unservable.
func TestDevboxLandsAMacosItem(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	it := add(t, s, "mac app", "P1")
	dev := []string{"emulator", "gpu", "voice"}
	triagedWith(t, s, it, "macos", dev...)
	mac := func(agent string, stage pipeline.Stage) *Claim {
		t.Helper()
		c, err := s.Claim(ctx, "mbp", pipeline.ClaimRequest{Runner: "mbp", Agent: agent, Capabilities: []string{"macos"},
			OnlyResources: []string{"macos"}, Stages: []pipeline.Stage{stage}})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := mac("mbp-1", pipeline.Build); c == nil || c.Item.ID != it.ID || !slices.Equal(c.Lease.Resources, []string{"macos"}) {
		t.Fatalf("mbp build claim: %+v", c)
	}
	if _, err := s.AddEvidence(ctx, "mbp", it.ID, "log", "run.log", strings.NewReader("ok\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(ctx, "mbp", it.ID, pipeline.PROpened, map[string]string{"repo": "o/r", "pr": "7", "branch": "loop/1-x", "head_sha": shaA}, []string{"M x.go"}); err != nil {
		t.Fatal(err)
	}
	if c := mac("mbp-2", pipeline.Review); c == nil || c.Item.ID != it.ID {
		t.Fatalf("mbp review claim: %+v", c)
	}
	if _, err := s.Advance(ctx, "mbp", it.ID, pipeline.Approved, map[string]string{"summary": "ok", "head_sha": shaA}, nil); err != nil {
		t.Fatal(err)
	}
	// The Mac takes no land, and goes quiet past the window.
	if c := mac("mbp-3", pipeline.Land); c != nil {
		t.Fatalf("mbp took land: %+v", c)
	}
	clk.Advance(pipeline.RunnerSeenWindow + time.Minute)
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	c := claim(t, s, "agent-1", dev...)
	if c == nil || c.Item.ID != it.ID || c.Stage != pipeline.Land || c.Lease.Resources != nil {
		t.Fatalf("devbox land claim: %+v", c)
	}
	if got := kindsOf(t, s, it.ID, KindUnservable, KindServable); !slices.Equal(got, []string{KindUnservable, KindServable}) {
		t.Fatalf("events %v, want only the triage-time flag and its clearing", got)
	}
	if b := boardItem(t, s, it.ID); b.Unservable != nil {
		t.Fatalf("flagged in land: %v", b.Unservable)
	}
}
