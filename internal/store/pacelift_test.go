package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// #316: the user lifts claude's pacing until a time (or the next limit hit); the orchestrator's
// notebook says so; the lift is stored, expires, and can be cleared.
func TestPaceLiftLiftsAWarnedWindow(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	warned := claudeRun(1000)
	warned["rate_limit_status"] = "allowed_warning"
	a, b := add(t, s, "P2 M one", "P2"), add(t, s, "P2 M two", "P2")
	m := claudeRun(1000)
	m["size"] = "M"
	triageWith(t, s, a, m)
	warned["size"] = "M"
	triageWith(t, s, b, warned)

	// Warned: slow, 3 at once; the fallback takes one P2 M, not the second.
	if c := claim(t, s, "agent-1"); c == nil || c.Item.ID != a.ID {
		t.Fatalf("slow claim %+v, want the fallback's one", c)
	}
	if c := claim(t, s, "agent-2"); c != nil {
		t.Fatalf("second claim while slow: %+v", c)
	}

	until := clk.Now().Add(2 * time.Hour)
	ctl, err := s.SetControl(ctx, user, ControlPatch{PaceNormalUntil: ptr(until.Format(time.RFC3339))})
	if err != nil {
		t.Fatal(err)
	}
	if !ctl.PaceNormalSet.Equal(clk.Now()) || !ctl.PaceNormalUntil.Equal(until) {
		t.Fatalf("stored %v until %v", ctl.PaceNormalSet, ctl.PaceNormalUntil)
	}
	if back, _ := s.Control(ctx); !back.PaceNormalSet.Equal(ctl.PaceNormalSet) || !back.PaceNormalUntil.Equal(until) {
		t.Fatalf("read back %v until %v", back.PaceNormalSet, back.PaceNormalUntil)
	}
	notes, _ := s.Notebook(ctx, 1)
	if len(notes) != 1 || notes[0].Actor != user || !strings.Contains(str(notes[0].Data, "text"), "lifted claude's pacing until") {
		t.Fatalf("notebook %+v", notes)
	}
	if c := claim(t, s, "agent-2"); c == nil || c.Item.ID != b.ID {
		t.Fatalf("claim while lifted %+v", c)
	}
	if e := lastPace(t, s); e.Data["level"] != float64(pipeline.PaceNormal) || e.Data["lifted"] != true {
		t.Fatalf("pace event %+v", e.Data)
	}
	rep, _ := s.Usage(ctx)
	if !rep.Pace.Lifted || !strings.Contains(rep.Pace.Describe(time.UTC), "pacing lifted until") {
		t.Fatalf("usage pace %+v", rep.Pace)
	}

	// It expires at its time.
	clk.Advance(2*time.Hour + time.Second)
	if rep, _ := s.Usage(ctx); rep.Pace.Lifted || rep.Pace.Level != pipeline.PaceSlow {
		t.Fatalf("after --until: %+v", rep.Pace)
	}

	// "reset" (no time) lasts to the next hit; "auto" clears it.
	ctl, err = s.SetControl(ctx, user, ControlPatch{PaceNormalUntil: ptr(PaceUntilNextHit)})
	if err != nil || ctl.PaceNormalSet.IsZero() || !ctl.PaceNormalUntil.IsZero() {
		t.Fatalf("reset lift: %+v %v", ctl, err)
	}
	if notes, _ := s.Notebook(ctx, 1); !strings.Contains(str(notes[0].Data, "text"), "until the next limit hit") {
		t.Fatalf("notebook %+v", notes)
	}
	ctl, err = s.SetControl(ctx, user, ControlPatch{PaceNormalUntil: ptr("")})
	if err != nil || !ctl.PaceNormalSet.IsZero() || !ctl.PaceNormalUntil.IsZero() {
		t.Fatalf("cleared: %+v %v", ctl, err)
	}
	if notes, _ := s.Notebook(ctx, 1); !strings.Contains(str(notes[0].Data, "text"), "back on") {
		t.Fatalf("notebook %+v", notes)
	}
	// A time in the past or nonsense is refused.
	for _, v := range []string{clk.Now().Add(-time.Hour).Format(time.RFC3339), "soon"} {
		if _, err := s.SetControl(ctx, user, ControlPatch{PaceNormalUntil: ptr(v)}); err == nil {
			t.Fatalf("%q: no error", v)
		}
	}
}

