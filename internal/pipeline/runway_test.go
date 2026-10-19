package pipeline

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

var testDurations = map[Stage]time.Duration{Triage: 10 * time.Minute, Build: time.Hour, Review: 20 * time.Minute, Land: 10 * time.Minute}

func TestRunwayEveryChainWaitsOnOneHeldItem(t *testing.T) {
	items := map[int64]RunwayNode{
		1: {Stage: Held, Hold: true, Priority: "P2"},
		2: {Stage: Build, DependsOn: []int64{1}, Priority: "P2"},
		3: {Stage: Build, DependsOn: []int64{1}, Priority: "P2"},
		4: {Stage: Triage, DependsOn: []int64{2}, Priority: "P2"},
	}
	r := ComputeRunway(items, testDurations)
	if r.Hours != 0 || len(r.Runnable) != 0 {
		t.Fatalf("runway = %.2f h, runnable %v; want 0, none", r.Hours, r.Runnable)
	}
	want := []Unblocker{{ItemID: 1, Action: UnblockApprove, Frees: 4, Priority: "P2"}}
	if !slices.Equal(r.Unblockers, want) {
		t.Fatalf("unblockers = %+v, want %+v", r.Unblockers, want)
	}
}

func TestRunwayPlentyOfClaimableWork(t *testing.T) {
	items := map[int64]RunwayNode{
		1:  {Stage: Build, Priority: "P2"},
		2:  {Stage: Build, Priority: "P2"},
		3:  {Stage: Review, Claimed: true, Priority: "P2"},
		4:  {Stage: Build, DependsOn: []int64{3}, Priority: "P2"}, // met by work in flight
		5:  {Stage: Build, DependsOn: []int64{9}, Priority: "P2"}, // 9 is in qa: merged
		9:  {Stage: QA},
		10: {Stage: Done},
	}
	r := ComputeRunway(items, testDurations)
	if want := []int64{1, 2, 3, 4, 5}; !slices.Equal(r.Runnable, want) {
		t.Fatalf("runnable = %v, want %v", r.Runnable, want)
	}
	// Build: build+review+land = 1.5 h each (x4 = 6 h), the review: 0.5 h.
	if r.Hours < 6.49 || r.Hours > 6.51 {
		t.Fatalf("hours = %.2f, want 6.5", r.Hours)
	}
	if len(r.Unblockers) != 0 {
		t.Fatalf("unblockers = %+v, want none", r.Unblockers)
	}
}

func TestRunwayRanksByFreesThenPriority(t *testing.T) {
	items := map[int64]RunwayNode{
		1: {Stage: NeedsYou, Priority: "P2"},              // answer: frees itself
		2: {Stage: NeedsYou, Trust: true, Priority: "P3"}, // trust: frees itself + 3 dependents
		3: {Stage: Build, DependsOn: []int64{2}, Priority: "P2"},
		4: {Stage: Build, DependsOn: []int64{2}, Priority: "P2"},
		5: {Stage: Build, DependsOn: []int64{3}, Priority: "P2"},
		6: {Stage: Held, Priority: "P1"},                            // approve: frees itself
		7: {Stage: Build, DependsOn: []int64{6, 1}, Priority: "P2"}, // needs both: freed by neither alone
		8: {Stage: Build, Unservable: true, Priority: "P2"},         // no runner: not runnable, not the user's button
	}
	r := ComputeRunway(items, testDurations)
	want := []Unblocker{
		{ItemID: 2, Action: UnblockTrust, Frees: 4, Priority: "P3"},
		{ItemID: 6, Action: UnblockApprove, Frees: 1, Priority: "P1"},
		{ItemID: 1, Action: UnblockAnswer, Frees: 1, Priority: "P2"},
	}
	if !slices.Equal(r.Unblockers, want) {
		t.Fatalf("unblockers = %+v, want %+v", r.Unblockers, want)
	}
}

func TestRunwayCycleAndMissingDependencyNeverRun(t *testing.T) {
	items := map[int64]RunwayNode{
		1: {Stage: Build, DependsOn: []int64{2}},
		2: {Stage: Build, DependsOn: []int64{1}},
		3: {Stage: Build, DependsOn: []int64{42}},
	}
	if r := ComputeRunway(items, testDurations); len(r.Runnable) != 0 || r.Hours != 0 {
		t.Fatalf("runway = %+v, want nothing runnable", r)
	}
}

func TestRunwayMissingDurationFallsBack(t *testing.T) {
	r := ComputeRunway(map[int64]RunwayNode{1: {Stage: Land}}, nil)
	if want := DefaultStageDurations[Land].Hours(); r.Hours != want {
		t.Fatalf("hours = %v, want %v", r.Hours, want)
	}
}

func TestUserAway(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) time.Time { return time.Date(2026, 10, 17, h, m, 0, 0, ny) }
	cases := []struct {
		name      string
		last, now time.Time
		want      bool
	}{
		{"active at 15:00", at(14, 50), at(15, 0), false},
		{"30 minutes quiet", at(14, 30), at(15, 0), true},
		{"after 22:00", at(22, 5), at(22, 10), true},
		{"never acted", time.Time{}, at(15, 0), true},
	}
	for _, c := range cases {
		if got := UserAway(c.last, c.now, ny); got != c.want {
			t.Errorf("%s: UserAway = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRunwayWarnState(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 14, 58, 0, 0, time.UTC)
	plenty := Runway{Hours: 8}
	approve1 := Unblocker{ItemID: 1, Action: UnblockApprove, Frees: 3}
	dry := Runway{Hours: 1.5, Unblockers: []Unblocker{approve1}}
	nothing := Runway{Hours: 1}
	warned := RunwayWarned{Warned: true, Hours: 1.5, Unblockers: []string{"1:approve"}}
	cases := []struct {
		name      string
		r         Runway
		threshold int
		away      bool
		last      RunwayWarned
		send      bool
		next      RunwayWarned
	}{
		{"low, away, first time", dry, 3, true, RunwayWarned{}, true, warned},
		{"low, away, warned already", dry, 3, true, warned, false, warned},
		{"low, user here", dry, 3, false, RunwayWarned{}, false, RunwayWarned{}},
		{"low, user here, warned", dry, 3, false, warned, false, warned},
		{"nothing to unblock", nothing, 3, true, RunwayWarned{}, false, RunwayWarned{}},
		{"off clears", dry, 0, true, warned, false, RunwayWarned{}},
		{"recovered: the spell stays open", Runway{Hours: 8, Unblockers: []Unblocker{approve1}}, 3, true, warned, false,
			RunwayWarned{Warned: true, Hours: 1.5, Unblockers: []string{"1:approve"}, OKSince: t0}},
		{"recovered, the unblocker done: forgotten", plenty, 3, true, warned, false,
			RunwayWarned{Warned: true, Hours: 1.5, OKSince: t0}},
		{"recovered for RunwayAwayAfter clears", plenty, 3, true,
			RunwayWarned{Warned: true, Hours: 1.5, Unblockers: []string{"1:approve"}, OKSince: t0.Add(-RunwayAwayAfter)},
			false, RunwayWarned{}},
		{"low again after a blip: same warning, not sent", dry, 3, true,
			RunwayWarned{Warned: true, Hours: 1.5, Unblockers: []string{"1:approve"}, OKSince: t0.Add(-time.Minute)},
			false, warned},
		{"a new low is sent", Runway{Hours: 0.5, Unblockers: []Unblocker{approve1}}, 3, true, warned, true,
			RunwayWarned{Warned: true, Hours: 0.5, Unblockers: []string{"1:approve"}}},
		{"less than RunwayLowStep lower is not a new low", Runway{Hours: 0.6, Unblockers: []Unblocker{approve1}}, 3, true, warned, false, warned},
		{"higher but still low keeps the last low", Runway{Hours: 2.5, Unblockers: []Unblocker{approve1}}, 3, true, warned, false, warned},
		{"a new unblocker is sent", Runway{Hours: 1.5, Unblockers: []Unblocker{approve1, {ItemID: 2, Action: UnblockAnswer, Frees: 1}}}, 3, true, warned, true,
			RunwayWarned{Warned: true, Hours: 1.5, Unblockers: []string{"1:approve", "2:answer"}}},
		{"a new unblocker waits for the user to be away", Runway{Hours: 1.5, Unblockers: []Unblocker{approve1, {ItemID: 2, Action: UnblockAnswer, Frees: 1}}}, 3, false, warned, false, warned},
		{"reordered unblockers are not news", Runway{Hours: 1.5, Unblockers: []Unblocker{{ItemID: 2, Action: UnblockAnswer, Frees: 4}, approve1}}, 3, true,
			RunwayWarned{Warned: true, Hours: 1.5, Unblockers: []string{"1:approve", "2:answer"}}, false,
			RunwayWarned{Warned: true, Hours: 1.5, Unblockers: []string{"1:approve", "2:answer"}}},
		{"an unblocker done is forgotten", Runway{Hours: 1.5, Unblockers: []Unblocker{approve1}}, 3, true,
			RunwayWarned{Warned: true, Hours: 1.5, Unblockers: []string{"1:approve", "2:answer"}}, false, warned},
		{"same item, other action is news", Runway{Hours: 1.5, Unblockers: []Unblocker{{ItemID: 1, Action: UnblockTrust, Frees: 3}}}, 3, true, warned, true,
			RunwayWarned{Warned: true, Hours: 1.5, Unblockers: []string{"1:trust"}}},
	}
	for _, c := range cases {
		send, next := RunwayWarn(c.r, c.threshold, c.away, c.last, t0)
		if send != c.send || !reflect.DeepEqual(next, c.next) {
			t.Errorf("%s: RunwayWarn = %v, %+v; want %v, %+v", c.name, send, next, c.send, c.next)
		}
	}
}

// #227: the warning fired twice two minutes apart with the same content. A one-tick blip at the
// threshold between two low ticks must not re-send it.
func TestRunwayWarnOnePerRealChange(t *testing.T) {
	now := time.Date(2026, 10, 18, 14, 58, 51, 0, time.UTC)
	ub := []Unblocker{{ItemID: 7, Action: UnblockAnswer, Frees: 2}}
	var last RunwayWarned
	sends := 0
	for _, h := range []float64{1.5, 3.2, 1.5, 1.5, 3.4, 1.5} {
		var send bool
		send, last = RunwayWarn(Runway{Hours: h, Unblockers: ub}, 3, true, last, now)
		if send {
			sends++
		}
		now = now.Add(time.Minute)
	}
	if sends != 1 {
		t.Fatalf("low, blip, low: %d warnings, want 1", sends)
	}
}

// A dependency in review with risk hold does not make its dependent runnable: it stops at land for
// the user. Its own stages still count; the dependent is not counted in Hours.
func TestRunwayHeldRiskDependencyDoesNotFreeDependents(t *testing.T) {
	items := map[int64]RunwayNode{
		1: {Stage: Review, Hold: true, Priority: "P2"},
		2: {Stage: Build, DependsOn: []int64{1}, Priority: "P2"},
		3: {Stage: Build, DependsOn: []int64{1}, Priority: "P2"},
	}
	r := ComputeRunway(items, testDurations)
	if want := []int64{1}; !slices.Equal(r.Runnable, want) {
		t.Fatalf("runnable = %v, want %v", r.Runnable, want)
	}
	// The review's review+land: 0.5 h.
	if r.Hours < 0.49 || r.Hours > 0.51 {
		t.Fatalf("hours = %.2f, want 0.5", r.Hours)
	}
	// Approved for its head (Hold false), it frees both.
	items[1] = RunwayNode{Stage: Review, Priority: "P2"}
	if r := ComputeRunway(items, testDurations); len(r.Runnable) != 3 {
		t.Fatalf("runnable = %v, want all 3", r.Runnable)
	}
}
