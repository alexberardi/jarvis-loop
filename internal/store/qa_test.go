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

// QA after deploy (docs/decisions/2026-10-16-qa-after-deploy.md): the land script's
// qa_after_deploy puts the item in qa awaiting a deploy, listed for the runners; a runner's
// deployed ends the wait once, with no refusal recorded for a second report.
func TestMergedAwaitsDeployThenDeployed(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := toMerged(t, s, runner, nil, map[string]string{"qa_after_deploy": "true"})
	d, _ := s.GetItem(ctx, it.ID)
	if d.Item.Stage != pipeline.QA || !d.Item.NeedsQA || !d.Item.AwaitingDeploy {
		t.Fatalf("after merged: %+v", d.Item)
	}
	rows, err := s.ListItemsFilter(ctx, ListFilter{AwaitingDeploy: true})
	if err != nil || len(rows) != 1 || rows[0].ID != it.ID {
		t.Fatalf("awaiting deploy list: %+v %v", rows, err)
	}
	rev := "abcdef0123456789abcdef0123456789abcdef01"
	if _, err := s.Deployed(ctx, "other-runner", it.ID, map[string]any{"revision": "nope"}); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("bad revision: %v", err)
	}
	if _, err := s.Deployed(ctx, "other-runner", it.ID, map[string]any{"revision": rev, "x": "y"}); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("extra data: %v", err)
	}
	ev, err := s.Deployed(ctx, "other-runner", it.ID, map[string]any{"revision": rev})
	if err != nil || ev.Kind != "deployed" || ev.Actor != "runner:other-runner" || ev.Data["revision"] != rev {
		t.Fatalf("deployed: %+v %v", ev, err)
	}
	d, _ = s.GetItem(ctx, it.ID)
	if d.Item.Stage != pipeline.QA || d.Item.AwaitingDeploy {
		t.Fatalf("after deployed: %+v", d.Item)
	}
	if rows, _ := s.ListItemsFilter(ctx, ListFilter{AwaitingDeploy: true}); len(rows) != 0 {
		t.Fatalf("still listed: %+v", rows)
	}
	if _, err := s.Deployed(ctx, runner, it.ID, map[string]any{"revision": rev}); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("second deployed: %v", err)
	}
	if ev := lastEvent(t, s, it.ID, "refused"); ev != nil {
		t.Fatalf("a runner's late deployed is not a refusal: %+v", ev)
	}
	if _, err := s.Verified(ctx, user, it.ID, ""); err != nil {
		t.Fatal(err)
	}
}
