package pipeline

import (
	"testing"
	"time"
)

func ny(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no zone database")
	}
	return loc
}

func TestParseClock(t *testing.T) {
	for s, want := range map[string]int{"01:00": 60, "00:00": 0, "23:59": 23*60 + 59, "08:30": 510} {
		if got, err := ParseClock(s); err != nil || got != want {
			t.Errorf("ParseClock(%q) = %d, %v; want %d", s, got, err, want)
		}
	}
	for _, s := range []string{"", "1:00", "24:00", "01:60", "1am", "01:00:00"} {
		if _, err := ParseClock(s); err == nil {
			t.Errorf("ParseClock(%q) took it", s)
		}
	}
}

func TestNextAt(t *testing.T) {
	loc := ny(t)
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, loc) }
	cases := []struct {
		now  time.Time
		hhmm string
		want time.Time
	}{
		{at(19, 9, 0), "01:00", at(20, 1, 0)},  // in the day: tonight's (tomorrow's date)
		{at(20, 0, 30), "01:00", at(20, 1, 0)}, // after midnight: the coming one
		{at(20, 1, 0), "01:00", at(20, 1, 0)},  // at it
		{at(20, 1, 1), "01:00", at(21, 1, 0)},
		{at(19, 22, 0), "23:30", at(19, 23, 30)},
		{at(19, 9, 0), "bad", at(20, 1, 0)}, // the default
	}
	for _, tc := range cases {
		if got := NextAt(tc.now, loc, tc.hhmm); !got.Equal(tc.want) {
			t.Errorf("NextAt(%s, %s) = %s, want %s", tc.now, tc.hhmm, got, tc.want)
		}
	}
}

func TestMorningReport(t *testing.T) {
	loc := ny(t)
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, loc) }
	cases := []struct{ finished, want time.Time }{
		{at(20, 3, 10), at(20, 8, 30)}, // at night: the morning
		{at(19, 23, 0), at(20, 8, 30)}, // late evening: the next morning
		{at(20, 8, 30), at(20, 8, 30)}, // in the day: at once
		{at(20, 14, 0), at(20, 14, 0)}, // a run that waited for a quiet queue into the day
		{at(20, 19, 59), at(20, 19, 59)},
		{at(20, 20, 0), at(21, 8, 30)},
	}
	for _, tc := range cases {
		if got := MorningReport(tc.finished, loc); !got.Equal(tc.want) {
			t.Errorf("MorningReport(%s) = %s, want %s", tc.finished, got, tc.want)
		}
	}
}

func TestOvernightReady(t *testing.T) {
	one := t0
	cases := []struct {
		name            string
		quietSince, now time.Time
		want            bool
	}{
		{"before the set time, though quiet for hours", one.Add(-5 * time.Hour), one.Add(-time.Minute), false},
		{"at the set time, quiet for 10 minutes", one.Add(-OvernightQuiet), one, true},
		{"at the set time, quiet for 9 minutes", one.Add(-9 * time.Minute), one, false},
		{"quiet from 01:00: starts at 01:10 (the later of the two)", one, one.Add(OvernightQuiet), true},
		{"not quiet", time.Time{}, one.Add(time.Hour), false},
	}
	for _, tc := range cases {
		if got := OvernightReady(one, tc.quietSince, tc.now); got != tc.want {
			t.Errorf("%s: OvernightReady = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestQueueBusy(t *testing.T) {
	devbox := []RunnerSeen{{Name: "devbox", Capabilities: []string{"emulator"}, LastSeen: t0}}
	merged := map[int64]Stage{}
	dep := waiting(2, Build, "P3", 0)
	dep.DependsOn = []int64{9}
	cases := []struct {
		name   string
		items  []Item
		deps   map[int64]Stage
		live   []LiveLease
		turns  []TurnCandidate
		orch   bool
		frozen Frozen
		want   bool
	}{
		{"nothing at all", nil, nil, nil, nil, false, nil, false},
		{"a build lease is live", nil, nil, []LiveLease{{ItemID: 1, Stage: Build}}, nil, false, nil, true},
		{"a review lease is live", nil, nil, []LiveLease{{ItemID: 1, Stage: Review}}, nil, false, nil, true},
		{"a triage lease is live", nil, nil, []LiveLease{{ItemID: 1, Stage: Triage}}, nil, false, nil, true},
		{"a plan turn runs", nil, nil, []LiveLease{{Stage: PlanTurnStage}}, nil, false, nil, true},
		{"a land lease is not work for the night", nil, nil, []LiveLease{{ItemID: 1, Stage: Land}}, nil, false, nil, false},
		{"a manual regression run is not an agent", nil, nil, []LiveLease{{Stage: RegressionStage}}, nil, false, nil, false},
		{"an item waits in build", []Item{waiting(1, Build, "P3", 0)}, merged, nil, nil, false, nil, true},
		{"an item waits in triage", []Item{waiting(1, Triage, "P3", 0)}, merged, nil, nil, false, nil, true},
		{"an item waiting to land is not", []Item{waiting(1, Land, "P3", 0)}, merged, nil, nil, false, nil, false},
		{"an item waiting on a dependency is not", []Item{dep}, map[int64]Stage{9: Build}, nil, nil, false, nil, false},
		{"an item no runner can serve is not", []Item{waiting(1, Build, "P3", 0, "macos")}, merged, nil, nil, false, nil, false},
		{"an item a runner serves is", []Item{waiting(1, Build, "P3", 0, "emulator")}, merged, nil, nil, false, nil, true},
		{"an item of a frozen repo is not", []Item{{ID: 1, Stage: Build, Priority: "P3", Repo: "a/b"}}, merged, nil, nil, false,
			Frozen{"a/b": 99}, false},
		{"a queued plan turn", nil, nil, nil, []TurnCandidate{{TurnID: 1}}, false, nil, true},
		{"the queued orchestrate turn", nil, nil, nil, nil, true, nil, true},
	}
	for _, tc := range cases {
		if got := QueueBusy(DefaultControl(), Pace{}, tc.items, tc.deps, tc.live, tc.turns, tc.orch, tc.frozen, devbox, t0); (got != "") != tc.want {
			t.Errorf("%s: QueueBusy = %q, want busy %v", tc.name, got, tc.want)
		}
	}
}

// The claude pace counts (#317): what it holds back cannot be claimed, so it leaves the queue quiet
// for the regression, whose lanes are no claude runs. A slow pace's starved fallback (#316) still
// claims, so the queue stays busy then.
func TestQueueBusyPaced(t *testing.T) {
	ctl := DefaultControl()
	devbox := []RunnerSeen{{Name: "devbox", Capabilities: []string{"emulator"}, LastSeen: t0}}
	qwenBox := []RunnerSeen{{Name: "devbox", Capabilities: []string{"emulator"}, Builders: []string{BuilderQwen}, LastSeen: t0}}
	paused := Pace{Level: PacePaused}
	slow := Pace{Level: PaceSlow, Agents: 2}
	p2 := Item{ID: 1, Title: "t", Stage: Build, Priority: "P2", Size: "M", CreatedAt: t0}
	p1 := Item{ID: 2, Title: "t", Stage: Build, Priority: "P1", Size: "M", CreatedAt: t0}
	land := LiveLease{ItemID: 7, Stage: Land}
	claudeBuild := LiveLease{ItemID: 8, Stage: Build, Claude: true}
	cases := []struct {
		name    string
		pace    Pace
		items   []Item
		live    []LiveLease
		turns   []TurnCandidate
		orch    bool
		runners []RunnerSeen
		want    bool
	}{
		{"paused: ready claude items are held back", paused, []Item{p2, p1}, nil, nil, false, devbox, false},
		{"paused: a land lease is still no work", paused, []Item{p2}, []LiveLease{land}, nil, false, devbox, false},
		{"paused: a qwen-routable build is claimable", paused, []Item{{ID: 3, Stage: Build, Priority: "P3", Size: "M", CreatedAt: t0}}, nil, nil, false, qwenBox, true},
		{"paused: a plan turn waits", paused, nil, nil, []TurnCandidate{{TurnID: 1, Priority: "P1"}}, false, devbox, false},
		{"paused: the orchestrate turn waits", paused, nil, nil, nil, true, devbox, false},
		{"paused: a live claude run is busy", paused, nil, []LiveLease{claudeBuild}, nil, false, devbox, true},
		{"slow: an eligible item waits for its claim", slow, []Item{p2, p1}, nil, nil, false, devbox, true},
		{"slow: only ineligible items, the starved fallback claims", slow, []Item{p2}, nil, nil, false, devbox, true},
		{"slow: a P2 plan turn is held back", slow, nil, nil, []TurnCandidate{{TurnID: 1, Priority: "P2"}}, false, devbox, false},
		{"slow: a P1 plan turn is claimable", slow, nil, nil, []TurnCandidate{{TurnID: 1, Priority: "P1"}}, false, devbox, true},
	}
	for _, tc := range cases {
		if got := QueueBusy(ctl, tc.pace, tc.items, map[int64]Stage{}, tc.live, tc.turns, tc.orch, nil, tc.runners, t0); (got != "") != tc.want {
			t.Errorf("%s: QueueBusy = %q, want busy %v", tc.name, got, tc.want)
		}
	}
}

func TestQueueBusySaysWhy(t *testing.T) {
	ctl := DefaultControl()
	devbox := []RunnerSeen{{Name: "devbox", Capabilities: []string{"emulator"}, LastSeen: t0}}
	for _, tc := range []struct {
		name  string
		items []Item
		live  []LiveLease
		turns []TurnCandidate
		orch  bool
		want  string
	}{
		{"a lease", nil, []LiveLease{{ItemID: 4, Stage: Build}}, nil, false, "the build of #4 runs"},
		{"a plan turn runs", nil, []LiveLease{{Stage: PlanTurnStage}}, nil, false, "a plan turn runs"},
		{"an item", []Item{waiting(5, Review, "P2", 0)}, nil, nil, false, "#5 waits in review"},
		{"a turn", nil, nil, []TurnCandidate{{TurnID: 1, PlanID: 3, Priority: "P1"}}, false, "a turn of plan 3 is queued"},
		{"orchestrate", nil, nil, nil, true, "the orchestrate turn is queued"},
	} {
		if got := QueueBusy(ctl, Pace{}, tc.items, map[int64]Stage{}, tc.live, tc.turns, tc.orch, nil, devbox, t0); got != tc.want {
			t.Errorf("%s: QueueBusy = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestChooseWorkOvernightHolds(t *testing.T) {
	ctl := DefaultControl()
	req := ClaimRequest{Agent: "a", Capabilities: []string{"emulator", "gpu", "voice"}, Kinds: []string{WorkItem, WorkPlan, WorkOrchestrate, WorkRegression}}
	overnight := LiveLease{Stage: RegressionStage, Resources: SuiteResources(SuiteOvernight), Holds: true}
	manual := LiveLease{Stage: RegressionStage}
	ready := []Item{waiting(1, Build, "P1", time.Hour), waiting(2, Land, "P3", 0)}
	turns := []TurnCandidate{{TurnID: 5, PlanID: 1, Priority: "P1"}}
	orch := &OrchCandidate{TurnID: 9}

	// While it runs: no build, no plan turn, no orchestrate turn; land still goes on.
	w, ok := ChooseWork(ctl, ready, nil, []LiveLease{overnight}, turns, orch, nil, nil, req, nil)
	if !ok || w.Item == nil || w.Item.ID != 2 {
		t.Fatalf("while the overnight run runs, got %+v (ok %v), want only the land item #2", w, ok)
	}
	if w, ok := ChooseWork(ctl, ready[:1], nil, []LiveLease{overnight}, turns, orch, nil, nil, req, nil); ok {
		t.Fatalf("while the overnight run runs, an item in build was claimed: %+v", w)
	}
	// A frozen repo's fix waits too: the run has the machine.
	fix := Item{ID: 3, Stage: Build, Priority: "P1", Repo: "a/b"}
	if w, ok := ChooseWork(ctl, []Item{fix}, nil, []LiveLease{overnight}, nil, nil, nil, nil, req, Frozen{"a/b": 3}); ok {
		t.Fatalf("a frozen repo's fix was claimed during the overnight run: %+v", w)
	}
	// A manual run holds nothing: it is one agent slot.
	if w, ok := ChooseWork(ctl, ready[:1], nil, []LiveLease{manual}, nil, nil, nil, nil, req, nil); !ok || w.Item == nil || w.Item.ID != 1 {
		t.Fatalf("a manual run held the build: %+v (ok %v)", w, ok)
	}
	// The overnight candidate needs the voice lane's resources.
	cand := &RegCandidate{RunID: 4, Suite: SuiteOvernight, Resources: SuiteResources(SuiteOvernight)}
	noVoice := req
	noVoice.Capabilities = []string{"emulator"}
	if w, ok := ChooseWork(ctl, nil, nil, nil, nil, nil, cand, nil, noVoice, nil); ok {
		t.Fatalf("a runner without gpu and voice took the overnight run: %+v", w)
	}
	if w, ok := ChooseWork(ctl, nil, nil, nil, nil, nil, cand, nil, req, nil); !ok || w.Reg == nil || w.Reg.RunID != 4 {
		t.Fatalf("the overnight run was not claimed: %+v (ok %v)", w, ok)
	}
}

func TestParseRegressionTonight(t *testing.T) {
	cases := []struct {
		text   string
		on, ok bool
	}{
		{"regression tonight", true, true},
		{"Regression Tonight.", true, true},
		{"regression tonight off", false, true},
		{"regression tonight cancel", false, true},
		{"regression", false, false},
		{"regression tonight please fix the inbox", false, false},
	}
	for _, tc := range cases {
		on, ok := ParseRegressionTonight(tc.text)
		if on != tc.on || ok != tc.ok {
			t.Errorf("ParseRegressionTonight(%q) = %v, %v; want %v, %v", tc.text, on, ok, tc.on, tc.ok)
		}
	}
	if _, ok := ParseRegression("regression tonight"); ok {
		t.Error("ParseRegression took 'regression tonight' as a run now")
	}
}

func TestLastAt(t *testing.T) {
	loc := ny(t)
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct{ now, want string }{
		{"2026-10-21 07:00", "2026-10-21 07:00"},
		{"2026-10-21 06:59", "2026-10-20 07:00"},
		{"2026-10-21 23:00", "2026-10-21 07:00"},
		{"2026-11-01 08:00", "2026-11-01 07:00"}, // the day the clocks go back
	} {
		if got := LastAt(at(c.now), loc, "07:00"); !got.Equal(at(c.want)) {
			t.Errorf("LastAt(%s) = %s, want %s", c.now, got, c.want)
		}
	}
}
