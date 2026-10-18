package pipeline

import (
	"slices"
	"testing"
	"time"
)

var testDurations = map[Stage]time.Duration{Triage: 10 * time.Minute, Build: time.Hour, Review: 20 * time.Minute, Land: 10 * time.Minute}

func TestRunwayEveryChainWaitsOnOneHeldItem(t *testing.T) {
	items := map[int64]RunwayNode{
		1: {Stage: Held, Priority: "P2"},
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
	plenty := Runway{Hours: 8}
	dry := Runway{Hours: 1, Unblockers: []Unblocker{{ItemID: 1, Action: UnblockApprove, Frees: 3}}}
	nothing := Runway{Hours: 1}
	cases := []struct {
		name         string
		r            Runway
		threshold    int
		away, warned bool
		send, keep   bool
	}{
		{"low, away, first time", dry, 3, true, false, true, true},
		{"low, away, warned already", dry, 3, true, true, false, true},
		{"low, user here", dry, 3, false, false, false, false},
		{"low, user here, warned", dry, 3, false, true, false, true},
		{"recovered clears", plenty, 3, true, true, false, false},
		{"nothing to unblock", nothing, 3, true, false, false, false},
		{"off", dry, 0, true, false, false, false},
	}
	for _, c := range cases {
		send, keep := RunwayWarn(c.r, c.threshold, c.away, c.warned)
		if send != c.send || keep != c.keep {
			t.Errorf("%s: RunwayWarn = %v, %v; want %v, %v", c.name, send, keep, c.send, c.keep)
		}
	}
}
