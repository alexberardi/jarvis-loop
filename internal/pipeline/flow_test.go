package pipeline

import (
	"testing"
	"time"
)

func TestWaitReason(t *testing.T) {
	ctl := DefaultControl() // 4 agents, triage:1, review:2, land:1, gpu:2, voice:1, emulator:1
	stopped := DefaultControl()
	stopped.Stopped = true
	paused := DefaultControl()
	paused.ActiveAgents = 0
	two := DefaultControl()
	two.ActiveAgents = 2

	dep := func(it Item, ids ...int64) Item { it.DependsOn = ids; return it }
	qa := waiting(1, QA, "P3", 0)
	qa.AwaitingDeploy = true

	cases := []struct {
		name       string
		ctl        Control
		it         Item
		deps       map[int64]Stage
		live       []LiveLease
		unservable []string
		want       string
	}{
		{"needs you", ctl, waiting(1, NeedsYou, "P3", 0), nil, nil, nil, WaitNeedsYou},
		{"held", ctl, waiting(1, Held, "P3", 0), nil, nil, nil, WaitHeld},
		{"qa", ctl, waiting(1, QA, "P3", 0), nil, nil, nil, WaitQA},
		{"qa after deploy", ctl, qa, nil, nil, nil, WaitQA},
		{"done is not waiting", ctl, waiting(1, Done, "P3", 0), nil, nil, nil, ""},
		{"closed is not waiting", ctl, waiting(1, Closed, "P3", 0), nil, nil, nil, ""},
		{"claimed is not waiting", ctl, waiting(1, Build, "P3", 0), nil, []LiveLease{{ItemID: 1, Stage: Build}}, nil, ""},
		{"ready", ctl, waiting(1, Build, "P3", 0), nil, nil, nil, WaitReady},
		{"dependency not done", ctl, dep(waiting(1, Build, "P3", 0), 7), map[int64]Stage{7: QA}, nil, nil, WaitDeps},
		{"dependency done", ctl, dep(waiting(1, Build, "P3", 0), 7), map[int64]Stage{7: Done}, nil, nil, WaitReady},
		// The item's own blockers come before the loop-wide ones: once the loop resumes it would
		// still wait for them.
		{"dependency beats stopped", stopped, dep(waiting(1, Build, "P3", 0), 7), map[int64]Stage{}, nil, nil, WaitDeps},
		{"no runner for a resource", ctl, waiting(1, Build, "P3", 0, "macos"), nil, nil, []string{"macos"}, WaitNoRunner},
		{"stopped", stopped, waiting(1, Build, "P3", 0), nil, nil, nil, WaitStopped},
		{"active agents 0", paused, waiting(1, Build, "P3", 0), nil, nil, nil, WaitNoSlot},
		{"every slot busy", two, waiting(1, Build, "P3", 0), nil,
			[]LiveLease{{ItemID: 8, Stage: Build}, {ItemID: 9, Stage: PlanTurnStage}}, nil, WaitNoSlot},
		{"stage limit", ctl, waiting(1, Triage, "P3", 0), nil, []LiveLease{{ItemID: 8, Stage: Triage}}, nil, "limit triage:1"},
		{"resource limit", ctl, waiting(1, Build, "P3", 0, "voice"), nil, []LiveLease{{ItemID: 8, Stage: Review, Resources: []string{"voice"}}}, nil, "limit voice:1"},
		{"resource limit before stage limit", ctl, waiting(1, Land, "P3", 0, "emulator"), nil,
			[]LiveLease{{ItemID: 8, Stage: Land, Resources: []string{"emulator"}}}, nil, "limit emulator:1"},
		{"below the limit", ctl, waiting(1, Review, "P3", 0), nil, []LiveLease{{ItemID: 8, Stage: Review}}, nil, WaitReady},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := WaitReason(c.ctl, c.it, c.deps, c.live, c.unservable); got != c.want {
				t.Fatalf("WaitReason = %q, want %q", got, c.want)
			}
		})
	}
}

// Choose skips exactly the items WaitReason says wait on the item itself (deps, limits): the two
// share the checks.
func TestWaitReasonAgreesWithChoose(t *testing.T) {
	ctl := DefaultControl()
	req := ClaimRequest{Runner: "r", Agent: "a", Capabilities: Resources}
	live := []LiveLease{{ItemID: 8, Stage: Triage}, {ItemID: 9, Stage: Build, Resources: []string{"voice"}}}
	deps := map[int64]Stage{7: Build, 6: Done}
	items := []Item{
		waiting(1, Triage, "P1", 0),
		waiting(2, Build, "P1", 0, "voice"),
		{ID: 3, Stage: Build, Priority: "P1", DependsOn: []int64{7}, CreatedAt: t0},
		{ID: 4, Stage: Build, Priority: "P1", DependsOn: []int64{6}, CreatedAt: t0.Add(time.Second)},
		waiting(5, Review, "P2", 0),
	}
	for _, it := range items {
		_, chosen := Choose(ctl, []Item{it}, deps, live, req, nil)
		ready := WaitReason(ctl, it, deps, live, nil) == WaitReady
		if chosen != ready {
			t.Errorf("item %d: Choose %v, WaitReason %q", it.ID, chosen, WaitReason(ctl, it, deps, live, nil))
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
