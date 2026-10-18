package pipeline

import (
	"slices"
	"strings"
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
		// Land (CI, merge, hold) needs none of the item's resources (#209): any runner that takes land
		// lands it, past the resource limits; a Mac runner (only_resources) takes no land.
		{"land: the dev box lands a macos item", ctl, []Item{waiting(1, Land, "P1", 0, "macos")}, nil, allCaps, 1},
		{"land: past the resource limits", ctl, []Item{waiting(1, Land, "P1", 0, "voice")},
			[]LiveLease{{ItemID: 8, Stage: Build, Resources: []string{"voice"}}}, allCaps, 1},
		{"land: still its stage limit", ctl, []Item{waiting(1, Land, "P1", 0, "macos")}, []LiveLease{{ItemID: 8, Stage: Land}}, allCaps, 0},
		{"land: build of a macos item still needs macos", ctl, []Item{waiting(1, Build, "P1", 0, "macos"), waiting(2, Review, "P1", 0, "macos")}, nil, allCaps, 0},
		{"land: only_resources takes no land", ctl, []Item{waiting(1, Land, "P1", 0, "macos")}, nil,
			ClaimRequest{Runner: "mbp", Agent: "mbp-1", Capabilities: []string{"macos"}, OnlyResources: []string{"macos"}}, 0},
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
			got, ok := Choose(tc.ctl, tc.items, nil, tc.live, tc.req, nil)
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

// The last free agent slot is held for an only_resources runner (a Mac's) while it is around and
// has an item it can take now, unless the claim takes that item itself (#192).
func TestSlotHold(t *testing.T) {
	two := DefaultControl()
	two.ActiveAgents = 2
	three := DefaultControl()
	three.ActiveAgents = 3
	mac1 := DefaultControl()
	mac1.ActiveAgents = 3
	mac1.Limits = map[string]int{"macos": 1}
	devbox := ClaimRequest{Runner: "devbox", Agent: "devbox-1", Capabilities: []string{"gpu", "voice", "emulator"}}
	macReq := ClaimRequest{Runner: "mbp", Agent: "mbp-1", Capabilities: []string{"macos"}, OnlyResources: []string{"macos"}}
	mbp := RunnerSeen{Name: "mbp", Capabilities: []string{"macos"}, OnlyResources: []string{"macos"}, PollSeconds: 30, LastSeen: t0.Add(-10 * time.Second)}
	iosBox := RunnerSeen{Name: "mini", Capabilities: []string{"ios"}, OnlyResources: []string{"ios"}, LastSeen: t0.Add(-10 * time.Second)}
	quiet := mbp
	quiet.LastSeen = t0.Add(-61 * time.Second) // more than 2 × 30 s ago
	fast := mbp
	fast.PollSeconds, fast.LastSeen = 5, t0.Add(-11*time.Second)
	macBR := mbp // the runbook's Mac: build and review, landing stays on the dev box
	macBR.Stages = []Stage{Build, Review}
	devboxSeen := RunnerSeen{Name: "devbox", Capabilities: []string{"gpu"}, LastSeen: t0}
	oneLive := []LiveLease{{ItemID: 9, Stage: Build}}
	macItem := waiting(2, Build, "P4", 0, "macos")
	plain := waiting(1, Build, "P1", time.Hour)
	deps := map[int64]Stage{7: Build}
	blocked := macItem
	blocked.DependsOn = []int64{7}

	cases := []struct {
		name    string
		ctl     Control
		items   []Item
		live    []LiveLease
		runners []RunnerSeen
		req     ClaimRequest
		holds   []string // the held resources
		want    int64    // the item the claim takes, 0: nothing
	}{
		{"the dev box is refused the last slot", two, []Item{plain, macItem}, oneLive, []RunnerSeen{mbp, devboxSeen}, devbox, []string{"macos"}, 0},
		{"a dev box with macos takes the held item", two, []Item{plain, macItem}, oneLive, []RunnerSeen{mbp},
			ClaimRequest{Runner: "devbox", Agent: "devbox-1", Capabilities: []string{"gpu", "macos"}}, []string{"macos"}, 2},
		{"the Mac takes it", two, []Item{plain, macItem}, oneLive, []RunnerSeen{mbp, devboxSeen}, macReq, []string{"macos"}, 2},
		{"(a) the Mac runner has gone quiet", two, []Item{plain, macItem}, oneLive, []RunnerSeen{quiet}, devbox, nil, 1},
		{"(a) the window is the runner's own poll interval", two, []Item{plain, macItem}, oneLive, []RunnerSeen{fast}, devbox, nil, 1},
		{"(a) a runner without only_resources holds nothing", two, []Item{plain, macItem}, oneLive,
			[]RunnerSeen{{Name: "both", Capabilities: []string{"macos", "gpu"}, LastSeen: t0}}, devbox, nil, 1},
		{"(b) the macos item is in land and the Mac takes build and review", two, []Item{plain, waiting(2, Land, "P4", 0, "macos")}, oneLive, []RunnerSeen{macBR, devboxSeen}, devbox, nil, 1},
		{"(b) the macos item is in land and the Mac takes every stage", two, []Item{plain, waiting(2, Land, "P4", 0, "macos")}, oneLive, []RunnerSeen{mbp, devboxSeen}, devbox, nil, 1},
		{"(c) landing a macos item is not the held work", two, []Item{macItem, waiting(3, Land, "P1", 0, "macos")}, oneLive, []RunnerSeen{mbp, devboxSeen}, devbox, []string{"macos"}, 0},
		{"(b) the macos item is in triage and the Mac takes build and review", two, []Item{plain, waiting(2, Triage, "P4", 0, "macos")}, oneLive, []RunnerSeen{macBR, devboxSeen}, devbox, nil, 1},
		{"(b) the macos item is in one of the Mac's stages", two, []Item{plain, macItem}, oneLive, []RunnerSeen{macBR, devboxSeen}, devbox, []string{"macos"}, 0},
		{"(b) no macos item", two, []Item{plain}, oneLive, []RunnerSeen{mbp}, devbox, nil, 1},
		{"(b) the macos item waits on a dependency", two, []Item{plain, blocked}, oneLive, []RunnerSeen{mbp}, devbox, nil, 1},
		{"(b) the macos item is leased", two, []Item{plain, macItem}, []LiveLease{{ItemID: 2, Stage: Build, Resources: []string{"macos"}}}, []RunnerSeen{mbp}, devbox, nil, 1},
		{"(b) at the macos limit", mac1, []Item{plain, waiting(3, Build, "P4", 0, "macos")},
			[]LiveLease{{ItemID: 2, Stage: Build, Resources: []string{"macos"}}, {ItemID: 9, Stage: Build}}, []RunnerSeen{mbp}, devbox, nil, 1},
		{"(c) a claimant whose best item needs macos", two, []Item{waiting(1, Build, "P5", 0), macItem}, oneLive, []RunnerSeen{mbp},
			ClaimRequest{Runner: "big", Agent: "big-1", Capabilities: []string{"macos"}}, []string{"macos"}, 2},
		{"not the last slot", three, []Item{plain, macItem}, oneLive, []RunnerSeen{mbp}, devbox, []string{"macos"}, 1},
		{"no free slot holds nothing", two, []Item{plain, macItem}, []LiveLease{{ItemID: 8, Stage: Build}, {ItemID: 9, Stage: Build}}, []RunnerSeen{mbp}, devbox, nil, 0},
		{"two resources hold a slot each", three, []Item{plain, macItem, waiting(3, Build, "P4", 0, "ios")}, oneLive,
			[]RunnerSeen{mbp, iosBox}, devbox, []string{"ios", "macos"}, 0},
		{"two resources, the ios runner takes its own", three, []Item{plain, macItem, waiting(3, Build, "P4", 0, "ios")}, oneLive,
			[]RunnerSeen{mbp, iosBox}, ClaimRequest{Runner: "mini", Agent: "mini-1", Capabilities: []string{"ios"}, OnlyResources: []string{"ios"}}, []string{"ios", "macos"}, 3},
		{"two resources, one free slot holds one", two, []Item{plain, macItem, waiting(3, Build, "P4", 0, "ios")}, oneLive,
			[]RunnerSeen{mbp, iosBox}, devbox, []string{"ios"}, 0},
		{"one runner for two resources holds a slot each", three, []Item{plain, macItem, waiting(3, Build, "P4", 0, "ios")}, oneLive,
			[]RunnerSeen{{Name: "mac", Capabilities: []string{"ios", "macos"}, OnlyResources: []string{"ios", "macos"}, LastSeen: t0}}, devbox, []string{"ios", "macos"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			holds := SlotHolds(tc.ctl, tc.items, deps, tc.live, tc.runners, nil, t0)
			var got []string
			for _, h := range holds {
				got = append(got, h.Resource)
			}
			if !slices.Equal(got, tc.holds) {
				t.Fatalf("holds %+v, want %v", holds, tc.holds)
			}
			w, ok := ChooseWork(tc.ctl, tc.items, deps, tc.live, nil, nil, holds, tc.req, nil)
			var id int64
			if ok {
				id = w.Item.ID
			}
			if id != tc.want {
				t.Fatalf("claimed #%d (ok %v, held %q), want #%d", id, ok, w.Held, tc.want)
			}
			if tc.want == 0 && len(tc.holds) > 0 && !strings.HasSuffix(w.Held, " held for "+strings.Join(tc.holds, ", ")+" work") {
				t.Fatalf("reason %q", w.Held)
			}
		})
	}
}

// A macos item in a frozen repo can't be claimed, so it holds nothing; the repo's fix still does.
func TestSlotHoldFrozen(t *testing.T) {
	two := DefaultControl()
	two.ActiveAgents = 2
	mbp := RunnerSeen{Name: "mbp", Capabilities: []string{"macos"}, OnlyResources: []string{"macos"}, LastSeen: t0}
	live := []LiveLease{{ItemID: 9, Stage: Build}}
	macItem := waiting(2, Build, "P4", 0, "macos")
	macItem.Repo = "o/mac"
	if h := SlotHolds(two, []Item{macItem}, nil, live, []RunnerSeen{mbp}, Frozen{"o/mac": 5}, t0); len(h) != 0 {
		t.Fatalf("frozen: holds %+v, want none", h)
	}
	if h := SlotHolds(two, []Item{macItem}, nil, live, []RunnerSeen{mbp}, Frozen{"o/mac": 2}, t0); len(h) != 1 || h[0].ItemID != 2 {
		t.Fatalf("the fix: holds %+v, want macos #2", h)
	}
}

// A held slot is held from turns too: a plan or orchestrate turn takes a slot like an item.
func TestSlotHoldTurns(t *testing.T) {
	two := DefaultControl()
	two.ActiveAgents = 2
	holds := []SlotHold{{Resource: "macos", Runner: "mbp", ItemID: 2}}
	req := ClaimRequest{Runner: "devbox", Agent: "a", Kinds: []string{WorkItem, WorkPlan, WorkOrchestrate}}
	turns := []TurnCandidate{{TurnID: 5, PlanID: 5, Priority: "P1", CreatedAt: t0}}
	if w, ok := ChooseWork(two, nil, nil, []LiveLease{{ItemID: 9, Stage: Build}}, turns, &OrchCandidate{}, holds, req, nil); ok {
		t.Fatalf("got %+v, want nothing", w)
	}
}
