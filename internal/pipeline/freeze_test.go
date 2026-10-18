package pipeline

import (
	"strings"
	"testing"
	"time"
)

func inRepo(it Item, repo string) Item {
	it.Repo = repo
	return it
}

func TestChooseFrozen(t *testing.T) {
	req := ClaimRequest{Runner: "devbox", Agent: "agent-1", Capabilities: []string{"gpu"}}
	ctl := DefaultControl()
	one := DefaultControl()
	one.ActiveAgents = 1
	stopped := DefaultControl()
	stopped.Stopped = true
	paused := DefaultControl()
	paused.ActiveAgents = 0
	land1 := DefaultControl() // land's limit is 1
	gpu1 := DefaultControl()
	gpu1.Limits = map[string]int{"gpu": 1}
	frozen := Frozen{"o/a": 9}

	cases := []struct {
		name   string
		ctl    Control
		items  []Item
		live   []LiveLease
		frozen Frozen
		want   int64
	}{
		{"not frozen: priority order", ctl, []Item{inRepo(waiting(1, Build, "P1", 0), "o/a"), inRepo(waiting(9, Triage, "P1", 0), "o/a")}, nil, nil, 1},
		{"a frozen repo's items are skipped", ctl, []Item{inRepo(waiting(1, Build, "P1", 0), "o/a"), inRepo(waiting(2, Land, "P1", 0), "o/a")}, nil, frozen, 0},
		{"other repos keep going", ctl, []Item{inRepo(waiting(1, Build, "P1", 0), "o/a"), inRepo(waiting(2, Build, "P5", 0), "o/b")}, nil, frozen, 2},
		{"an item with no repo yet is not frozen", ctl, []Item{inRepo(waiting(1, Build, "P1", 0), "o/a"), waiting(2, Triage, "P5", 0)}, nil, frozen, 2},
		{"the fix goes first, before a P1 of another repo", ctl, []Item{inRepo(waiting(2, Land, "P1", time.Hour), "o/b"), inRepo(waiting(9, Triage, "P1", 0), "o/a")}, nil, frozen, 9},
		{"the fix takes a slot over active agents", one, []Item{inRepo(waiting(9, Build, "P1", 0), "o/a"), inRepo(waiting(2, Build, "P1", 0), "o/b")},
			[]LiveLease{{ItemID: 1, Stage: Build}}, frozen, 9},
		{"but nothing else does", one, []Item{inRepo(waiting(2, Build, "P1", 0), "o/b")}, []LiveLease{{ItemID: 1, Stage: Build}}, frozen, 0},
		{"the fix passes land's limit", land1, []Item{inRepo(waiting(9, Land, "P1", 0), "o/a")}, []LiveLease{{ItemID: 1, Stage: Land}}, frozen, 9},
		{"the fix waits on a resource limit", gpu1, []Item{inRepo(waiting(9, Build, "P1", 0, "gpu"), "o/a")}, []LiveLease{{ItemID: 1, Stage: Build, Resources: []string{"gpu"}}}, frozen, 0},
		{"stopped stops the fix too", stopped, []Item{inRepo(waiting(9, Build, "P1", 0), "o/a")}, nil, frozen, 0},
		{"0 active agents stops the fix too", paused, []Item{inRepo(waiting(9, Build, "P1", 0), "o/a")}, nil, frozen, 0},
		{"a fix already claimed is not claimed again", ctl, []Item{inRepo(waiting(9, Build, "P1", 0), "o/a")}, []LiveLease{{ItemID: 9, Stage: Build}}, frozen, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Choose(tc.ctl, tc.items, nil, tc.live, req, tc.frozen)
			if tc.want == 0 {
				if ok {
					t.Fatalf("got item %d, want nothing", got.ID)
				}
				return
			}
			if !ok || got.ID != tc.want {
				t.Fatalf("got %d (ok %v), want %d", got.ID, ok, tc.want)
			}
		})
	}
}

func TestChooseWorkFixBeatsOrchestrate(t *testing.T) {
	req := ClaimRequest{Runner: "r", Agent: "a", Kinds: []string{WorkItem, WorkOrchestrate}}
	items := []Item{inRepo(waiting(9, Triage, "P1", 0), "o/a")}
	w, ok := ChooseWork(DefaultControl(), items, nil, nil, nil, &OrchCandidate{}, nil, req, Frozen{"o/a": 9})
	if !ok || w.Item == nil || w.Item.ID != 9 {
		t.Fatalf("got %+v, want the fix", w)
	}
	w, ok = ChooseWork(DefaultControl(), items, nil, nil, nil, &OrchCandidate{}, nil, req, nil)
	if !ok || w.Orch == nil {
		t.Fatalf("got %+v, want the orchestrate turn when nothing is frozen", w)
	}
}

func TestApplyMainRedRebuild(t *testing.T) {
	b := Item{ID: 1, Stage: Build, CIRedSHA: "abc1234"}
	tr, err := ApplyMainRedRebuild(b)
	if err != nil || tr.To != Build || tr.Kind != "rebuild" || tr.Data["notes"] != MainRedNotes {
		t.Fatalf("from build: %+v, %v", tr, err)
	}
	ny := Item{ID: 1, Stage: NeedsYou, ReturnStage: Build, Question: "CI failed twice"}
	tr, err = ApplyMainRedRebuild(ny)
	if err != nil || tr.Item.Stage != Build || tr.Item.Question != "" || tr.Data["question"] != "CI failed twice" {
		t.Fatalf("from needs_you: %+v, %v", tr, err)
	}
	for _, it := range []Item{{Stage: Review}, {Stage: NeedsYou, ReturnStage: Triage}, {Stage: Held}} {
		if _, err := ApplyMainRedRebuild(it); err == nil {
			t.Errorf("%s/%s: want refused", it.Stage, it.ReturnStage)
		}
	}
}

func TestMainRedAsk(t *testing.T) {
	ask := MainRedAsk(MainRed{Slug: "o/a", PR: 12, Item: 3, MergeSHA: strings.Repeat("a", 40), LastGreen: strings.Repeat("b", 40),
		Failing: []string{"test"}, URLs: []string{"https://github.com/o/a/actions/runs/1"}})
	for _, want := range []string{"the squash merge of o/a#12.", "Caused by item #3 / merge " + strings.Repeat("a", 40) + ".",
		"Last green: " + strings.Repeat("b", 40), "Likely culprit: o/a#12 (item #3)", "revert that PR", "actions/runs/1"} {
		if !strings.Contains(ask, want) {
			t.Errorf("ask lacks %q:\n%s", want, ask)
		}
	}
	if got := MainRedTitle("o/a", []string{"test", "lint"}); got != "main red in o/a: test, lint" {
		t.Errorf("title %q", got)
	}
}
