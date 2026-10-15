package store

import (
	"context"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

func lastEvent(t *testing.T, s *Store, id int64, kind string) *Event {
	t.Helper()
	d, err := s.GetItem(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(d.Events) - 1; i >= 0; i-- {
		if d.Events[i].Kind == kind {
			return &d.Events[i]
		}
	}
	return nil
}

// The user marks an item needs_qa before it lands; merged then puts it in qa, and the flag
// survives the store.
func TestSetQAThenMergedGoesToQA(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := toMerged(t, s, runner, func(it pipeline.Item) {
		got, err := s.SetQA(ctx, user, it.ID)
		if err != nil || !got.NeedsQA {
			t.Fatalf("set qa: %+v %v", got, err)
		}
	}, nil)
	d, _ := s.GetItem(ctx, it.ID)
	if d.Item.Stage != pipeline.QA || !d.Item.NeedsQA || d.Display != "qa" {
		t.Fatalf("after merged: %+v (%s)", d.Item, d.Display)
	}
	if ev := lastEvent(t, s, it.ID, "qa_marked"); ev == nil || ev.Actor != user {
		t.Fatalf("qa_marked event: %+v", ev)
	}
	if _, err := s.SetQA(ctx, user, it.ID); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("set qa in qa: %v", err)
	}
	// Nothing claims it.
	if c, err := s.Claim(ctx, runner, pipeline.ClaimRequest{Runner: runner, Agent: "x"}); err != nil || c != nil {
		t.Fatalf("claimed a qa item: %+v %v", c, err)
	}
	// main's CI still records on a qa item, for the runner that merged it.
	if _, err := s.MainCI(ctx, runner, it.ID, map[string]any{"result": "green"}); err != nil {
		t.Fatalf("main_ci on a qa item: %v", err)
	}
}

// The land script's needs_qa (the repo's qa: true) does the same.
func TestMergedNeedsQAField(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := toMerged(t, s, runner, nil, map[string]string{"needs_qa": "true"})
	d, _ := s.GetItem(ctx, it.ID)
	if d.Item.Stage != pipeline.QA {
		t.Fatalf("stage %s", d.Item.Stage)
	}
	got, err := s.Verified(ctx, user, it.ID, "Verified button")
	if err != nil || got.Stage != pipeline.Done {
		t.Fatalf("verified: %+v %v", got, err)
	}
	if ev := lastEvent(t, s, it.ID, "verified"); ev == nil || ev.From != "qa" || ev.To != "done" || ev.Data["words"] != "Verified button" {
		t.Fatalf("verified event: %+v", ev)
	}
	if _, err := s.Verified(ctx, user, it.ID, ""); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("second verified: %v", err)
	}
}

// Reopen files a P2 item linked to the qa item, with the words as its ask, in one transaction.
func TestReopen(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := toMerged(t, s, runner, nil, map[string]string{"needs_qa": "true"})
	if _, _, err := s.Reopen(ctx, user, it.ID, "  "); code(err) != pipeline.CodeMissingField {
		t.Fatalf("reopen without words: %v", err)
	}
	if ev := lastEvent(t, s, it.ID, "refused"); ev == nil || ev.Data["action"] != "reopen" {
		t.Fatalf("refusal not recorded: %+v", ev)
	}
	old, n, err := s.Reopen(ctx, user, it.ID, "the code is still small on the phone")
	if err != nil || old.Stage != pipeline.Done {
		t.Fatalf("reopen: %+v %v", old, err)
	}
	d, _ := s.GetItem(ctx, n.ID)
	if n.ID == it.ID || d.Item.ReopenOf != it.ID || d.Item.Priority != "P2" || d.Item.Stage != pipeline.Triage ||
		d.Item.Ask != "the code is still small on the phone" || d.Item.Repo != old.Repo || old.Repo == "" {
		t.Fatalf("new item: %+v", d.Item)
	}
	ev := lastEvent(t, s, it.ID, "reopened")
	if ev == nil || ev.From != "qa" || ev.To != "done" || ev.Data["reopen_item"] != float64(n.ID) {
		t.Fatalf("reopened event: %+v", ev)
	}
	if ev := lastEvent(t, s, n.ID, "created"); ev == nil || ev.Data["reopen_of"] != float64(it.ID) || ev.Data["priority"] != "P2" {
		t.Fatalf("created event: %+v", ev)
	}
	if _, _, err := s.Reopen(ctx, user, it.ID, "again"); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("reopen a done item: %v", err)
	}
	// An item that is not in qa files nothing.
	other := add(t, s, "open", "")
	if _, _, err := s.Reopen(ctx, user, other.ID, "x"); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("reopen a triage item: %v", err)
	}
	rows, _ := s.ListItems(ctx, "", "")
	if len(rows) != 3 {
		t.Fatalf("items: %d", len(rows))
	}
}
