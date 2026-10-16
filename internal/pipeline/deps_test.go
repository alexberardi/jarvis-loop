package pipeline

import (
	"slices"
	"testing"
	"time"
)

func after(it Item, deps ...int64) Item {
	it.DependsOn = deps
	return it
}

func TestChooseDependencies(t *testing.T) {
	allCaps := ClaimRequest{Runner: "devbox", Agent: "agent-1", Capabilities: []string{"gpu", "voice", "emulator"}}
	ctl := DefaultControl()
	cases := []struct {
		name   string
		items  []Item
		stages map[int64]Stage
		want   int64 // 0 = nothing
	}{
		{"no dependencies", []Item{waiting(2, Triage, "P3", 0)}, nil, 2},
		{"dependency done", []Item{after(waiting(2, Triage, "P3", 0), 1)}, map[int64]Stage{1: Done}, 2},
		{"dependency in build", []Item{after(waiting(2, Triage, "P3", 0), 1)}, map[int64]Stage{1: Build}, 0},
		{"dependency in triage", []Item{after(waiting(2, Triage, "P3", 0), 1)}, map[int64]Stage{1: Triage}, 0},
		{"dependency in qa is not done", []Item{after(waiting(2, Build, "P3", 0), 1)}, map[int64]Stage{1: QA}, 0},
		{"dependency held", []Item{after(waiting(2, Triage, "P3", 0), 1)}, map[int64]Stage{1: Held}, 0},
		{"dependency closed", []Item{after(waiting(2, Triage, "P3", 0), 1)}, map[int64]Stage{1: Closed}, 0},
		{"dependency unknown", []Item{after(waiting(2, Triage, "P3", 0), 1)}, nil, 0},
		{"one of two done", []Item{after(waiting(3, Triage, "P3", 0), 1, 2)}, map[int64]Stage{1: Done, 2: Review}, 0},
		{"both done", []Item{after(waiting(3, Triage, "P3", 0), 1, 2)}, map[int64]Stage{1: Done, 2: Done}, 3},
		{"blocked P1 lets a P3 through", []Item{after(waiting(2, Triage, "P1", time.Hour), 1), waiting(3, Triage, "P3", 0)},
			map[int64]Stage{1: Build}, 3},
		{"the dependency itself is claimable", []Item{waiting(1, Build, "P3", 0), after(waiting(2, Triage, "P1", time.Hour), 1)},
			map[int64]Stage{1: Build}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Choose(ctl, tc.items, tc.stages, nil, allCaps)
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

func TestWaitingOn(t *testing.T) {
	it := after(item(Triage), 3, 1, 2)
	got := WaitingOn(it, map[int64]Stage{1: Done, 2: QA})
	if !slices.Equal(got, []int64{3, 2}) {
		t.Fatalf("waiting on %v, want [3 2]", got)
	}
	if got := WaitingOn(item(Triage), nil); len(got) != 0 {
		t.Fatalf("no dependencies: waiting on %v", got)
	}
}

func TestDependencyCycle(t *testing.T) {
	cases := []struct {
		name  string
		graph map[int64][]int64
		want  []int64
	}{
		{"empty", nil, nil},
		{"chain", map[int64][]int64{3: {2}, 2: {1}, 1: nil}, nil},
		{"diamond", map[int64][]int64{4: {2, 3}, 2: {1}, 3: {1}}, nil},
		{"self", map[int64][]int64{5: {5}}, []int64{5, 5}},
		{"two", map[int64][]int64{1: {2}, 2: {1}}, []int64{1, 2, 1}},
		{"three, behind a tail", map[int64][]int64{1: {2}, 2: {3}, 3: {4}, 4: {2}}, []int64{2, 3, 4, 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DependencyCycle(tc.graph); !slices.Equal(got, tc.want) {
				t.Fatalf("cycle %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyDependencyClosed(t *testing.T) {
	for _, s := range []Stage{Triage, Build, Review, Land, Held} {
		t.Run(string(s), func(t *testing.T) {
			tr, ok := ApplyDependencyClosed(after(item(s), 7, 8), 7)
			if !ok || tr.From != s || tr.To != NeedsYou || tr.Kind != "dependency_closed" {
				t.Fatalf("from %s: ok=%v %+v", s, ok, tr)
			}
			want := "#7 it depends on was dropped: keep, change or drop this one?"
			if tr.Item.Question != want || tr.Item.ReturnStage != s || tr.Data["question"] != want || tr.Data["dependency"] != int64(7) {
				t.Fatalf("from %s: %+v %+v", s, tr.Item, tr.Data)
			}
			if !slices.Equal(tr.Item.DependsOn, []int64{8}) {
				t.Fatalf("depends on %v, want [8]", tr.Item.DependsOn)
			}
		})
	}
	t.Run("already needs_you keeps its question", func(t *testing.T) {
		it := after(item(NeedsYou), 7)
		it.ReturnStage, it.Question = Build, "which flag?"
		tr, ok := ApplyDependencyClosed(it, 7)
		if !ok || tr.To != NeedsYou || tr.Item.Question != "which flag?" || tr.Item.ReturnStage != Build || len(tr.Item.DependsOn) != 0 {
			t.Fatalf("ok=%v %+v", ok, tr)
		}
	})
	for _, s := range []Stage{QA, Done, Closed} {
		if _, ok := ApplyDependencyClosed(after(item(s), 7), 7); ok {
			t.Fatalf("%s: moved", s)
		}
	}
	if _, ok := ApplyDependencyClosed(after(item(Triage), 8), 7); ok {
		t.Fatal("not a dependency: moved")
	}
}
