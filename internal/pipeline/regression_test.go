package pipeline

import (
	"testing"
	"time"
)

func TestParseRegression(t *testing.T) {
	cases := []struct {
		text, suite string
		ok          bool
	}{
		{"regression", SuiteAll, true},
		{"  Regression  ", SuiteAll, true},
		{"regression web", SuiteWeb, true},
		{"regression MOBILE", SuiteMobile, true},
		{"regression all", SuiteAll, true},
		{"regression all.", SuiteAll, true},
		{"regression voice", "", false},
		{"regression in the inbox: the delete button is gone", "", false},
		{"run the regression", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		suite, ok := ParseRegression(tc.text)
		if suite != tc.suite || ok != tc.ok {
			t.Errorf("ParseRegression(%q) = %q, %v; want %q, %v", tc.text, suite, ok, tc.suite, tc.ok)
		}
	}
}

func TestSuiteResources(t *testing.T) {
	if r := SuiteResources(SuiteWeb); len(r) != 0 {
		t.Errorf("web needs %v, want nothing", r)
	}
	for _, s := range []string{SuiteMobile, SuiteAll} {
		if r := SuiteResources(s); len(r) != 1 || r[0] != "emulator" {
			t.Errorf("%s needs %v, want [emulator]", s, r)
		}
	}
}

func TestChooseWorkRegression(t *testing.T) {
	ctl := DefaultControl()
	two := DefaultControl()
	two.ActiveAgents = 2
	emuCaps := []string{"emulator"}
	all := ClaimRequest{Runner: "devbox", Agent: "a", Capabilities: emuCaps, Kinds: []string{WorkItem, WorkPlan, WorkOrchestrate, WorkRegression}}
	noEmu := all
	noEmu.Capabilities = nil
	web := &RegCandidate{RunID: 7, Suite: SuiteWeb, CreatedAt: t0}
	mobile := &RegCandidate{RunID: 8, Suite: SuiteMobile, Resources: []string{"emulator"}, CreatedAt: t0}
	running := LiveLease{Stage: RegressionStage}
	build := LiveLease{ItemID: 3, Stage: Build}
	emuBuild := LiveLease{ItemID: 4, Stage: Build, Resources: []string{"emulator"}}
	cases := []struct {
		name  string
		ctl   Control
		items []Item
		orch  *OrchCandidate
		reg   *RegCandidate
		live  []LiveLease
		req   ClaimRequest
		item  int64
		run   int64
		otrn  int64
	}{
		{"a queued run beats a P1 item", ctl, []Item{waiting(1, Land, "P1", time.Hour)}, nil, web, nil, all, 0, 7, 0},
		{"the orchestrate turn goes first", ctl, nil, &OrchCandidate{TurnID: 9}, web, nil, all, 0, 0, 9},
		{"one at a time", ctl, []Item{waiting(1, Build, "P3", 0)}, nil, web, []LiveLease{running}, all, 1, 0, 0},
		{"it counts as an agent", two, []Item{waiting(1, Build, "P3", 0)}, nil, nil, []LiveLease{build, running}, all, 0, 0, 0},
		{"it shares the active-agents limit", two, nil, nil, web, []LiveLease{build, {ItemID: 5, Stage: Review}}, all, 0, 0, 0},
		{"mobile needs the emulator capability", ctl, []Item{waiting(1, Build, "P3", 0)}, nil, mobile, nil, noEmu, 1, 0, 0},
		{"web needs none", ctl, nil, nil, web, nil, noEmu, 0, 7, 0},
		{"mobile waits for the emulator limit", ctl, []Item{waiting(1, Build, "P3", 0)}, nil, mobile, []LiveLease{emuBuild}, all, 1, 0, 0},
		{"mobile takes the free emulator", ctl, nil, nil, mobile, []LiveLease{build}, all, 0, 8, 0},
		{"not for a runner that does not ask", ctl, []Item{waiting(1, Build, "P3", 0)}, nil, web, nil,
			ClaimRequest{Agent: "a", Kinds: []string{WorkItem}}, 1, 0, 0},
		{"stopped", Control{Stopped: true, ActiveAgents: 4}, nil, nil, web, nil, all, 0, 0, 0},
		{"no agents", Control{ActiveAgents: 0}, nil, nil, web, nil, all, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, ok := ChooseWork(tc.ctl, tc.items, nil, tc.live, nil, tc.orch, tc.reg, nil, tc.req, nil)
			var gotItem, gotRun, gotOrch int64
			if ok && w.Item != nil {
				gotItem = w.Item.ID
			}
			if ok && w.Reg != nil {
				gotRun = w.Reg.RunID
			}
			if ok && w.Orch != nil {
				gotOrch = w.Orch.TurnID
			}
			want := tc.item != 0 || tc.run != 0 || tc.otrn != 0
			if ok != want || gotItem != tc.item || gotRun != tc.run || gotOrch != tc.otrn {
				t.Fatalf("got item %d run %d orchestrate %d (ok %v), want %d %d %d", gotItem, gotRun, gotOrch, ok, tc.item, tc.run, tc.otrn)
			}
		})
	}
}

func TestChooseWorkRegressionHeldSlot(t *testing.T) {
	// The last slot is held for emulator work: a mobile run is emulator work, a web run is not.
	two := DefaultControl()
	two.ActiveAgents = 2
	req := ClaimRequest{Agent: "a", Capabilities: []string{"emulator"}, Kinds: []string{WorkItem, WorkRegression}}
	holds := []SlotHold{{Resource: "emulator", Runner: "mac", ItemID: 4}}
	live := []LiveLease{{ItemID: 9, Stage: Build}}
	if w, ok := ChooseWork(two, nil, nil, live, nil, nil, &RegCandidate{RunID: 1, Suite: SuiteWeb}, holds, req, nil); ok {
		t.Fatalf("a web run took the held slot: %+v", w)
	}
	w, ok := ChooseWork(two, nil, nil, live, nil, nil, &RegCandidate{RunID: 2, Suite: SuiteMobile, Resources: []string{"emulator"}}, holds, req, nil)
	if !ok || w.Reg == nil || w.Reg.RunID != 2 {
		t.Fatalf("a mobile run did not take the emulator's held slot: %+v %v", w, ok)
	}
}

// #505: a run no runner seen lately can take (none seen, none claims regression runs, or none has
// every resource) is not scheduled nightly and expires unnoted.
func TestRegressionTaker(t *testing.T) {
	now := time.Date(2026, 10, 21, 7, 0, 0, 0, time.UTC)
	res := SuiteResources(SuiteOvernight)
	dev := RunnerSeen{Name: "dev", Capabilities: []string{"emulator", "gpu"}, Kinds: []string{WorkItem, WorkRegression}, LastSeen: now}
	full := RunnerSeen{Name: "full", Capabilities: []string{"emulator", "gpu", "voice"}, Kinds: []string{WorkItem, WorkRegression}, LastSeen: now}
	noKind := RunnerSeen{Name: "nokind", Capabilities: []string{"emulator", "gpu", "voice"}, LastSeen: now}
	stale := full
	stale.LastSeen = now.Add(-RunnerSeenWindow - time.Second)
	for _, c := range []struct {
		name    string
		runners []RunnerSeen
		want    bool
	}{
		{"none seen", nil, false},
		{"one takes it", []RunnerSeen{dev, full}, true},
		{"lacks a resource", []RunnerSeen{dev}, false},
		{"does not claim regression runs", []RunnerSeen{noKind}, false},
		{"the one that could is stale", []RunnerSeen{dev, stale}, false},
	} {
		if got := RegressionTaker(res, c.runners, now); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
