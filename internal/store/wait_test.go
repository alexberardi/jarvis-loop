package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// b is parked in review on a; a's merge sends b back to review, with the note, recorded by loopd.
func TestWaitReleasedWhenTheDependencyMerges(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	var b pipeline.Item
	a := toMerged(t, s, runner, func(a pipeline.Item) {
		b = add(t, s, "b", "P5")
		setStage(t, s, b.ID, pipeline.Review)
		got, err := s.Wait(ctx, user, b.ID, []int64{a.ID}, nil, "needs a's schema")
		if err != nil || got.Stage != pipeline.Waiting || got.ReturnStage != pipeline.Review || !got.WaitingSince.Equal(t0) {
			t.Fatalf("wait: %+v %v", got, err)
		}
		rows, _ := s.ListItems(ctx, "waiting", "")
		if len(rows) != 1 || rows[0].WaitsOn != "#1" || rows[0].Display != "waiting" {
			t.Fatalf("board: %+v", rows)
		}
		clk.Advance(time.Minute)
	}, nil)
	got := item(t, s, b.ID)
	if got.Stage != pipeline.Review || got.ReturnStage != "" || !got.WaitingSince.IsZero() {
		t.Fatalf("b after a merged: %+v", got)
	}
	e := lastEvent(t, s, b.ID, "released")
	if e == nil || e.Actor != "loopd" || e.From != "waiting" || e.To != "review" || str(e.Data, "note") != "#1 done: back to review" {
		t.Fatalf("released: %+v", e)
	}
	if w := lastEvent(t, s, b.ID, "waiting"); str(w.Data, "reason") != "needs a's schema" || str(w.Data, "waits_on") != "#1" {
		t.Fatalf("waiting event: %+v", w)
	}
	_ = a
}

// A dependency dropped while an item waits on it sends the item to the user; answered, it goes
// back to the stage it left.
func TestWaitDependencyDropped(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	c := add(t, s, "c", "")
	d := add(t, s, "d", "")
	setStage(t, s, d.ID, pipeline.Land)
	if _, err := s.Wait(ctx, user, d.ID, []int64{c.ID}, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Drop(ctx, user, c.ID, "not now"); err != nil {
		t.Fatal(err)
	}
	got := item(t, s, d.ID)
	if got.Stage != pipeline.NeedsYou || got.ReturnStage != pipeline.Land || !strings.HasPrefix(got.Question, "#1 it depends on was dropped") {
		t.Fatalf("d: %+v", got)
	}
	if got, err := s.Answer(ctx, user, d.ID, "keep it"); err != nil || got.Stage != pipeline.Land {
		t.Fatalf("answer: %+v %v", got, err)
	}
}

// A wait that would make a cycle is refused, and the item stays where it was.
func TestWaitCycleRefused(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	a := add(t, s, "a", "")
	b := add(t, s, "b", "")
	setStage(t, s, a.ID, pipeline.Build)
	setStage(t, s, b.ID, pipeline.Build)
	if _, err := s.Wait(ctx, user, b.ID, []int64{a.ID}, nil, ""); err != nil {
		t.Fatal(err)
	}
	_, err := s.Wait(ctx, user, a.ID, []int64{b.ID}, nil, "")
	if code(err) != pipeline.CodeInvalidField || !strings.Contains(err.Error(), "makes a cycle: #1 → #2 → #1") {
		t.Fatalf("cycle: %v", err)
	}
	if got := item(t, s, a.ID); got.Stage != pipeline.Build || len(got.DependsOn) != 0 {
		t.Fatalf("a after the refused wait: %+v", got)
	}
	// Refused too: an item a run holds, a stage that cannot wait, nothing to wait on.
	if _, err := s.Wait(ctx, user, a.ID, nil, nil, ""); code(err) != pipeline.CodeMissingField {
		t.Fatalf("nothing: %v", err)
	}
	setStage(t, s, a.ID, pipeline.Held)
	if _, err := s.Wait(ctx, user, a.ID, nil, []string{"o/n#1"}, ""); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("held: %v", err)
	}
	setStage(t, s, a.ID, pipeline.Build)
	if c := claim(t, s, "builder"); c == nil || c.Item.ID != a.ID {
		t.Fatalf("claim: %+v", c)
	}
	if _, err := s.Wait(ctx, user, a.ID, nil, []string{"o/n#1"}, ""); code(err) != pipeline.CodeWrongStage || !strings.Contains(err.Error(), "a run holds") {
		t.Fatalf("leased: %v", err)
	}
}

// A PR wait: the runner's report of the merge releases the item; of a close, asks the user.
func TestWaitOnPR(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	a := add(t, s, "a", "")
	setStage(t, s, a.ID, pipeline.Build)
	got, err := s.Wait(ctx, user, a.ID, nil, []string{"#444", "o/other#5"}, "main's test fix")
	if err != nil || len(got.WaitPRs) != 2 || got.WaitPRs[0].Ref != "jarvis-server#444" {
		t.Fatalf("wait: %+v %v", got, err)
	}
	rows, _ := s.ListItems(ctx, "waiting", "")
	if len(rows) != 1 || rows[0].WaitsOn != "jarvis-server#444, o/other#5" || len(rows[0].WaitPRs) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	if got, err := s.WaitPR(ctx, "runner:"+runner, a.ID, "o/other#5", "MERGED"); err != nil || got.Stage != pipeline.Waiting {
		t.Fatalf("first merge: %+v %v", got, err)
	}
	if _, err := s.WaitPR(ctx, "runner:"+runner, a.ID, "jarvis-server#444", "MERGED"); err != nil {
		t.Fatal(err)
	}
	if got := item(t, s, a.ID); got.Stage != pipeline.Build {
		t.Fatalf("after both merged: %+v", got)
	}
	if e := lastEvent(t, s, a.ID, "released"); str(e.Data, "note") != "jarvis-server#444 merged, o/other#5 merged: back to build" {
		t.Fatalf("note: %+v", e.Data)
	}

	b := add(t, s, "b", "")
	setStage(t, s, b.ID, pipeline.Review)
	if _, err := s.Wait(ctx, user, b.ID, nil, []string{"o/n#9"}, ""); err != nil {
		t.Fatal(err)
	}
	got, err = s.WaitPR(ctx, "runner:"+runner, b.ID, "o/n#9", "CLOSED")
	if err != nil || got.Stage != pipeline.NeedsYou || got.ReturnStage != pipeline.Review || !strings.Contains(got.Question, "closed without merging") {
		t.Fatalf("closed: %+v %v", got, err)
	}
}

// An agent parks its own item with the wait outcome; a wait loopd refuses goes to the user.
func TestAgentWait(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	a := add(t, s, "a", "P1")
	b := add(t, s, "b", "P2")
	setStage(t, s, a.ID, pipeline.Build)
	setStage(t, s, b.ID, pipeline.Build)
	if c := claim(t, s, "builder"); c == nil || c.Item.ID != a.ID {
		t.Fatalf("claim: %+v", c)
	}
	got, err := s.Advance(ctx, runner, a.ID, pipeline.Wait, map[string]string{"on": "#2", "reason": "b first"}, nil)
	if err != nil || got.Stage != pipeline.Waiting || got.ReturnStage != pipeline.Build {
		t.Fatalf("agent wait: %+v %v", got, err)
	}
	if d, _ := s.GetItem(ctx, a.ID); d.Lease != nil {
		t.Fatalf("lease still live: %+v", d.Lease)
	}
	if e := lastEvent(t, s, a.ID, "waiting"); e.Actor != "runner:"+runner || e.Outcome != "wait" || str(e.Data, "agent") != "builder" {
		t.Fatalf("event: %+v", e)
	}
	// b's agent asks to wait on a: a cycle, so b goes to the user with the reason.
	if c := claim(t, s, "builder"); c == nil || c.Item.ID != b.ID {
		t.Fatalf("claim b: %+v", c)
	}
	got, err = s.Advance(ctx, runner, b.ID, pipeline.Wait, map[string]string{"on": "1", "reason": "a first"}, nil)
	if err != nil || got.Stage != pipeline.NeedsYou || got.ReturnStage != pipeline.Build || !strings.Contains(got.Question, "makes a cycle") {
		t.Fatalf("refused agent wait: %+v %v", got, err)
	}
	if len(item(t, s, b.ID).DependsOn) != 0 {
		t.Fatal("the refused wait linked b")
	}
}
