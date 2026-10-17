package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// A message to the orchestrator is kept (so acting for the user can name it) and becomes one
// orchestrator.message event, the trigger for its turn; the same message twice is one event.
func TestOrchMessageRecordsOnce(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	m := OrchestratorMessage{Channel: "D1", TS: "1.1", User: "U0APPROVER", Text: "drop #61", Approver: true, ItemID: 7}
	e, fresh, err := s.OrchMessage(ctx, m)
	if err != nil || !fresh {
		t.Fatalf("first: %v %v", fresh, err)
	}
	if e.Kind != KindOrchMessage || e.ItemID != 0 || e.Data["channel"] != "D1" || e.Data["ts"] != "1.1" || e.Data["text"] != "drop #61" ||
		e.Data["user"] != "U0APPROVER" || e.Data["approver"] != true || e.Data["item_id"] != float64(7) {
		t.Fatalf("event %+v", e)
	}
	if _, fresh, err := s.OrchMessage(ctx, m); err != nil || fresh {
		t.Fatalf("again: %v %v", fresh, err)
	}
	if _, err := s.UseApproverMessage(ctx, "D1", "1.1", "U0APPROVER", "drop", "61", orch); err != nil {
		t.Fatalf("the recorded message acts for the user: %v", err)
	}
	if _, _, err := s.OrchMessage(ctx, OrchestratorMessage{Channel: "D1", TS: "1.2", User: "U0OTHER", Text: "drop #61"}); err != nil {
		t.Fatal(err)
	}
	all, err := s.OrchMessages(ctx, 0, 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("messages: %d %v", len(all), err)
	}
	if all[1].Data["approver"] != false {
		t.Fatalf("someone else's: %+v", all[1])
	}
	since, err := s.OrchMessages(ctx, all[0].ID, 0)
	if err != nil || len(since) != 1 || since[0].ID != all[1].ID {
		t.Fatalf("since: %+v %v", since, err)
	}
	if one, _ := s.OrchMessages(ctx, 0, 1); len(one) != 1 || one[0].ID != all[0].ID {
		t.Fatalf("limit: %+v", one)
	}
}

func TestOrchReply(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if _, err := s.OrchReply(ctx, orch, "D1", "1.1", " "); code(err) != pipeline.CodeMissingField {
		t.Fatalf("no text: %v", err)
	}
	if _, err := s.OrchReply(ctx, orch, "", "1.1", "hi"); code(err) != pipeline.CodeMissingField {
		t.Fatalf("no channel: %v", err)
	}
	e, err := s.OrchReply(ctx, orch, "D1", "1.1", " #35 waits on #34. ")
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != KindOrchReply || e.Actor != orch || e.Data["channel"] != "D1" || e.Data["thread_ts"] != "1.1" || e.Data["text"] != "#35 waits on #34." {
		t.Fatalf("event %+v", e)
	}
}

// The orchestrator never approves: asked to, it has the approval message posted again, for an item
// held for approval only.
func TestOrchAskApproval(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := add(t, s, "x", "")
	if _, err := s.OrchAskApproval(ctx, orch, it.ID, "the user said approve #1"); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("not held: %v", err)
	}
	if e := newest(t, s, it.ID); e.Kind != "refused" || e.Data["action"] != "approval" {
		t.Fatalf("refusal: %+v", e)
	}
	if _, err := s.w.Exec(`UPDATE items SET stage = ?, head_sha = 'abc123' WHERE id = ?`, string(pipeline.Held), it.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OrchAskApproval(ctx, orch, it.ID, ""); code(err) != pipeline.CodeMissingField {
		t.Fatalf("no reason: %v", err)
	}
	e, err := s.OrchAskApproval(ctx, orch, it.ID, "the user said approve #1")
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != KindOrchApprovalAsked || e.ItemID != it.ID || e.Data["head_sha"] != "abc123" || e.Data["reason"] != "the user said approve #1" {
		t.Fatalf("event %+v", e)
	}
	if d, _ := s.GetItem(ctx, it.ID); d.Item.Stage != pipeline.Held {
		t.Fatalf("stage %s", d.Item.Stage)
	}
}

// Accept records the escalation's recommendation as the user's answer, marked as accepted; never
// for an escalation the item has moved past.
func TestAcceptRecommendation(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	const you = "slack:U0APPROVER"
	it := add(t, s, "x", "")
	stuck(t, s, it.ID, pipeline.Question)
	esc, err := s.OrchEscalate(ctx, orch, it.ID, "no emulator", "open the PR without the PNGs", "a product call")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptRecommendation(ctx, you, 9999); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("no such escalation: %v", err)
	}
	got, err := s.AcceptRecommendation(ctx, you, esc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stage != pipeline.Triage {
		t.Fatalf("stage %s", got.Stage)
	}
	e := newest(t, s, it.ID)
	if e.Kind != "answered" || e.Actor != you || e.Data["answer"] != "open the PR without the PNGs" || e.Data["accepted"] != float64(esc.ID) {
		t.Fatalf("event %+v", e)
	}
	// Pressed again: the item is no longer waiting on it.
	if _, err := s.AcceptRecommendation(ctx, you, esc.ID); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("twice: %v", err)
	}
	// The item asks something new: the old recommendation does not answer it.
	stuck(t, s, it.ID, pipeline.Question)
	if _, err := s.AcceptRecommendation(ctx, you, esc.ID); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("stale: %v", err)
	}
	// An escalation with no recommendation, or about no item, has nothing to accept.
	clkless, err := s.OrchNote(ctx, orch, "n", "r")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptRecommendation(ctx, you, clkless.ID); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("not an escalation: %v", err)
	}
}

// Asked to OK trusted files, it has the item's trust message posted again, for an item waiting on
// that OK only: a plain question is refused, and nothing is trusted.
func TestOrchAskTrust(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it, err := s.AddItem(ctx, user, NewItem{Title: "trusted"})
	if err != nil {
		t.Fatal(err)
	}
	it.Stage, it.ReturnStage, it.Question = pipeline.NeedsYou, pipeline.Build, "verify refused"
	if err := s.tx(ctx, func(tx *sql.Tx) error { return saveItem(tx, it) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OrchAskApproval(ctx, orch, it.ID, "the user said OK the trusted files"); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("a plain question: %v", err)
	}
	sha := "0123456789abcdef0123456789abcdef01234567"
	it.Trust = &pipeline.Trust{HeadSHA: sha, Paths: []string{"tools/pipeline", "scripts/check"}, Diffstat: "tools/pipeline | 2 +-"}
	if err := s.tx(ctx, func(tx *sql.Tx) error { return saveItem(tx, it) }); err != nil {
		t.Fatal(err)
	}
	e, err := s.OrchAskApproval(ctx, orch, it.ID, "the user said OK the trusted files")
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != KindOrchApprovalAsked || e.Data["trust"] != true || e.Data[pipeline.FieldTrustSHA] != sha ||
		e.Data[pipeline.FieldTrustPaths] != "tools/pipeline,scripts/check" || e.Data[pipeline.FieldTrustDiffstat] != "tools/pipeline | 2 +-" {
		t.Fatalf("event %+v", e)
	}
	esc, err := s.OrchEscalate(ctx, orch, it.ID, "verify refused tools/pipeline", "OK it", "the item asks for it")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptRecommendation(ctx, user, esc.ID); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("Accept as the OK: %v", err)
	}
	if d, _ := s.GetItem(ctx, it.ID); d.Item.Stage != pipeline.NeedsYou || d.Item.Trust.By != "" {
		t.Fatalf("trusted: %+v", d.Item)
	}
	if _, err := s.Trust(ctx, user, it.ID, sha); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OrchAskApproval(ctx, orch, it.ID, "again"); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("after the OK: %v", err)
	}
}
