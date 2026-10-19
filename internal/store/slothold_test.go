package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The last agent slot is held for a Mac runner's item while the Mac runner is around (#192).
func TestClaimHoldsTheLastSlotForMacWork(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	dev := []string{"emulator", "gpu", "voice"}
	busy := add(t, s, "busy", "P1")
	plain := add(t, s, "plain", "P2")
	mac := add(t, s, "mac app", "P4")
	for _, tr := range []struct {
		it  pipeline.Item
		res string
	}{{busy, ""}, {plain, ""}, {mac, "macos"}} {
		c, err := s.Claim(ctx, runner, pipeline.ClaimRequest{Runner: runner, Agent: "agent-1", Capabilities: dev, Stages: []pipeline.Stage{pipeline.Triage}})
		if err != nil || c == nil || c.Item.ID != tr.it.ID {
			t.Fatalf("triage claim: %+v %v", c, err)
		}
		evidence(t, s, tr.it.ID)
		f := map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": tr.res}
		if _, err := s.Advance(ctx, runner, tr.it.ID, pipeline.Reproduced, f, nil); err != nil {
			t.Fatal(err)
		}
	}
	two := 2
	if _, err := s.SetControl(ctx, user, ControlPatch{ActiveAgents: &two}); err != nil {
		t.Fatal(err)
	}
	if c := claim(t, s, "agent-1", dev...); c == nil || c.Item.ID != busy.ID {
		t.Fatalf("busy claim: %+v", c)
	}
	// The Mac runner checks in while loopd is stopped, so it takes nothing yet. First it takes
	// review only: the macos item waits in build, so nothing is held for it.
	stopped, running := true, false
	checkIn := func(stages ...pipeline.Stage) {
		t.Helper()
		if _, err := s.SetControl(ctx, user, ControlPatch{Stopped: &stopped}); err != nil {
			t.Fatal(err)
		}
		macReq := pipeline.ClaimRequest{Runner: "mbp", Agent: "mbp-1", Capabilities: []string{"macos"}, OnlyResources: []string{"macos"},
			PollSeconds: 20, RunnerStages: stages, Stages: stages}
		if c, err := s.Claim(ctx, "mbp", macReq); err != nil || c != nil {
			t.Fatalf("mbp check-in: %+v %v", c, err)
		}
		if _, err := s.SetControl(ctx, user, ControlPatch{Stopped: &running}); err != nil {
			t.Fatal(err)
		}
	}
	checkIn(pipeline.Review)
	if p, err := s.Runners(ctx); err != nil || len(p.Holds) != 0 {
		t.Fatalf("a Mac that takes review only holds %+v %v", p.Holds, err)
	}
	// Then build and review, as the runbook's Mac: the slot is held for the macos item.
	checkIn(pipeline.Review, pipeline.Build)

	if c := claim(t, s, "agent-2", dev...); c != nil {
		t.Fatalf("devbox took the held slot: #%d", c.Item.ID)
	}
	// The runners' columns round-trip.
	p, err := s.Runners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []pipeline.SlotHold{{Resource: "macos", Runner: "mbp", ItemID: mac.ID}}; !slices.Equal(p.Holds, want) || len(p.Runners) != 2 ||
		p.Runners[1].Name != "mbp" || !slices.Equal(p.Runners[1].OnlyResources, []string{"macos"}) || p.Runners[1].PollSeconds != 20 ||
		!slices.Equal(p.Runners[1].Stages, []pipeline.Stage{pipeline.Build, pipeline.Review}) {
		t.Fatalf("panel %+v, want holds %+v", p, want)
	}
	// Paused by a usage limit (#199), claude may not build the macos item, so nothing is held for
	// it; at the reset the hold is back.
	if _, err := s.w.Exec(`INSERT INTO usage_hits (at, provider, limit_window, reset_at, burn, item_id, error) VALUES (?,?,?,?,0,0,'')`,
		clk.Now().UnixNano(), pipeline.ProviderClaude, pipeline.Window5h, clk.Now().Add(10*time.Second).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Runners(ctx); err != nil || len(p.Holds) != 0 {
		t.Fatalf("paused, the panel holds %+v %v", p.Holds, err)
	}
	clk.Advance(11 * time.Second)
	if p, err := s.Runners(ctx); err != nil || len(p.Holds) != 1 {
		t.Fatalf("after the reset, the panel holds %+v %v, want the macos hold", p.Holds, err)
	}

	// The Mac goes quiet for more than two of its polls: the slot goes to whoever asks.
	clk.Advance(41 * time.Second)
	if c := claim(t, s, "agent-2", dev...); c == nil || c.Item.ID != plain.ID {
		t.Fatalf("devbox after the Mac went quiet: %+v", c)
	}
}

// Land runs no agent (#210): with every agent slot busy, an approved item is still claimed for land,
// and its land lease takes no agent slot.
func TestClaimLandsWhenAgentSlotsAreFull(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	a := add(t, s, "approved", "P2")
	step := func(agent string, out pipeline.Outcome, f map[string]string, paths []string, ev bool) {
		t.Helper()
		c := claim(t, s, agent)
		if c == nil || c.Item.ID != a.ID {
			t.Fatalf("claim for %s: %+v", out, c)
		}
		if ev {
			evidence(t, s, a.ID)
		}
		if _, err := s.Advance(ctx, runner, a.ID, out, f, paths); err != nil {
			t.Fatalf("%s: %v", out, err)
		}
	}
	step("triager", pipeline.Reproduced, map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": ""}, nil, true)
	step("builder", pipeline.PROpened, map[string]string{"repo": "o/r", "pr": "7", "branch": "loop/1-x", "head_sha": shaA}, []string{"M x.go"}, true)
	step("reviewer", pipeline.Approved, map[string]string{"summary": "ok", "head_sha": shaA}, nil, false)

	one := 1
	if _, err := s.SetControl(ctx, user, ControlPatch{ActiveAgents: &one}); err != nil {
		t.Fatal(err)
	}
	b := add(t, s, "busy", "P1")
	agentReq := pipeline.ClaimRequest{Runner: runner, Agent: "agent-1", Stages: []pipeline.Stage{pipeline.Triage, pipeline.Build, pipeline.Review}}
	if c, err := s.Claim(ctx, runner, agentReq); err != nil || c == nil || c.Item.ID != b.ID {
		t.Fatalf("agent claim: %+v %v", c, err)
	}
	landReq := pipeline.ClaimRequest{Runner: runner, Agent: "lander", Stages: []pipeline.Stage{pipeline.Land}}
	if c, err := s.Claim(ctx, runner, landReq); err != nil || c == nil || c.Item.ID != a.ID {
		t.Fatalf("land claim with the agent slot full: %+v %v", c, err)
	}
	c3 := add(t, s, "next", "P3")
	if c, err := s.Claim(ctx, runner, agentReq); err != nil || c != nil {
		t.Fatalf("agent claim over the cap: %+v %v", c, err)
	}
	// b's agent finishes; the land lease is still live, and the slot is free.
	evidence(t, s, b.ID)
	if _, err := s.Advance(ctx, runner, b.ID, pipeline.Reproduced, map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": ""}, nil); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Claim(ctx, runner, agentReq); err != nil || c == nil || c.Item.ID == a.ID {
		t.Fatalf("agent claim next to a land lease: %+v %v (want b or #%d)", c, err, c3.ID)
	}
}
