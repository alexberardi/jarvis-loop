package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

const orch = "orchestrator:loop"

// stuck puts the item (at triage) in needs_you: a failed run, or a question.
func stuck(t *testing.T, s *Store, id int64, outcome pipeline.Outcome) {
	t.Helper()
	c := claim(t, s, "a")
	if c == nil || c.Item.ID != id {
		t.Fatalf("claim: %+v", c)
	}
	f := map[string]string{"error": "fatal: Unable to create '/w/1/.git/index.lock'"}
	if outcome == pipeline.Question {
		f = map[string]string{"question": "can't run the emulator: skip the screenshots?"}
	}
	if _, err := s.Advance(context.Background(), runner, id, outcome, f, nil); err != nil {
		t.Fatal(err)
	}
}

func newest(t *testing.T, s *Store, id int64) Event {
	t.Helper()
	d, err := s.GetItem(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return d.Events[len(d.Events)-1]
}

func TestOrchAnswerLimits(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := add(t, s, "x", "")
	if _, err := s.OrchAnswer(ctx, orch, it.ID, pipeline.AnswerHarness, "skip it", "standing answer"); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("answer at triage: %v", err)
	}
	stuck(t, s, it.ID, pipeline.Question)
	if _, err := s.OrchAnswer(ctx, orch, it.ID, pipeline.AnswerHarness, "skip it", " "); code(err) != pipeline.CodeMissingField {
		t.Fatalf("no reason: %v", err)
	}
	if _, err := s.OrchAnswer(ctx, orch, it.ID, "product", "skip it", "r"); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("product answer: %v", err)
	}
	if _, err := s.OrchAnswer(ctx, orch, it.ID, pipeline.AnswerHarness, "list it under not verified", "standing answer"); err != nil {
		t.Fatal(err)
	}
	e := newest(t, s, it.ID)
	if e.Kind != "answered" || e.Actor != orch || e.Data["reason"] != "standing answer" || e.Data["answer_kind"] != pipeline.AnswerHarness {
		t.Fatalf("answer event: %+v", e)
	}
	stuck(t, s, it.ID, pipeline.Question)
	if _, err := s.OrchAnswer(ctx, orch, it.ID, pipeline.AnswerHarness, "List it under  not verified", "again"); code(err) != pipeline.CodeLimit {
		t.Fatalf("same answer twice: %v", err)
	}
	if _, err := s.OrchAnswer(ctx, orch, it.ID, pipeline.AnswerHistory, "as answered before: yes", "history"); err != nil {
		t.Fatal(err)
	}
	stuck(t, s, it.ID, pipeline.Question)
	if _, err := s.OrchAnswer(ctx, orch, it.ID, pipeline.AnswerLoop, "evidence goes in .loop/evidence", "third"); code(err) != pipeline.CodeLimit {
		t.Fatalf("third answer on the head: %v", err)
	}
	// The user's answers do not count against it.
	if _, err := s.Answer(ctx, user, it.ID, "fine"); err != nil {
		t.Fatal(err)
	}
	if e := newest(t, s, it.ID); e.Kind != "answered" || e.Actor != user {
		t.Fatalf("refusals must not move it: %+v", e)
	}
}

func TestOrchAnswerRefusesRunFailures(t *testing.T) {
	s, _ := open(t)
	it := add(t, s, "x", "")
	stuck(t, s, it.ID, pipeline.Failed)
	if _, err := s.OrchAnswer(context.Background(), orch, it.ID, pipeline.AnswerLoop, "go on", "r"); code(err) != pipeline.CodeLimit {
		t.Fatalf("answering a run failure: %v", err)
	}
	if e := newest(t, s, it.ID); e.Kind != "refused" || e.Actor != orch || e.Data["action"] != "answer" {
		t.Fatalf("refused event: %+v", e)
	}
}

func TestOrchRetryBackoffAndEscalation(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	it := add(t, s, "x", "")
	if _, err := s.OrchRetry(ctx, orch, it.ID, "git lock"); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("retry at triage: %v", err)
	}
	stuck(t, s, it.ID, pipeline.Question)
	if _, err := s.OrchRetry(ctx, orch, it.ID, "git lock"); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("retry of a question: %v", err)
	}
	if _, err := s.Answer(ctx, user, it.ID, "ok"); err != nil {
		t.Fatal(err)
	}

	for failure, wait := range []time.Duration{2 * time.Minute, 10 * time.Minute} {
		stuck(t, s, it.ID, pipeline.Failed)
		if _, err := s.OrchRetry(ctx, orch, it.ID, ""); code(err) != pipeline.CodeMissingField {
			t.Fatalf("retry without reason: %v", err)
		}
		clk.Advance(wait - time.Second)
		if _, err := s.OrchRetry(ctx, orch, it.ID, "git lock"); code(err) != pipeline.CodeTooSoon {
			t.Fatalf("failure %d: retry inside the backoff: %v", failure+1, err)
		}
		clk.Advance(time.Second)
		got, err := s.OrchRetry(ctx, orch, it.ID, "git lock")
		if err != nil || got.Stage != pipeline.Triage {
			t.Fatalf("failure %d: retry: %+v %v", failure+1, got, err)
		}
		e := newest(t, s, it.ID)
		if e.Kind != "retried" || e.Data["reason"] != "git lock" || e.Data["failure"] != float64(failure+1) {
			t.Fatalf("retry event: %+v", e)
		}
	}
	stuck(t, s, it.ID, pipeline.Failed)
	clk.Advance(time.Hour)
	if _, err := s.OrchRetry(ctx, orch, it.ID, "git lock"); code(err) != pipeline.CodeLimit {
		t.Fatalf("third failure on the head: %v", err)
	}
	if e := newest(t, s, it.ID); e.Kind != KindOrchEscalated || e.Actor != "loopd" || e.Data["failure"] != float64(3) {
		t.Fatalf("third failure must escalate: %+v", e)
	}
	// Retried again, it is still refused, but loopd escalates once per head.
	if _, err := s.OrchRetry(ctx, orch, it.ID, "git lock"); code(err) != pipeline.CodeLimit {
		t.Fatalf("third failure again: %v", err)
	}
	d, _ := s.GetItem(ctx, it.ID)
	n := 0
	for _, e := range d.Events {
		if e.Kind == KindOrchEscalated && e.Actor == "loopd" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d loopd escalations, want 1", n)
	}
	if d, _ := s.GetItem(ctx, it.ID); d.Item.Stage != pipeline.NeedsYou {
		t.Fatalf("stage %s", d.Item.Stage)
	}
}

func TestOrchPriority(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := add(t, s, "x", "P3")
	if _, err := s.OrchPriority(ctx, orch, it.ID, "P1", ""); code(err) != pipeline.CodeMissingField {
		t.Fatalf("no reason: %v", err)
	}
	if _, err := s.OrchPriority(ctx, orch, it.ID, "P6", "r"); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("P6: %v", err)
	}
	got, err := s.OrchPriority(ctx, orch, it.ID, "P1", "blocks three items")
	if err != nil || got.Priority != "P1" {
		t.Fatalf("%+v %v", got, err)
	}
	if e := newest(t, s, it.ID); e.Kind != "patched" || e.Data["reason"] != "blocks three items" {
		t.Fatalf("event %+v", e)
	}
}

func TestOrchLink(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	done := toDone(t, s, runner)
	a, b, c := add(t, s, "a", ""), add(t, s, "b", ""), add(t, s, "c", "")
	if _, err := s.Drop(ctx, user, c.ID, "no"); err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct {
		id, after int64
		code      string
	}{{a.ID, c.ID, pipeline.CodeInvalidField}, {a.ID, done.ID, pipeline.CodeInvalidField}, {a.ID, 99, pipeline.CodeInvalidField}, {a.ID, a.ID, pipeline.CodeInvalidField}, {c.ID, a.ID, pipeline.CodeWrongStage}} {
		if _, err := s.OrchLink(ctx, orch, x.id, x.after, "r"); code(err) != x.code {
			t.Fatalf("link #%d after #%d: %v, want %s", x.id, x.after, err, x.code)
		}
	}
	got, err := s.OrchLink(ctx, orch, a.ID, b.ID, "a needs b's route")
	if err != nil || len(got.DependsOn) != 1 || got.DependsOn[0] != b.ID {
		t.Fatalf("%+v %v", got, err)
	}
	if e := newest(t, s, a.ID); e.Kind != KindLinked || e.Data["reason"] != "a needs b's route" {
		t.Fatalf("event %+v", e)
	}
	if _, err := s.OrchLink(ctx, orch, b.ID, a.ID, "r"); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("cycle: %v", err)
	}
	if d, _ := s.GetItem(ctx, b.ID); len(d.Item.DependsOn) != 0 {
		t.Fatalf("a refused link was kept: %v", d.Item.DependsOn)
	}
}

func TestOrchAddLimitAndDedupe(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	n := NewItem{Title: "git index.lock on every run", Repo: "jarvis-server", Priority: "P1"}
	if _, _, err := s.OrchAdd(ctx, orch, n, "", "r"); code(err) != pipeline.CodeMissingField {
		t.Fatalf("no signature: %v", err)
	}
	first, dup, err := s.OrchAdd(ctx, orch, n, "fatal: Unable to create '/w/71-build/.git/index.lock'", "two items")
	if err != nil || dup || first.Source != "finding" {
		t.Fatalf("%+v %v %v", first, dup, err)
	}
	again, dup, err := s.OrchAdd(ctx, orch, n, "fatal: unable to create '/w/68-build/.git/index.lock'", "a third item")
	if err != nil || !dup || again.ID != first.ID {
		t.Fatalf("same signature: %+v %v %v", again, dup, err)
	}
	if e := newest(t, s, first.ID); e.Kind != KindOrchSeenAgain || e.Data["reason"] != "a third item" {
		t.Fatalf("event %+v", e)
	}
	for i := 2; i <= pipeline.OrchAddsPerDay; i++ {
		if _, dup, err := s.OrchAdd(ctx, orch, n, fmt.Sprintf("error kind %c", 'a'+i), "r"); err != nil || dup {
			t.Fatalf("add %d: %v %v", i, dup, err)
		}
	}
	if _, _, err := s.OrchAdd(ctx, orch, n, "error kind z", "r"); code(err) != pipeline.CodeLimit {
		t.Fatalf("sixth add in a day: %v", err)
	}
	// A dropped item's signature files a new one; a day later the limit is free again.
	if _, err := s.Drop(ctx, user, first.ID, "fixed"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(24 * time.Hour)
	if got, dup, err := s.OrchAdd(ctx, orch, n, "fatal: unable to create '/x/.git/index.lock'", "back"); err != nil || dup || got.ID == first.ID {
		t.Fatalf("after drop: %+v %v %v", got, dup, err)
	}
}

func TestOrchEscalateRate(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	it := add(t, s, "x", "")
	if _, err := s.OrchEscalate(ctx, orch, it.ID, "", "rec", "r"); code(err) != pipeline.CodeMissingField {
		t.Fatalf("no text: %v", err)
	}
	if _, err := s.OrchEscalate(ctx, orch, it.ID, "product call", "do A", "not mine"); err != nil {
		t.Fatal(err)
	}
	if e := newest(t, s, it.ID); e.Kind != KindOrchEscalated || e.Data["recommendation"] != "do A" || e.Data["reason"] != "not mine" {
		t.Fatalf("event %+v", e)
	}
	// An escalation about an item held for approval is not rate limited, and neither counts nor
	// resets the window.
	held := add(t, s, "held", "")
	if _, err := s.w.Exec(`UPDATE items SET stage = ? WHERE id = ?`, string(pipeline.Held), held.ID); err != nil {
		t.Fatal(err)
	}
	clk.Advance(5 * time.Minute)
	if _, err := s.OrchEscalate(ctx, orch, held.ID, "merge #2?", "approve", "held on auth"); err != nil {
		t.Fatalf("approval escalation inside the window: %v", err)
	}
	if e := newest(t, s, held.ID); e.Kind != KindOrchEscalated || e.Data["approval"] != true {
		t.Fatalf("approval event %+v", e)
	}
	clk.Advance(4 * time.Minute)
	if _, err := s.OrchEscalate(ctx, orch, 0, "another", "do B", "r"); code(err) != pipeline.CodeTooSoon {
		t.Fatalf("second within 10 min: %v", err)
	}
	clk.Advance(time.Minute)
	if _, err := s.OrchEscalate(ctx, orch, 0, "another", "do B", "r"); err != nil {
		t.Fatal(err)
	}
}

func TestOrchAgentsDownOnly(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	four := 4
	if _, err := s.SetControl(ctx, user, ControlPatch{ActiveAgents: &four}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{4, 5} {
		if _, err := s.OrchAgentsDown(ctx, orch, n, "r"); code(err) != pipeline.CodeLimit {
			t.Fatalf("agents %d: %v", n, err)
		}
	}
	if _, err := s.OrchAgentsDown(ctx, orch, 1, ""); code(err) != pipeline.CodeMissingField {
		t.Fatalf("no reason: %v", err)
	}
	c, err := s.OrchAgentsDown(ctx, orch, 1, "every run fails on the git lock")
	if err != nil || c.ActiveAgents != 1 {
		t.Fatalf("%+v %v", c, err)
	}
	e, ok, _ := s.LastEvent(ctx, "control")
	if !ok || e.Actor != orch || e.Data["reason"] != "every run fails on the git lock" || e.Data["ping"] != true {
		t.Fatalf("event %+v", e)
	}
	if _, err := s.OrchAgentsDown(ctx, orch, 2, "r"); code(err) != pipeline.CodeLimit {
		t.Fatalf("back up: %v", err)
	}
}

func TestOrchNotebook(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if _, err := s.OrchNote(ctx, orch, "watching #35", ""); code(err) != pipeline.CodeMissingField {
		t.Fatalf("no reason: %v", err)
	}
	for _, n := range []string{"watching #35", "git lock on #36 too"} {
		if _, err := s.OrchNote(ctx, orch, n, "pattern"); err != nil {
			t.Fatal(err)
		}
	}
	nb, err := s.Notebook(ctx, 1)
	if err != nil || len(nb) != 1 || nb[0].Data["text"] != "git lock on #36 too" {
		t.Fatalf("%+v %v", nb, err)
	}
	if nb, _ = s.Notebook(ctx, 0); len(nb) != 2 || nb[0].Data["text"] != "watching #35" {
		t.Fatalf("%+v", nb)
	}
}

func TestUseApproverMessage(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	const approver, other = "U0APPROVER", "U0OTHER"
	if err := s.RecordSlackMessage(ctx, "C1", "1.1", approver, "drop #1 and #2"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSlackMessage(ctx, "C1", "1.2", other, "drop #1"); err != nil {
		t.Fatal(err)
	}
	use := func(ts, action, target string) string {
		_, err := s.UseApproverMessage(ctx, "C1", ts, approver, action, target, orch)
		return code(err)
	}
	if c := use("9.9", "drop", "1"); c != CodeNotApprover {
		t.Fatalf("a message loopd never saw: %s", c)
	}
	if c := use("1.2", "drop", "1"); c != CodeNotApprover {
		t.Fatalf("someone else's message: %s", c)
	}
	if _, err := s.UseApproverMessage(ctx, "C1", "1.1", "", "drop", "1", orch); code(err) != CodeNotApprover {
		t.Fatalf("no approver configured: %v", err)
	}
	if c := use("1.1", "drop", "1"); c != "" {
		t.Fatalf("first use: %s", c)
	}
	if c := use("1.1", "drop", "2"); c != "" {
		t.Fatalf("the same message, another item: %s", c)
	}
	if c := use("1.1", "drop", "1"); c != CodeUsed {
		t.Fatalf("reused: %s", c)
	}
	clk.Advance(OnBehalfWindow + time.Second)
	if c := use("1.1", "hold", "1"); c != CodeNotApprover {
		t.Fatalf("an old message: %s", c)
	}
}

func TestWithViaMarksEvents(t *testing.T) {
	s, _ := open(t)
	ctx := WithVia(context.Background(), orch)
	it := add(t, s, "x", "")
	if _, err := s.Drop(ctx, "slack:U0APPROVER", it.ID, "drop #1"); err != nil {
		t.Fatal(err)
	}
	if e := newest(t, s, it.ID); e.Kind != "dropped" || e.Actor != "slack:U0APPROVER" || e.Data["via"] != orch {
		t.Fatalf("event %+v", e)
	}
	if e := newest(t, s, it.ID); e.Data["via"] == nil {
		t.Fatal("no via")
	}
	// Without it, nothing says via.
	it2 := add(t, s, "y", "")
	if e := newest(t, s, it2.ID); e.Data["via"] != nil {
		t.Fatalf("via leaked into the next transaction: %+v", e)
	}
}
