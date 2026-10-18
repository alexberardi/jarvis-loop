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
		{"dependency in qa is merged", []Item{after(waiting(2, Build, "P3", 0), 1)}, map[int64]Stage{1: QA}, 2},
		{"dependency in land is not merged", []Item{after(waiting(2, Build, "P3", 0), 1)}, map[int64]Stage{1: Land}, 0},
		{"dependency in review is not merged", []Item{after(waiting(2, Build, "P3", 0), 1)}, map[int64]Stage{1: Review}, 0},
		{"one done, one in qa", []Item{after(waiting(3, Triage, "P3", 0), 1, 2)}, map[int64]Stage{1: Done, 2: QA}, 3},
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
			got, ok := Choose(ctl, tc.items, tc.stages, nil, allCaps, nil)
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
	got := WaitingOn(it, map[int64]Stage{1: Done, 2: Land, 4: QA})
	if !slices.Equal(got, []int64{3, 2}) {
		t.Fatalf("waiting on %v, want [3 2]", got)
	}
	if got := WaitingOn(after(item(Triage), 1, 2), map[int64]Stage{1: Done, 2: QA}); len(got) != 0 {
		t.Fatalf("merged dependencies: waiting on %v", got)
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

func TestWaitChains(t *testing.T) {
	n := func(st Stage, claimed bool, deps ...int64) WaitNode {
		return WaitNode{Stage: st, Claimed: claimed, DependsOn: deps}
	}
	cases := []struct {
		name  string
		id    int64
		items map[int64]WaitNode
		want  []WaitChain
	}{
		{"no deps", 1, map[int64]WaitNode{1: n(Build, false)}, nil},
		{"done dep skipped", 1, map[int64]WaitNode{1: n(Triage, false, 2), 2: n(Done, false)}, nil},
		{"qa dep skipped: it is merged", 1, map[int64]WaitNode{1: n(Triage, false, 2), 2: n(QA, false)}, nil},
		{"qa link in the middle is the root's way", 1,
			map[int64]WaitNode{1: n(Triage, false, 2), 2: n(Review, false, 3), 3: n(QA, false)},
			[]WaitChain{{Path: []int64{2}, Root: "queued for review"}}},
		{"direct dep needs you", 1, map[int64]WaitNode{1: n(Triage, false, 2), 2: n(NeedsYou, false)},
			[]WaitChain{{Path: []int64{2}, Root: "needs you"}}},
		{"A→B→C needs you", 1, map[int64]WaitNode{1: n(Triage, false, 2), 2: n(Triage, false, 3), 3: n(NeedsYou, false)},
			[]WaitChain{{Path: []int64{2, 3}, Root: "needs you"}}},
		{"done link in the middle is the root's way", 1,
			map[int64]WaitNode{1: n(Triage, false, 2), 2: n(Build, true, 3), 3: n(Done, false)},
			[]WaitChain{{Path: []int64{2}, Root: "in build"}}},
		{"two branches", 1,
			map[int64]WaitNode{1: n(Triage, false, 2, 4), 2: n(Held, false), 3: n(Done, false), 4: n(Build, false, 3, 5), 5: n(Review, false)},
			[]WaitChain{{Path: []int64{2}, Root: "held for approval"}, {Path: []int64{4, 5}, Root: "queued for review"}}},
		{"closed dep", 1, map[int64]WaitNode{1: n(NeedsYou, false, 2), 2: n(Closed, false)},
			[]WaitChain{{Path: []int64{2}, Root: "closed"}}},
		{"missing dep", 1, map[int64]WaitNode{1: n(Triage, false, 9)},
			[]WaitChain{{Path: []int64{9}, Root: "not found"}}},
		{"cycle", 1, map[int64]WaitNode{1: n(Triage, false, 2), 2: n(Triage, false, 3), 3: n(Triage, false, 2)},
			[]WaitChain{{Path: []int64{2, 3, 2}, Root: "a cycle"}}},
		{"back to the item itself", 1, map[int64]WaitNode{1: n(Triage, false, 2), 2: n(Triage, false, 1)},
			[]WaitChain{{Path: []int64{2, 1}, Root: "a cycle"}}},
		{"roots by stage", 1,
			map[int64]WaitNode{1: n(Triage, false, 2, 3, 4, 5, 6, 7), 2: n(Triage, false), 3: n(Triage, true), 4: n(Build, false),
				5: n(Land, true), 6: n(NeedsYou, false), 7: n(Land, false)},
			[]WaitChain{{Path: []int64{2}, Root: "waiting for triage"}, {Path: []int64{3}, Root: "in triage"},
				{Path: []int64{4}, Root: "ready, not claimed"}, {Path: []int64{5}, Root: "landing"}, {Path: []int64{6}, Root: "needs you"},
				{Path: []int64{7}, Root: "queued to land"}}},
		{"a done item waits on nothing", 1, map[int64]WaitNode{1: n(Done, false, 2), 2: n(Build, false)}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := WaitChains(c.id, c.items)
			if len(got) != len(c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
			for i := range got {
				if !slices.Equal(got[i].Path, c.want[i].Path) || got[i].Root != c.want[i].Root {
					t.Fatalf("chain %d: got %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestWaitChainText(t *testing.T) {
	if got := (WaitChain{Path: []int64{12, 35}, Root: "needs you"}).String(); got != "#12 → #35 → needs you" {
		t.Fatalf("got %q", got)
	}
}
