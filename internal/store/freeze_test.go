package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

var redData = func() map[string]any {
	return map[string]any{"result": "red", "urls": []any{"https://github.com/o/r/actions/runs/1"}, "failing": []any{"test"},
		"merge_sha": "ffffffffffffffffffffffffffffffffffffffff", "last_green": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "rerun": []any{"test"}}
}

func addIn(t *testing.T, s *Store, title, repo, pri string) pipeline.Item {
	t.Helper()
	it, err := s.AddItem(context.Background(), user, NewItem{Title: title, Repo: repo, Priority: pri})
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func claimed(t *testing.T, s *Store, agent string) *Claim {
	t.Helper()
	c, err := s.Claim(context.Background(), runner, pipeline.ClaimRequest{Runner: runner, Agent: agent})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var triaged = map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": ""}

// merged is toDone, read back (its repo is the PR's).
func merged(t *testing.T, s *Store) pipeline.Item {
	t.Helper()
	d, err := s.GetItem(context.Background(), toDone(t, s, runner).ID)
	if err != nil {
		t.Fatal(err)
	}
	return d.Item
}

func countEvents(t *testing.T, s *Store, kind string) int {
	t.Helper()
	var n int
	if err := s.w.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A red main files the repo's P1 and freezes it: the P1 goes first (over the active-agents
// number), other repos keep going, nothing else in the repo is claimed, a run already going finishes
// and its advance waits; lifting the freeze applies the queued advances in the order they finished.
func TestMainRedFreezesTheRepo(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	culprit := merged(t, s)
	repo := culprit.Repo
	if _, err := s.SetControl(ctx, user, ControlPatch{Limits: map[string]int{"triage": 4}}); err != nil {
		t.Fatal(err)
	}
	// Two runs going in the repo when main goes red, and work queued in it and in another repo.
	first := addIn(t, s, "first", repo, "P2")
	second := addIn(t, s, "second", repo, "P2")
	c1, c2 := claimed(t, s, "t1"), claimed(t, s, "t2")
	if c1 == nil || c2 == nil || c1.Item.ID != first.ID || c2.Item.ID != second.ID {
		t.Fatalf("claims %+v %+v", c1, c2)
	}
	queued := addIn(t, s, "queued", repo, "P1")
	other := addIn(t, s, "other repo", "o/other", "P3")

	ev, err := s.MainCI(ctx, runner, culprit.ID, redData())
	if err != nil {
		t.Fatal(err)
	}
	p1 := int64(ev.Data["p1_item"].(float64))
	fz, err := s.Freezes(ctx)
	if err != nil || len(fz) != 1 || fz[0].Repo != repo || fz[0].ItemID != p1 || fz[0].Jobs[0] != "test" {
		t.Fatalf("freezes %+v %v", fz, err)
	}
	d, _ := s.GetItem(ctx, p1)
	if d.Item.Title != "main red in "+repo+": test" || d.Item.Priority != "P1" || d.Item.Source != "finding" || d.Frozen == nil || d.Frozen.ItemID != p1 {
		t.Fatalf("P1 %+v frozen %+v", d.Item, d.Frozen)
	}
	if countEvents(t, s, KindFrozen) != 1 {
		t.Fatal("no frozen event")
	}

	// Active agents at 2 with both runs going: the P1 still gets a slot; nothing else does.
	two := 2
	if _, err := s.SetControl(ctx, user, ControlPatch{ActiveAgents: &two}); err != nil {
		t.Fatal(err)
	}
	if c := claimed(t, s, "t3"); c == nil || c.Item.ID != p1 {
		t.Fatalf("the P1 first: %+v", c)
	}
	if c := claimed(t, s, "t4"); c != nil {
		t.Fatalf("over active agents: %+v", c)
	}
	six := 6
	s.SetControl(ctx, user, ControlPatch{ActiveAgents: &six})
	if c := claimed(t, s, "t4"); c == nil || c.Item.ID != other.ID {
		t.Fatalf("the other repo keeps going, the frozen one does not: %+v", c)
	}
	if c := claimed(t, s, "t5"); c != nil {
		t.Fatalf("a frozen item claimed: %+v", c)
	}

	// The runs finish, second first: the advances wait, the items stay in triage, unclaimable.
	evidence(t, s, second.ID)
	if _, err := s.Advance(ctx, runner, second.ID, pipeline.Reproduced, triaged, nil); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	evidence(t, s, first.ID)
	if it, err := s.Advance(ctx, runner, first.ID, pipeline.Reproduced, triaged, nil); err != nil || it.Stage != pipeline.Triage {
		t.Fatalf("queued advance: %+v %v", it, err)
	}
	// A bad report is refused at once, not queued.
	d, _ = s.GetItem(ctx, first.ID)
	if d.Lease != nil || d.Item.Stage != pipeline.Triage {
		t.Fatalf("after the queued advance: %+v", d)
	}
	if countEvents(t, s, KindAdvanceQueued) != 2 {
		t.Fatal("want 2 advance_queued")
	}
	if fz, _ := s.Freezes(ctx); fz[0].Queued != 2 {
		t.Fatalf("queued %d", fz[0].Queued)
	}
	if c := claimed(t, s, "t6"); c != nil {
		t.Fatalf("claimed while frozen: %+v", c)
	}

	// Lift by hand: queued advances apply in finish order, and the queued item becomes claimable.
	if _, err := s.Unfreeze(ctx, user, "nope/nope"); err == nil {
		t.Fatal("unfreeze of an unknown repo")
	}
	if _, err := s.Unfreeze(ctx, user, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Unfreeze(ctx, user, repo); code(err) != CodeNotFrozen {
		t.Fatalf("a second unfreeze: %v", err)
	}
	var order []int64
	rows, _ := s.w.Query(`SELECT item_id FROM events WHERE kind = 'advanced' AND outcome = 'reproduced' AND item_id IN (?, ?) ORDER BY id`, first.ID, second.ID)
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		order = append(order, id)
	}
	rows.Close()
	if len(order) != 2 || order[0] != second.ID || order[1] != first.ID {
		t.Fatalf("applied order %v, want [%d %d]", order, second.ID, first.ID)
	}
	for _, id := range []int64{first.ID, second.ID} {
		if d, _ := s.GetItem(ctx, id); d.Item.Stage != pipeline.Build || d.Frozen != nil {
			t.Fatalf("item %d after the thaw: %s %+v", id, d.Item.Stage, d.Frozen)
		}
	}
	if c := claimed(t, s, "t7"); c == nil || c.Item.ID != queued.ID {
		t.Fatalf("after the thaw, the P1 queued item: %+v", c)
	}
	if countEvents(t, s, KindUnfrozen) != 1 {
		t.Fatal("no unfrozen event")
	}
}

// A second red while the P1 is open reuses it; a queued advance whose item moved meanwhile (the
// user dropped it) is dropped, not applied.
func TestMainRedReusesTheOpenP1(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	a := merged(t, s)
	b := merged(t, s)
	run := addIn(t, s, "running", a.Repo, "P2")
	if c := claimed(t, s, "t1"); c == nil || c.Item.ID != run.ID {
		t.Fatalf("claim %+v", c)
	}
	ev1, err := s.MainCI(ctx, runner, a.ID, redData())
	if err != nil {
		t.Fatal(err)
	}
	ev2, err := s.MainCI(ctx, runner, b.ID, redData())
	if err != nil {
		t.Fatal(err)
	}
	if ev1.Data["p1_item"] != ev2.Data["p1_item"] || ev2.Data["reused"] != true {
		t.Fatalf("p1 %v then %v (%v)", ev1.Data["p1_item"], ev2.Data["p1_item"], ev2.Data)
	}
	evidence(t, s, run.ID)
	if _, err := s.Advance(ctx, runner, run.ID, pipeline.Reproduced, triaged, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Drop(ctx, user, run.ID, "never mind"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Unfreeze(ctx, user, a.Repo); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetItem(ctx, run.ID); d.Item.Stage != pipeline.Closed {
		t.Fatalf("dropped item %s", d.Item.Stage)
	}
	if countEvents(t, s, KindQueuedDropped) != 1 {
		t.Fatal("want a queued_advance_dropped")
	}
}

// The P1 goes through every stage while the repo is frozen; main green on its merge lifts the
// freeze, and an item land sent back for red CI in the red window is rebuilt ("rebase and re-run").
func TestMainGreenOnTheFixUnfreezes(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	culprit := merged(t, s)
	repo := culprit.Repo
	// An item land sends back for red CI after the culprit merged, before the freeze.
	ci := addIn(t, s, "ci red", repo, "P3")
	clk.Advance(time.Minute)
	step := func(id int64, agent string, out pipeline.Outcome, f map[string]string, paths []string, ev bool) {
		t.Helper()
		c := claimed(t, s, agent)
		if c == nil || c.Item.ID != id {
			t.Fatalf("claim for %s: %+v", out, c)
		}
		if ev {
			evidence(t, s, id)
		}
		if _, err := s.Advance(ctx, runner, id, out, f, paths); err != nil {
			t.Fatalf("%s: %v", out, err)
		}
	}
	step(ci.ID, "triager", pipeline.Reproduced, triaged, nil, true)
	step(ci.ID, "builder", pipeline.PROpened, map[string]string{"repo": repo, "pr": "8", "branch": "loop/2-x", "head_sha": shaA}, []string{"M y.go"}, true)
	step(ci.ID, "reviewer", pipeline.Approved, map[string]string{"summary": "ok", "head_sha": shaA}, nil, false)
	step(ci.ID, "lander", pipeline.BackToBuild, map[string]string{"reason": "CI failed", "ci_head_sha": shaA}, []string{"M y.go"}, false)

	ev, err := s.MainCI(ctx, runner, culprit.ID, redData())
	if err != nil {
		t.Fatal(err)
	}
	p1 := int64(ev.Data["p1_item"].(float64))
	fix := "ffffffffffffffffffffffffffffffffffffff00"
	step(p1, "triager", pipeline.Reproduced, triaged, nil, true)
	step(p1, "builder", pipeline.PROpened, map[string]string{"repo": repo, "pr": "9", "branch": "loop/3-fix", "head_sha": shaA}, []string{"M x.go"}, true)
	step(p1, "reviewer", pipeline.Approved, map[string]string{"summary": "ok", "head_sha": shaA}, nil, false)
	step(p1, "lander", pipeline.Merged, map[string]string{"merge_sha": fix, "head_sha": shaA}, []string{"M x.go"}, false)
	if fz, _ := s.Freezes(ctx); len(fz) != 1 {
		t.Fatal("unfrozen by the merge alone")
	}
	if _, err := s.MainCI(ctx, runner, p1, map[string]any{"result": "green", "merge_sha": fix}); err != nil {
		t.Fatal(err)
	}
	if fz, _ := s.Freezes(ctx); len(fz) != 0 {
		t.Fatalf("still frozen after main green: %+v", fz)
	}
	d, _ := s.GetItem(ctx, ci.ID)
	last := d.Events[len(d.Events)-1]
	if d.Item.Stage != pipeline.Build || last.Kind != "rebuild" || last.Actor != "loopd" || last.Data["notes"] != pipeline.MainRedNotes {
		t.Fatalf("red-window item: %s, last %+v", d.Item.Stage, last)
	}
}

// A green main_ci from a merge before the red one says nothing about main now: still frozen.
func TestMainGreenBeforeTheRedMergeKeepsTheFreeze(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	earlier := merged(t, s)
	clk.Advance(time.Minute)
	culprit := merged(t, s)
	if _, err := s.MainCI(ctx, runner, culprit.ID, redData()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MainCI(ctx, runner, earlier.ID, map[string]any{"result": "green"}); err != nil {
		t.Fatal(err)
	}
	if fz, _ := s.Freezes(ctx); len(fz) != 1 {
		t.Fatal("an earlier merge's green lifted the freeze")
	}
}

// A merge is a fact on GitHub, not a stage move the freeze can hold back: a land run that merged
// just as main went red (between its frozen check and the merge) applies at once, so the watch's
// main_ci on that merge is taken.
func TestMergedAppliesInAFrozenRepo(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	culprit := merged(t, s)
	repo := culprit.Repo
	it := addIn(t, s, "landing", repo, "P3")
	clk.Advance(time.Minute)
	step := func(agent string, out pipeline.Outcome, f map[string]string, paths []string, ev bool) {
		t.Helper()
		c := claimed(t, s, agent)
		if c == nil || c.Item.ID != it.ID {
			t.Fatalf("claim for %s: %+v", out, c)
		}
		if ev {
			evidence(t, s, it.ID)
		}
		if _, err := s.Advance(ctx, runner, it.ID, out, f, paths); err != nil {
			t.Fatalf("%s: %v", out, err)
		}
	}
	step("triager", pipeline.Reproduced, triaged, nil, true)
	step("builder", pipeline.PROpened, map[string]string{"repo": repo, "pr": "8", "branch": "loop/2-x", "head_sha": shaA}, []string{"M y.go"}, true)
	step("reviewer", pipeline.Approved, map[string]string{"summary": "ok", "head_sha": shaA}, nil, false)
	if c := claimed(t, s, "lander"); c == nil || c.Item.ID != it.ID {
		t.Fatalf("land claim: %+v", c)
	}
	if _, err := s.MainCI(ctx, runner, culprit.ID, redData()); err != nil {
		t.Fatal(err)
	}
	sha := "ffffffffffffffffffffffffffffffffffffff01"
	got, err := s.Advance(ctx, runner, it.ID, pipeline.Merged, map[string]string{"merge_sha": sha, "head_sha": shaA}, []string{"M y.go"})
	if err != nil || got.Stage != pipeline.Done {
		t.Fatalf("merged while frozen: %s %v", got.Stage, err)
	}
	if countEvents(t, s, KindAdvanceQueued) != 0 {
		t.Fatal("a merged advance was queued")
	}
	if _, err := s.MainCI(ctx, runner, it.ID, map[string]any{"result": "green", "merge_sha": sha}); err != nil {
		t.Fatalf("main_ci on the merge: %v", err)
	}
	if fz, _ := s.Freezes(ctx); len(fz) != 0 {
		t.Fatalf("main green on a merge after the red one, still frozen: %+v", fz)
	}
}

// #558: a freeze must not depend on one item id staying open
// (docs/decisions/2026-10-23-freeze-without-one-item.md).

// addMainRed files a main-red P1 the way loopd does (source finding, MainRedTitle).
func addMainRed(t *testing.T, s *Store, repo string) pipeline.Item {
	t.Helper()
	it, err := s.AddItem(context.Background(), "loopd", NewItem{Title: pipeline.MainRedTitle(repo, []string{"test"}),
		Ask: "main's CI failed", Repo: repo, Priority: "P1", Source: "finding"})
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// freezeOf is repo's live freeze (nil: it is not frozen).
func freezeOf(t *testing.T, s *Store, repo string) *Freeze {
	t.Helper()
	all, err := s.Freezes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := range all {
		if all[i].Repo == repo {
			return &all[i]
		}
	}
	return nil
}

// redP1 merges an item, reports a red main on it and returns the repo and the P1 loopd filed.
func redP1(t *testing.T, s *Store) (string, int64) {
	t.Helper()
	culprit := merged(t, s)
	ev, err := s.MainCI(context.Background(), runner, culprit.ID, redData())
	if err != nil {
		t.Fatal(err)
	}
	p1, ok := ev.Data["p1_item"].(float64)
	if !ok {
		t.Fatalf("main_ci: %+v", ev.Data)
	}
	return culprit.Repo, int64(p1)
}

func eventsOfKind(t *testing.T, s *Store, kind string) []Event {
	t.Helper()
	rows, err := s.r.Query(`SELECT `+eventCols+` FROM events WHERE kind = ? ORDER BY id`, kind)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := scanEvents(rows)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// The incident: triage closes the freeze's P1 as a duplicate of the item already fixing the same
// red main. The freeze hands over to that item instead of deadlocking the repo.
func TestFreezeHandsOverWhenItsItemClosesAsADuplicate(t *testing.T) {
	s, _ := open(t)
	repo, p1 := redP1(t, s)
	// The item the P1 duplicates: #539 in the incident, filed for the same red main and already
	// in flight. Frozen, it cannot move while the freeze names the P1.
	live := addIn(t, s, pipeline.MainRedTitle(repo, []string{"test"}), repo, "P1")
	closeNotReproducible(t, s, p1, fmt.Sprintf("duplicate of #%d / PR #133: the same ask is already in flight.", live.ID))

	if got := item(t, s, p1).Stage; got != pipeline.Closed {
		t.Fatalf("P1 #%d is %s", p1, got)
	}
	f := freezeOf(t, s, repo)
	if f == nil || f.ItemID != live.ID {
		t.Fatalf("freeze: %+v, want it on #%d", f, live.ID)
	}
	evs := eventsOfKind(t, s, KindFreezeHandover)
	if len(evs) != 1 || evID(evs[0].Data["from"]) != p1 || evID(evs[0].Data["to"]) != live.ID || evs[0].ItemID != live.ID {
		t.Fatalf("handover events: %+v", evs)
	}
	if evs[0].Data["refiled"] == true {
		t.Errorf("a duplicate closure should hand over, not re-file: %+v", evs[0].Data)
	}
	// The repo moves again: the live item is claimable, and nothing else of the repo is.
	blocked := addIn(t, s, "something else", repo, "P2")
	c := claim(t, s, "triager")
	if c == nil || c.Item.ID != live.ID {
		t.Fatalf("claim: %+v, want #%d", c, live.ID)
	}
	if c := claim(t, s, "triager-2"); c != nil && c.Item.ID == blocked.ID {
		t.Fatalf("claimed #%d: the repo is still frozen", blocked.ID)
	}
}

// Closed for any other reason, with another open main-red P1 of the repo: that one takes it over.
func TestFreezeHandsOverToTheReposOtherMainRedP1(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	repo, p1 := redP1(t, s)
	c := claim(t, s, "triager")
	if c == nil || c.Item.ID != p1 {
		t.Fatalf("claim of the P1: %+v", c)
	}
	other := addMainRed(t, s, repo)
	evidence(t, s, p1)
	if _, err := s.Advance(ctx, runner, p1, pipeline.NotReproducible,
		map[string]string{"summary": "works on main; the failure was the check's environment."}, nil); err != nil {
		t.Fatal(err)
	}
	f := freezeOf(t, s, repo)
	if f == nil || f.ItemID != other.ID {
		t.Fatalf("freeze: %+v, want it on #%d", f, other.ID)
	}
	evs := eventsOfKind(t, s, KindFreezeHandover)
	if len(evs) != 1 || evID(evs[0].Data["to"]) != other.ID || evs[0].Data["refiled"] == true {
		t.Fatalf("handover events: %+v", evs)
	}
	if n := titled(t, s, pipeline.MainRedTitle(repo, []string{"test"})); n != 2 {
		t.Errorf("%d items with the P1's title: a fresh one was filed though the repo had one", n)
	}
}

// Dropped, with nothing else to hand the freeze to: the P1 is re-filed, main stays protected, and
// the repo moves again. A second drop of the fresh one lifts the freeze instead of re-filing for
// ever.
func TestDroppingTheFreezeItemRefilesItThenLifts(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	repo, p1 := redP1(t, s)
	before := freezeOf(t, s, repo)
	if _, err := s.Drop(ctx, user, p1, "reverting by hand"); err != nil {
		t.Fatal(err)
	}
	f := freezeOf(t, s, repo)
	if f == nil {
		t.Fatal("the freeze lifted: merges resume onto a main that is still red")
	}
	fresh := f.ItemID
	if fresh == p1 {
		t.Fatalf("the freeze still names the dropped #%d", p1)
	}
	if !f.RedSince.Equal(before.RedSince) || len(f.Jobs) != 1 || f.Jobs[0] != "test" {
		t.Errorf("freeze lost the red window: %+v, was %+v", f, before)
	}
	it := item(t, s, fresh)
	if !pipeline.IsMainRed(it) || it.Stage != pipeline.Triage || it.Ask != item(t, s, p1).Ask {
		t.Fatalf("re-filed P1: %+v", it)
	}
	evs := eventsOfKind(t, s, KindFreezeHandover)
	if len(evs) != 1 || evID(evs[0].Data["from"]) != p1 || evID(evs[0].Data["to"]) != fresh || evs[0].Data["refiled"] != true {
		t.Fatalf("handover events: %+v", evs)
	}
	if c := claim(t, s, "triager"); c == nil || c.Item.ID != fresh {
		t.Fatalf("claim: %+v, want the re-filed #%d", c, fresh)
	}
	// The user drops that one too: the loop stops re-filing and lifts, saying why.
	if _, err := s.Drop(ctx, user, fresh, "reverted main by hand"); err != nil {
		t.Fatal(err)
	}
	if f := freezeOf(t, s, repo); f != nil {
		t.Fatalf("still frozen on #%d after the re-filed P1 was dropped too", f.ItemID)
	}
	if n := titled(t, s, pipeline.MainRedTitle(repo, []string{"test"})); n != 2 {
		t.Errorf("%d items with the P1's title, want 2 (the original and one re-file)", n)
	}
	un := eventsOfKind(t, s, KindUnfrozen)
	if len(un) != 1 || !strings.Contains(str(un[0].Data, "reason"), "re-filed") {
		t.Fatalf("unfrozen events: %+v", un)
	}
}

// A second red main after a hand unfreeze takes the repo's open P1 over instead of filing a
// byte-identical twin that triage would only close as a duplicate (#558's root cause).
func TestSecondRedTakesTheReposOpenP1(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	repo, p1 := redP1(t, s)
	if _, err := s.Unfreeze(ctx, user, repo); err != nil {
		t.Fatal(err)
	}
	n := 6
	if _, err := s.SetControl(ctx, user, ControlPatch{ActiveAgents: &n, Limits: map[string]int{"triage": 4, "review": 4, "land": 4}}); err != nil {
		t.Fatal(err)
	}
	// Park the P1 on a live lease, as its fix would be, so the next merge's claims leave it alone.
	if c := claim(t, s, "triager"); c == nil || c.Item.ID != p1 {
		t.Fatalf("claim of the P1: %+v", c)
	}
	second := merged(t, s)
	ev, err := s.MainCI(ctx, runner, second.ID, redData())
	if err != nil {
		t.Fatal(err)
	}
	if got := int64(ev.Data["p1_item"].(float64)); got != p1 {
		t.Fatalf("second red filed #%d, want the open #%d reused", got, p1)
	}
	if ev.Data["reused"] != true {
		t.Errorf("main_ci data: %+v", ev.Data)
	}
	if f := freezeOf(t, s, repo); f == nil || f.ItemID != p1 {
		t.Fatalf("freeze: %+v, want it on #%d", f, p1)
	}
	if n := titled(t, s, pipeline.MainRedTitle(repo, []string{"test"})); n != 1 {
		t.Errorf("%d items with the main-red title, want 1", n)
	}
	// The second freeze is a fresh row (the first was lifted by hand) on the P1 it took over.
	fz := eventsOfKind(t, s, KindFrozen)
	if len(fz) != 2 || fz[1].Data["reused"] != true || evID(fz[1].Data["item"]) != p1 {
		t.Fatalf("frozen events: %+v", fz)
	}
}

func titled(t *testing.T, s *Store, title string) int {
	t.Helper()
	var n int
	if err := s.w.QueryRow(`SELECT COUNT(*) FROM items WHERE title = ?`, title).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// evID is an id out of an event's data (JSON numbers come back as float64).
func evID(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	}
	return 0
}
