package pipeline

import (
	"slices"
	"testing"
	"time"
)

func TestWaitReason(t *testing.T) {
	ctl := DefaultControl() // 4 agents, triage:1, review:2, land:3, gpu:2, voice:1, emulator:1
	stopped := DefaultControl()
	stopped.Stopped = true
	paused := DefaultControl()
	paused.ActiveAgents = 0
	two := DefaultControl()
	two.ActiveAgents = 2

	dep := func(it Item, ids ...int64) Item { it.DependsOn = ids; return it }
	qa := waiting(1, QA, "P3", 0)
	qa.AwaitingDeploy = true
	in := func(it Item, repo string) Item { it.Repo = repo; return it }
	frozen := Frozen{"o/r": 5}
	macHold := []SlotHold{{Resource: "macos", Runner: "mac", ItemID: 2}}
	normal := Pace{Level: PaceNormal, Agents: ctl.ActiveAgents}

	cases := []struct {
		name       string
		ctl        Control
		it         Item
		deps       map[int64]Stage
		live       []LiveLease
		unservable []string
		frozen     Frozen
		holds      []SlotHold
		want       string
	}{
		{"needs you", ctl, waiting(1, NeedsYou, "P3", 0), nil, nil, nil, nil, nil, WaitNeedsYou},
		{"held", ctl, waiting(1, Held, "P3", 0), nil, nil, nil, nil, nil, WaitHeld},
		{"qa", ctl, waiting(1, QA, "P3", 0), nil, nil, nil, nil, nil, WaitQA},
		{"qa after deploy", ctl, qa, nil, nil, nil, nil, nil, WaitQA},
		{"done is not waiting", ctl, waiting(1, Done, "P3", 0), nil, nil, nil, nil, nil, ""},
		{"closed is not waiting", ctl, waiting(1, Closed, "P3", 0), nil, nil, nil, nil, nil, ""},
		{"claimed is not waiting", ctl, waiting(1, Build, "P3", 0), nil, []LiveLease{{ItemID: 1, Stage: Build}}, nil, nil, nil, ""},
		{"ready", ctl, waiting(1, Build, "P3", 0), nil, nil, nil, nil, nil, WaitReady},
		{"dependency not merged", ctl, dep(waiting(1, Build, "P3", 0), 7), map[int64]Stage{7: Land}, nil, nil, nil, nil, WaitDeps},
		{"dependency merged, in qa", ctl, dep(waiting(1, Build, "P3", 0), 7), map[int64]Stage{7: QA}, nil, nil, nil, nil, WaitReady},
		{"dependency done", ctl, dep(waiting(1, Build, "P3", 0), 7), map[int64]Stage{7: Done}, nil, nil, nil, nil, WaitReady},
		// The item's own blockers come before the loop-wide ones: once the loop resumes it would
		// still wait for them.
		{"dependency beats stopped", stopped, dep(waiting(1, Build, "P3", 0), 7), map[int64]Stage{}, nil, nil, nil, nil, WaitDeps},
		{"no runner for a resource", ctl, waiting(1, Build, "P3", 0, "macos"), nil, nil, []string{"macos"}, nil, nil, WaitNoRunner},
		{"stopped", stopped, waiting(1, Build, "P3", 0), nil, nil, nil, nil, nil, WaitStopped},
		{"active agents 0", paused, waiting(1, Build, "P3", 0), nil, nil, nil, nil, nil, WaitNoSlot},
		{"every slot busy", two, waiting(1, Build, "P3", 0), nil,
			[]LiveLease{{ItemID: 8, Stage: Build}, {ItemID: 9, Stage: PlanTurnStage}}, nil, nil, nil, WaitNoSlot},
		{"stage limit", ctl, waiting(1, Triage, "P3", 0), nil, []LiveLease{{ItemID: 8, Stage: Triage}}, nil, nil, nil, "limit triage:1"},
		{"resource limit", ctl, waiting(1, Build, "P3", 0, "voice"), nil, []LiveLease{{ItemID: 8, Stage: Review, Resources: []string{"voice"}}}, nil, nil, nil, "limit voice:1"},
		{"resource limit before stage limit", ctl, waiting(1, Triage, "P3", 0, "emulator"), nil,
			[]LiveLease{{ItemID: 8, Stage: Triage, Resources: []string{"emulator"}}}, nil, nil, nil, "limit emulator:1"},
		// Land runs no agent: it needs none of the item's resources and takes no agent slot.
		{"land ignores its resources' limits", ctl, waiting(1, Land, "P3", 0, "emulator"), nil,
			[]LiveLease{{ItemID: 8, Stage: Build, Resources: []string{"emulator"}}}, nil, nil, nil, WaitReady},
		// Land's default limit is 3 (#213): two lands running leave a lane, three fill it.
		{"land under its stage limit", ctl, waiting(1, Land, "P3", 0), nil,
			[]LiveLease{{ItemID: 8, Stage: Land}, {ItemID: 9, Stage: Land}}, nil, nil, nil, WaitReady},
		{"land's own stage limit", ctl, waiting(1, Land, "P3", 0), nil,
			[]LiveLease{{ItemID: 8, Stage: Land}, {ItemID: 9, Stage: Land}, {ItemID: 10, Stage: Land}}, nil, nil, nil, "limit land:3"},
		{"land with every agent slot busy", two, waiting(1, Land, "P3", 0), nil,
			[]LiveLease{{ItemID: 8, Stage: Build}, {ItemID: 9, Stage: Review}}, nil, nil, nil, WaitReady},
		{"a land lease takes no agent slot", two, waiting(1, Build, "P3", 0), nil,
			[]LiveLease{{ItemID: 8, Stage: Build}, {ItemID: 9, Stage: Land}}, nil, nil, nil, WaitReady},
		{"below the limit", ctl, waiting(1, Review, "P3", 0), nil, []LiveLease{{ItemID: 8, Stage: Review}}, nil, nil, nil, WaitReady},
		{"frozen repo", ctl, in(waiting(1, Build, "P3", 0), "o/r"), nil, nil, nil, frozen, nil, WaitFrozen},
		{"another repo is not frozen", ctl, in(waiting(1, Build, "P3", 0), "o/other"), nil, nil, nil, frozen, nil, WaitReady},
		{"dependency beats frozen", ctl, dep(in(waiting(1, Build, "P3", 0), "o/r"), 7), map[int64]Stage{}, nil, nil, frozen, nil, WaitDeps},
		{"frozen beats stopped", stopped, in(waiting(1, Build, "P3", 0), "o/r"), nil, nil, nil, frozen, nil, WaitFrozen},
		// The fix may take a slot over the active-agents number and its stage's limit (chooseFix).
		{"the fix is ready", ctl, in(waiting(5, Build, "P1", 0), "o/r"), nil, nil, nil, frozen, nil, WaitReady},
		{"the fix ignores a busy loop", two, in(waiting(5, Build, "P1", 0), "o/r"), nil,
			[]LiveLease{{ItemID: 8, Stage: Build}, {ItemID: 9, Stage: Build}}, nil, frozen, nil, WaitReady},
		{"the fix ignores its stage's limit", ctl, in(waiting(5, Triage, "P1", 0), "o/r"), nil, []LiveLease{{ItemID: 8, Stage: Triage}}, nil, frozen, nil, WaitReady},
		{"the fix waits for a stop", stopped, in(waiting(5, Build, "P1", 0), "o/r"), nil, nil, nil, frozen, nil, WaitStopped},
		{"the fix waits for 0 active agents", paused, in(waiting(5, Build, "P1", 0), "o/r"), nil, nil, nil, frozen, nil, WaitNoSlot},
		{"the fix waits for a resource limit", ctl, in(waiting(5, Build, "P1", 0, "voice"), "o/r"), nil,
			[]LiveLease{{ItemID: 8, Stage: Review, Resources: []string{"voice"}}}, nil, frozen, nil, "limit voice:1"},
		// The last free slots held for a Mac's work (SlotHolds): only an item needing a held
		// resource may take them; a frozen repo's fix takes a slot regardless (chooseFix).
		{"slot held for macos", two, waiting(1, Build, "P3", 0), nil, []LiveLease{{ItemID: 8, Stage: Build}}, nil, nil, macHold, WaitNoSlot},
		{"the held slot's own work", two, waiting(1, Build, "P3", 0, "macos"), nil, []LiveLease{{ItemID: 8, Stage: Build}}, nil, nil, macHold, WaitReady},
		{"more free slots than held", two, waiting(1, Build, "P3", 0), nil, nil, nil, nil, macHold, WaitReady},
		{"a limit before the held slot", two, waiting(1, Triage, "P3", 0), nil, []LiveLease{{ItemID: 8, Stage: Triage}}, nil, nil, macHold, "limit triage:1"},
		{"land ignores a held slot", two, waiting(1, Land, "P3", 0), nil, []LiveLease{{ItemID: 8, Stage: Build}}, nil, nil, macHold, WaitReady},
		// A running regression (#82) is a live lease: it takes an agent slot and its suite's resources.
		{"a regression takes the last slot", two, waiting(1, Build, "P3", 0), nil,
			[]LiveLease{{ItemID: 8, Stage: Build}, {Stage: RegressionStage}}, nil, nil, nil, WaitNoSlot},
		{"a regression holds its resources", ctl, waiting(1, Build, "P3", 0, "voice"), nil,
			[]LiveLease{{Stage: RegressionStage, Resources: []string{"voice"}}}, nil, nil, nil, "limit voice:1"},
		{"the fix ignores a held slot", two, in(waiting(5, Build, "P1", 0), "o/r"), nil, []LiveLease{{ItemID: 8, Stage: Build}}, nil, frozen, macHold, WaitReady},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := WaitReason(c.ctl, normal, c.it, c.deps, c.live, c.unservable, c.frozen, c.holds, nil); got != c.want {
				t.Fatalf("WaitReason = %q, want %q", got, c.want)
			}
		})
	}
}

// Under claude's usage pace (ChooseWorkPaced) an item claude may not take now waits on it, unless
// a runner with qwen would take the build instead.
func TestWaitReasonPaced(t *testing.T) {
	ctl := DefaultControl()
	slow := Pace{Level: PaceSlow, Agents: 2}
	paused := Pace{Level: PacePaused}
	sized := func(it Item, size string) Item { it.Size = size; return it }
	qwen := []string{BuilderQwen}
	twoClaude := []LiveLease{{ItemID: 8, Stage: Build, Claude: true}, {ItemID: 9, Stage: Review, Claude: true}}
	cases := []struct {
		name     string
		pace     Pace
		it       Item
		live     []LiveLease
		frozen   Frozen
		builders []string
		want     string
	}{
		{"a P3 size-M build under a slow pace", slow, sized(waiting(1, Build, "P3", 0), "M"), nil, nil, nil, WaitPaced},
		{"the same build with a qwen runner", slow, sized(waiting(1, Build, "P3", 0), "M"), nil, nil, qwen, WaitReady},
		{"a P1 build under a slow pace", slow, sized(waiting(1, Build, "P1", 0), "L"), nil, nil, nil, WaitReady},
		{"a size-S review under a slow pace", slow, sized(waiting(1, Review, "P3", 0), "S"), nil, nil, nil, WaitReady},
		{"triage under a slow pace", slow, waiting(1, Triage, "P3", 0), nil, nil, nil, WaitReady},
		{"a size-L P3 build is not routed", slow, sized(waiting(1, Build, "P3", 0), "L"), nil, nil, qwen, WaitPaced},
		{"a P1 build once the slow pace's runs are taken", slow, sized(waiting(1, Build, "P1", 0), "S"), twoClaude, nil, nil, WaitPaced},
		{"a P1 build under a pause", paused, sized(waiting(1, Build, "P1", 0), "S"), nil, nil, nil, WaitPaused},
		{"a qwen build under a pause", paused, sized(Item{ID: 1, Stage: Build, Priority: "P3", Builder: BuilderQwen, CreatedAt: t0}, "M"), nil, nil, nil, WaitReady},
		{"a routed build under a pause", paused, sized(waiting(1, Build, "P3", 0), "S"), nil, nil, qwen, WaitReady},
		{"land under a pause", paused, waiting(1, Land, "P3", 0), nil, nil, nil, WaitReady},
		{"a frozen repo's fix under a pause", paused, Item{ID: 5, Stage: Build, Priority: "P1", Repo: "o/r", CreatedAt: t0}, nil, Frozen{"o/r": 5}, nil, WaitPaused},
		{"no free slot comes before the pace", Pace{Level: PaceSlow, Agents: 1}, sized(waiting(1, Build, "P3", 0), "M"),
			[]LiveLease{{ItemID: 6, Stage: Build, Claude: true}, {ItemID: 7, Stage: Build}, {ItemID: 8, Stage: Build}, {ItemID: 9, Stage: Build}}, nil, nil, WaitNoSlot},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := WaitReason(ctl, c.pace, c.it, nil, c.live, nil, c.frozen, nil, c.builders); got != c.want {
				t.Fatalf("WaitReason = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRunnerBuilders(t *testing.T) {
	now := t0
	runners := []RunnerSeen{
		{Name: "a", Builders: []string{BuilderQwen}, LastSeen: now.Add(-RunnerSeenWindow - time.Second)},
		{Name: "b", LastSeen: now},
	}
	if got := RunnerBuilders(runners, now); len(got) != 0 {
		t.Fatalf("a quiet runner's builders count: %v", got)
	}
	runners[0].LastSeen = now
	if got := RunnerBuilders(runners, now); len(got) != 1 || got[0] != BuilderQwen {
		t.Fatalf("RunnerBuilders = %v, want [qwen]", got)
	}
}

// ChooseWork skips exactly the items WaitReason says wait (deps, limits, a frozen repo and its
// fix's exemptions, a full loop, the held slots): the two share the checks.
func TestWaitReasonAgreesWithChoose(t *testing.T) {
	full := DefaultControl()
	full.ActiveAgents = 2
	req := ClaimRequest{Runner: "r", Agent: "a", Capabilities: Resources}
	live := []LiveLease{{ItemID: 8, Stage: Triage}, {ItemID: 9, Stage: Build, Resources: []string{"voice"}}}
	deps := map[int64]Stage{7: Build, 6: Done}
	frozen := Frozen{"o/r": 11}
	items := []Item{
		{ID: 10, Stage: Build, Priority: "P1", Repo: "o/r", CreatedAt: t0},
		{ID: 11, Stage: Triage, Priority: "P1", Repo: "o/r", CreatedAt: t0},
		{ID: 12, Stage: Build, Priority: "P1", Repo: "o/other", CreatedAt: t0},
		waiting(1, Triage, "P1", 0),
		waiting(2, Build, "P1", 0, "voice"),
		{ID: 3, Stage: Build, Priority: "P1", DependsOn: []int64{7}, CreatedAt: t0},
		{ID: 4, Stage: Build, Priority: "P1", DependsOn: []int64{6}, CreatedAt: t0.Add(time.Second)},
		waiting(5, Review, "P2", 0),
	}
	three := DefaultControl()
	three.ActiveAgents = 3 // one free slot, which macHold holds
	macHold := []SlotHold{{Resource: "macos", Runner: "mac", ItemID: 13}}
	items = append(items, waiting(13, Build, "P2", 0, "macos"), waiting(14, Land, "P2", 0, "voice"))
	// Under a pace (ChooseWorkPaced): P3 builds qwen may take or not, a size-S one, a qwen build.
	items = append(items,
		Item{ID: 15, Stage: Build, Priority: "P3", Size: "M", CreatedAt: t0},
		Item{ID: 16, Stage: Build, Priority: "P3", Size: "L", CreatedAt: t0},
		Item{ID: 17, Stage: Review, Priority: "P4", Size: "S", CreatedAt: t0},
		Item{ID: 18, Stage: Build, Priority: "P3", Size: "M", Builder: BuilderQwen, CreatedAt: t0})
	paces := []Pace{{Level: PaceNormal, Agents: 4}, {Level: PaceSlow, Agents: 2}, {Level: PaceSlow, Agents: 1}, {Level: PacePaused}}
	for _, ctl := range []Control{DefaultControl(), full, three} {
		for _, holds := range [][]SlotHold{nil, macHold} {
			for _, pace := range paces {
				for _, builders := range [][]string{nil, {BuilderQwen}} {
					for _, claude := range []bool{false, true} {
						lv := slices.Clone(live)
						for i := range lv {
							lv[i].Claude = claude
						}
						r := req
						r.Builders = builders
						for _, it := range items {
							w, _ := ChooseWorkPaced(ctl, pace, []Item{it}, deps, lv, nil, nil, nil, holds, r, frozen)
							chosen := w.Item != nil
							why := WaitReason(ctl, pace, it, deps, lv, nil, frozen, holds, builders)
							if chosen != (why == WaitReady) {
								t.Errorf("%d agents, %d held, pace %d/%d, builders %v, claude %v, item %d: ChooseWork %v, WaitReason %q",
									ctl.ActiveAgents, len(holds), pace.Level, pace.Agents, builders, claude, it.ID, chosen, why)
							}
						}
					}
				}
			}
		}
	}
}

func TestDistOf(t *testing.T) {
	cases := []struct {
		in   []time.Duration
		want Dist
	}{
		{nil, Dist{}},
		{[]time.Duration{5 * time.Second}, Dist{Count: 1, MedianSeconds: 5, P90Seconds: 5}},
		{[]time.Duration{4 * time.Second, 1 * time.Second, 3 * time.Second, 2 * time.Second}, Dist{Count: 4, MedianSeconds: 2, P90Seconds: 4}},
		// Nearest rank: the median of 1..10 is the 5th, p90 the 9th.
		{secs(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), Dist{Count: 10, MedianSeconds: 5, P90Seconds: 9}},
		{secs(10, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1), Dist{Count: 11, MedianSeconds: 1, P90Seconds: 1}},
	}
	for _, c := range cases {
		if got := DistOf(c.in); got != c.want {
			t.Errorf("DistOf(%v) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func secs(n ...int) []time.Duration {
	out := make([]time.Duration, len(n))
	for i, s := range n {
		out[i] = time.Duration(s) * time.Second
	}
	return out
}
