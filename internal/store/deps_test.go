package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

func addAfter(t *testing.T, s *Store, title, pri string, deps ...int64) pipeline.Item {
	t.Helper()
	it, err := s.AddItem(context.Background(), user, NewItem{Title: title, Repo: "jarvis-server", Priority: pri, DependsOn: deps})
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func TestAddItemDependencies(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	a := add(t, s, "a", "")
	b := addAfter(t, s, "b", "", a.ID, a.ID)
	if !slices.Equal(b.DependsOn, []int64{a.ID}) {
		t.Fatalf("depends on %v, want [%d]", b.DependsOn, a.ID)
	}
	d, err := s.GetItem(ctx, b.ID)
	if err != nil || !slices.Equal(d.Item.DependsOn, []int64{a.ID}) || !slices.Equal(d.WaitingOn, []int64{a.ID}) {
		t.Fatalf("stored: %+v waiting %v %v", d.Item.DependsOn, d.WaitingOn, err)
	}
	if d.Events[0].Data["depends_on"] == nil {
		t.Fatalf("created event: %+v", d.Events[0].Data)
	}
	if d, _ := s.GetItem(ctx, a.ID); d.Item.DependsOn == nil || len(d.WaitingOn) != 0 {
		t.Fatalf("no dependencies: %+v %v", d.Item.DependsOn, d.WaitingOn)
	}

	cards, err := s.BoardItems(ctx)
	if err != nil || len(cards) != 2 || len(cards[0].WaitingOn) != 0 || !slices.Equal(cards[1].WaitingOn, []int64{a.ID}) {
		t.Fatalf("board: %+v %v", cards, err)
	}

	_, err = s.AddItem(ctx, user, NewItem{Title: "c", DependsOn: []int64{a.ID, 99}})
	if code(err) != pipeline.CodeInvalidField || !strings.Contains(err.Error(), "no item #99") {
		t.Fatalf("unknown id: %v", err)
	}
	// The next id is 3: naming it is the new item depending on itself, an item the user never saw.
	_, err = s.AddItem(ctx, user, NewItem{Title: "c", DependsOn: []int64{3}})
	if code(err) != pipeline.CodeInvalidField || !strings.Contains(err.Error(), "no item #3") {
		t.Fatalf("self: %v", err)
	}
	// A closed item never becomes done: an item after it would wait forever.
	if _, err := s.Drop(ctx, user, a.ID, ""); err != nil {
		t.Fatal(err)
	}
	_, err = s.AddItem(ctx, user, NewItem{Title: "c", DependsOn: []int64{a.ID}})
	if code(err) != pipeline.CodeInvalidField || !strings.Contains(err.Error(), fmt.Sprintf("#%d is closed", a.ID)) {
		t.Fatalf("closed: %v", err)
	}
	rows, _ := s.ListItems(ctx, "", "")
	if len(rows) != 2 {
		t.Fatalf("a refused add left an item: %+v", rows)
	}
}

// B depends on A: B is not claimed until A is merged, though it is the higher priority.
func TestClaimWaitsForDependencies(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	var b pipeline.Item
	a := toMerged(t, s, runner, func(a pipeline.Item) { b = addAfter(t, s, "b", "P1", a.ID) }, nil)
	if d, _ := s.GetItem(ctx, a.ID); d.Item.Stage != pipeline.Done {
		t.Fatalf("a is %s", d.Item.Stage)
	}
	if d, _ := s.GetItem(ctx, b.ID); len(d.WaitingOn) != 0 {
		t.Fatalf("b still waiting on %v", d.WaitingOn)
	}
	if c := claim(t, s, "x"); c == nil || c.Item.ID != b.ID {
		t.Fatalf("claim after a is done: %+v", c)
	}
	// Adding after a done item stays allowed (only closed ones are refused).
	if c := addAfter(t, s, "c", "", a.ID); len(c.DependsOn) != 1 {
		t.Fatalf("after a done item: %+v", c)
	}
}

// A dependency is met once merged (docs/decisions/2026-10-18-deps-at-merge.md): B is claimed while
// A waits in qa for the user's check.
func TestClaimTakesADependencyInQA(t *testing.T) {
	s, _ := open(t)
	var b pipeline.Item
	a := toMerged(t, s, runner, func(a pipeline.Item) {
		if _, err := s.SetQA(context.Background(), user, a.ID); err != nil {
			t.Fatal(err)
		}
		b = addAfter(t, s, "b", "P1", a.ID)
	}, nil)
	if d, _ := s.GetItem(context.Background(), a.ID); d.Item.Stage != pipeline.QA {
		t.Fatalf("a is %s", d.Item.Stage)
	}
	if d, _ := s.GetItem(context.Background(), b.ID); len(d.WaitingOn) != 0 {
		t.Fatalf("b still waiting on %v", d.WaitingOn)
	}
	if c := claim(t, s, "x"); c == nil || c.Item.ID != b.ID {
		t.Fatalf("claim while a is in qa: %+v", c)
	}
	// Adding after an item in qa stays allowed.
	addAfter(t, s, "c", "", a.ID)
}

// A reopened dependency flags each open item that depended on it, in its thread and on the board,
// without moving it or ending its claim; the flag goes once the reopen item is merged.
func TestReopenFlagsDependents(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	var b, c, dropped, merged, finished pipeline.Item
	a := toMerged(t, s, runner, func(a pipeline.Item) {
		if _, err := s.SetQA(ctx, user, a.ID); err != nil {
			t.Fatal(err)
		}
		b = addAfter(t, s, "b", "P1", a.ID)
		c = addAfter(t, s, "c", "P2", a.ID)
		dropped = addAfter(t, s, "dropped", "P3", a.ID)
		merged = addAfter(t, s, "merged", "P3", a.ID)
		finished = addAfter(t, s, "finished", "P3", a.ID)
		add(t, s, "unrelated", "P3")
	}, nil)
	// A dependent that merged already is flagged too (its qa check is next); a done one is not.
	if _, err := s.w.Exec(`UPDATE items SET stage = 'qa' WHERE id = ?`, merged.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`UPDATE items SET stage = 'done' WHERE id = ?`, finished.ID); err != nil {
		t.Fatal(err)
	}
	if cl := claim(t, s, "x"); cl == nil || cl.Item.ID != b.ID {
		t.Fatalf("claim b: %+v", cl)
	}
	if _, err := s.Drop(ctx, user, dropped.ID, ""); err != nil {
		t.Fatal(err)
	}
	_, n, err := s.Reopen(ctx, user, a.ID, "the code is still small")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{b.ID, c.ID} {
		d, _ := s.GetItem(ctx, id)
		e := d.Events[len(d.Events)-1]
		if e.Kind != KindDependencyReopened || e.Actor != "loopd" || e.From != "triage" || e.To != "triage" ||
			e.Data["dependency"] != float64(a.ID) || e.Data["reopen_item"] != float64(n.ID) || e.Data["words"] != "the code is still small" {
			t.Fatalf("#%d event: %+v", id, e)
		}
		if d.Item.Stage != pipeline.Triage || len(d.WaitingOn) != 0 {
			t.Fatalf("#%d moved: %+v waiting %v", id, d.Item, d.WaitingOn)
		}
	}
	if d, _ := s.GetItem(ctx, b.ID); d.Lease == nil {
		t.Fatal("b's claim ended")
	}
	if ev := lastEvent(t, s, dropped.ID, KindDependencyReopened); ev != nil {
		t.Fatalf("a closed item was flagged: %+v", ev)
	}
	if ev := lastEvent(t, s, finished.ID, KindDependencyReopened); ev != nil {
		t.Fatalf("a done item was flagged: %+v", ev)
	}
	if ev := lastEvent(t, s, merged.ID, KindDependencyReopened); ev == nil || ev.From != "qa" || ev.To != "qa" ||
		ev.Data["reopen_item"] != float64(n.ID) {
		t.Fatalf("the dependent in qa: %+v", ev)
	}
	flags := func() map[int64][]ReopenedDependency {
		t.Helper()
		cards, err := s.BoardItems(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64][]ReopenedDependency{}
		for _, card := range cards {
			if len(card.ReopenedDeps) > 0 {
				out[card.ID] = card.ReopenedDeps
			}
		}
		return out
	}
	want := []ReopenedDependency{{Dependency: a.ID, ReopenItem: n.ID, Words: "the code is still small"}}
	if got := flags(); len(got) != 3 || !slices.Equal(got[b.ID], want) || !slices.Equal(got[c.ID], want) ||
		!slices.Equal(got[merged.ID], want) {
		t.Fatalf("board flags: %+v", got)
	}
	snap, err := s.OrchSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range snap.Items {
		if it.ID == c.ID && !slices.Equal(it.ReopenedDeps, want) {
			t.Fatalf("orchestrator snapshot: %+v", it)
		}
	}
	// The flag goes when the dependent is closed.
	if _, err := s.Drop(ctx, user, c.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got := flags(); len(got) != 2 || got[b.ID] == nil || got[merged.ID] == nil {
		t.Fatalf("after c dropped: %+v", got)
	}
	// And when the reopen item is merged or closed.
	if _, err := s.Drop(ctx, user, n.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got := flags(); len(got) != 0 {
		t.Fatalf("after the reopen item closed: %+v", got)
	}
}

func TestDroppedDependencyAsksAboutDependents(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	a := add(t, s, "a", "")
	other := add(t, s, "other", "")
	b := addAfter(t, s, "b", "", a.ID, other.ID)
	c := addAfter(t, s, "c", "", other.ID)
	if _, err := s.Drop(ctx, user, a.ID, ""); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetItem(ctx, b.ID)
	want := "#1 it depends on was dropped: keep, change or drop this one?"
	if d.Item.Stage != pipeline.NeedsYou || d.Item.Question != want || d.Item.ReturnStage != pipeline.Triage ||
		!slices.Equal(d.Item.DependsOn, []int64{other.ID}) {
		t.Fatalf("b after a dropped: %+v", d.Item)
	}
	e := d.Events[len(d.Events)-1]
	if e.Kind != "dependency_closed" || e.Actor != "loopd" || e.From != "triage" || e.To != "needs_you" || e.Data["question"] != want {
		t.Fatalf("event: %+v", e)
	}
	if d, _ := s.GetItem(ctx, c.ID); d.Item.Stage != pipeline.Triage {
		t.Fatalf("c moved: %+v", d.Item)
	}
	// "keep": the answer sends it back to triage, waiting only on the dependency that is left.
	if _, err := s.Answer(ctx, user, b.ID, "keep"); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetItem(ctx, b.ID); d.Item.Stage != pipeline.Triage || !slices.Equal(d.WaitingOn, []int64{other.ID}) {
		t.Fatalf("b after keep: %+v %v", d.Item, d.WaitingOn)
	}
}

func TestClosedDependencyAsksAboutDependents(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	a := add(t, s, "a", "")
	b := addAfter(t, s, "b", "", a.ID)
	if c := claim(t, s, "x"); c == nil || c.Item.ID != a.ID {
		t.Fatalf("claim: %+v", c)
	}
	evidence(t, s, a.ID)
	if _, err := s.Advance(ctx, runner, a.ID, pipeline.NotReproducible, map[string]string{"summary": "works"}, nil); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetItem(ctx, b.ID); d.Item.Stage != pipeline.NeedsYou || d.Item.Question != pipeline.DependencyClosedQuestion(a.ID) {
		t.Fatalf("b after a closed: %+v", d.Item)
	}
}
