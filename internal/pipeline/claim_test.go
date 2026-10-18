package pipeline

import (
	"testing"
	"time"
)

func waiting(id int64, stage Stage, pri string, age time.Duration, res ...string) Item {
	return Item{ID: id, Title: "t", Stage: stage, Priority: pri, Resources: res, CreatedAt: t0.Add(-age)}
}

func TestChoose(t *testing.T) {
	allCaps := ClaimRequest{Runner: "devbox", Agent: "agent-1", Capabilities: []string{"gpu", "voice", "emulator"}}
	ctl := DefaultControl()
	stopped := DefaultControl()
	stopped.Stopped = true
	paused := DefaultControl()
	paused.ActiveAgents = 0
	two := DefaultControl()
	two.ActiveAgents = 2
	gpu1 := DefaultControl()
	gpu1.Limits = map[string]int{"gpu": 1}

	cases := []struct {
		name  string
		ctl   Control
		items []Item
		live  []LiveLease
		req   ClaimRequest
		want  int64 // 0 = nothing
	}{
		{"empty queue", ctl, nil, nil, allCaps, 0},
		{"stage filter skips other stages", ctl, []Item{waiting(1, Triage, "P1", time.Hour), waiting(2, Build, "P5", 0)}, nil,
			ClaimRequest{Runner: "r", Agent: "a", Stages: []Stage{Build}}, 2},
		{"stage filter with nothing in it", ctl, []Item{waiting(1, Triage, "P1", 0), waiting(2, Review, "P1", 0)}, nil,
			ClaimRequest{Runner: "r", Agent: "a", Stages: []Stage{Build}}, 0},
		{"one triage item", ctl, []Item{waiting(1, Triage, "P3", time.Hour)}, nil, allCaps, 1},

		// 1. control
		{"stopped", stopped, []Item{waiting(1, Triage, "P3", 0)}, nil, allCaps, 0},
		{"active agents 0", paused, []Item{waiting(1, Triage, "P3", 0)}, nil, allCaps, 0},
		{"live leases at active agents", two, []Item{waiting(1, Build, "P3", 0)},
			[]LiveLease{{ItemID: 8, Stage: Build}, {ItemID: 9, Stage: Build}}, allCaps, 0},
		{"live leases below active agents", two, []Item{waiting(1, Build, "P3", 0)},
			[]LiveLease{{ItemID: 8, Stage: Build}}, allCaps, 1},

		// 2. eligibility
		{"not a work stage", ctl, []Item{waiting(1, NeedsYou, "P1", 0), waiting(2, Held, "P1", 0), waiting(3, Done, "P1", 0), waiting(4, Closed, "P1", 0), waiting(5, QA, "P1", 0)}, nil, allCaps, 0},
		{"resources not a subset of caps", ctl, []Item{waiting(1, Build, "P1", 0, "macos")}, nil, allCaps, 0},
		{"resources subset of caps", ctl, []Item{waiting(1, Build, "P1", 0, "gpu", "voice")}, nil, allCaps, 1},
		{"no caps still gets triage", ctl, []Item{waiting(1, Build, "P1", 0, "gpu"), waiting(2, Triage, "P5", 0)}, nil, ClaimRequest{Runner: "r", Agent: "a"}, 2},
		{"no caps gets a build with no resources", ctl, []Item{waiting(1, Build, "P3", 0)}, nil, ClaimRequest{Runner: "r", Agent: "a"}, 1},
		// A Mac runner (capabilities and only_resources [macos]) takes the items that need macos
		// and leaves everything else to the dev box, whatever its priority.
		{"only: skips an item without the resource", ctl, []Item{waiting(1, Build, "P1", 0), waiting(2, Build, "P5", 0, "macos")}, nil,
			ClaimRequest{Runner: "mbp", Agent: "mbp-1", Capabilities: []string{"macos"}, OnlyResources: []string{"macos"}}, 2},
		{"only: nothing that needs it", ctl, []Item{waiting(1, Build, "P1", 0), waiting(2, Triage, "P1", 0), waiting(3, Review, "P1", 0, "gpu")}, nil,
			ClaimRequest{Runner: "mbp", Agent: "mbp-1", Capabilities: []string{"macos"}, OnlyResources: []string{"macos"}}, 0},
		{"only: one of several", ctl, []Item{waiting(1, Review, "P3", 0, "gpu", "macos")}, nil,
			ClaimRequest{Runner: "r", Agent: "a", Capabilities: []string{"macos", "gpu"}, OnlyResources: []string{"macos"}}, 1},
		{"already claimed is skipped", ctl, []Item{waiting(1, Build, "P1", 0), waiting(2, Build, "P2", 0)}, []LiveLease{{ItemID: 1, Stage: Build}}, allCaps, 2},

		// 3. per-resource limits
		{"gpu at limit", ctl, []Item{waiting(1, Build, "P1", 0, "gpu"), waiting(2, Build, "P5", 0)},
			[]LiveLease{{ItemID: 8, Stage: Build, Resources: []string{"gpu"}}, {ItemID: 9, Stage: Review, Resources: []string{"gpu", "voice"}}}, allCaps, 2},
		{"gpu below limit", ctl, []Item{waiting(1, Build, "P1", 0, "gpu")},
			[]LiveLease{{ItemID: 8, Stage: Build, Resources: []string{"gpu"}}}, allCaps, 1},
		{"voice at limit", ctl, []Item{waiting(1, Build, "P1", 0, "gpu", "voice")},
			[]LiveLease{{ItemID: 8, Stage: Build, Resources: []string{"voice"}}}, allCaps, 0},
		{"emulator at limit", ctl, []Item{waiting(1, Build, "P1", 0, "emulator")},
			[]LiveLease{{ItemID: 8, Stage: Build, Resources: []string{"emulator"}}}, allCaps, 0},
		{"limits from control", gpu1, []Item{waiting(1, Build, "P1", 0, "gpu")},
			[]LiveLease{{ItemID: 8, Stage: Build, Resources: []string{"gpu"}}}, allCaps, 0},
		{"unlimited resource", ctl, []Item{waiting(1, Build, "P1", 0, "macos")},
			[]LiveLease{{ItemID: 8, Stage: Build, Resources: []string{"macos"}}}, ClaimRequest{Runner: "mac", Agent: "a", Capabilities: []string{"macos"}}, 1},

		// 4. per-stage limits
		{"triage at limit", ctl, []Item{waiting(1, Triage, "P1", 0), waiting(2, Build, "P5", 0)},
			[]LiveLease{{ItemID: 8, Stage: Triage}}, allCaps, 2},
		{"review at limit", ctl, []Item{waiting(1, Review, "P1", 0)},
			[]LiveLease{{ItemID: 8, Stage: Review}, {ItemID: 9, Stage: Review}}, allCaps, 0},
		{"review below limit", ctl, []Item{waiting(1, Review, "P1", 0)},
			[]LiveLease{{ItemID: 8, Stage: Review}}, allCaps, 1},
		{"land at limit", ctl, []Item{waiting(1, Land, "P1", 0)},
			[]LiveLease{{ItemID: 8, Stage: Land}}, allCaps, 0},
		{"build limited only by active agents", ctl, []Item{waiting(1, Build, "P1", 0)},
			[]LiveLease{{ItemID: 7, Stage: Build}, {ItemID: 8, Stage: Build}, {ItemID: 9, Stage: Build}}, allCaps, 1},

		// 5. review independence
		{"builder cannot review", ctl, []Item{{ID: 1, Stage: Review, Priority: "P1", BuilderAgent: "agent-1", CreatedAt: t0}, waiting(2, Build, "P5", 0)}, nil, allCaps, 2},
		{"another agent can review", ctl, []Item{{ID: 1, Stage: Review, Priority: "P1", BuilderAgent: "agent-2", CreatedAt: t0}}, nil, allCaps, 1},
		{"builder may build again", ctl, []Item{{ID: 1, Stage: Build, Priority: "P1", BuilderAgent: "agent-1", CreatedAt: t0}}, nil, allCaps, 1},

		// 6. order
		{"priority first", ctl, []Item{waiting(1, Land, "P3", time.Hour), waiting(2, Triage, "P2", 0)}, nil, allCaps, 2},
		{"P1 before P5", ctl, []Item{waiting(1, Build, "P5", 48*time.Hour), waiting(2, Build, "P1", 0)}, nil, allCaps, 2},
		{"then stage: land before review", ctl, []Item{waiting(1, Review, "P3", time.Hour), waiting(2, Land, "P3", 0)}, nil, allCaps, 2},
		{"then stage: review before build", ctl, []Item{waiting(1, Build, "P3", time.Hour), waiting(2, Review, "P3", 0)}, nil, allCaps, 2},
		{"then stage: build before triage", ctl, []Item{waiting(1, Triage, "P3", time.Hour), waiting(2, Build, "P3", 0)}, nil, allCaps, 2},
		{"then oldest", ctl, []Item{waiting(1, Build, "P3", time.Minute), waiting(2, Build, "P3", time.Hour)}, nil, allCaps, 2},
		{"then id", ctl, []Item{waiting(3, Build, "P3", time.Hour), waiting(2, Build, "P3", time.Hour)}, nil, allCaps, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Choose(tc.ctl, tc.items, nil, tc.live, tc.req)
			if tc.want == 0 {
				if ok {
					t.Fatalf("want nothing, got item %d", got.ID)
				}
				return
			}
			if !ok || got.ID != tc.want {
				t.Fatalf("want item %d, got %d (ok=%v)", tc.want, got.ID, ok)
			}
		})
	}
}

func TestJudgeLease(t *testing.T) {
	maxRun := 45 * time.Minute
	cases := []struct {
		name             string
		claimed, expires time.Time
		now              time.Time
		want             LeaseVerdict
	}{
		{"fresh", t0, t0.Add(5 * time.Minute), t0.Add(time.Minute), LeaseLive},
		{"just before expiry", t0, t0.Add(5 * time.Minute), t0.Add(5*time.Minute - time.Nanosecond), LeaseLive},
		{"at expiry", t0, t0.Add(5 * time.Minute), t0.Add(5 * time.Minute), LeaseExpired},
		{"heartbeats kept it alive", t0, t0.Add(44 * time.Minute), t0.Add(40 * time.Minute), LeaseLive},
		{"past max run", t0, t0.Add(50 * time.Minute), t0.Add(45 * time.Minute), LeaseOvertime},
		{"past max run and expired", t0, t0.Add(30 * time.Minute), t0.Add(46 * time.Minute), LeaseOvertime},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := JudgeLease(c.claimed, c.expires, c.now, maxRun); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}
