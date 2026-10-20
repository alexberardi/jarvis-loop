package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// nightOn sets the night shift's mode.
func nightMode(t *testing.T, s *Store, mode string, off ...string) {
	t.Helper()
	p := ControlPatch{NightShift: &mode}
	if off != nil {
		p.NightClassesOff = &off
	}
	if _, err := s.SetControl(context.Background(), user, p); err != nil {
		t.Fatal(err)
	}
}

// toNeedsYou takes a new item through triage and a build (PR on shaA) to the user: the second of
// two bounces, the last one last (pipeline.Bounce or pipeline.VerifyFailed).
func toNeedsYou(t *testing.T, s *Store, last pipeline.Outcome) pipeline.Item {
	t.Helper()
	ctx := context.Background()
	it := add(t, s, "x", "")
	step := func(agent string, out pipeline.Outcome, f map[string]string, paths []string, ev bool) pipeline.Item {
		t.Helper()
		c, err := s.Claim(ctx, runner, pipeline.ClaimRequest{Runner: runner, Agent: agent})
		if err != nil || c == nil || c.Item.ID != it.ID {
			t.Fatalf("claim for %s: %+v %v", out, c, err)
		}
		if ev {
			if _, err := s.AddEvidence(ctx, runner, it.ID, "log", "run.log", strings.NewReader("ok\n")); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.Advance(ctx, runner, it.ID, out, f, paths)
		if err != nil {
			t.Fatalf("%s: %v", out, err)
		}
		return got
	}
	step("triager", pipeline.Reproduced, map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": ""}, nil, true)
	pr := map[string]string{"repo": "o/r", "pr": "7", "branch": "loop/1-x", "head_sha": shaA}
	step("builder", pipeline.PROpened, pr, []string{"M x.go"}, true)
	step("reviewer", pipeline.Bounce, map[string]string{"notes": "fix the nil check in handler.go"}, nil, false)
	if last == pipeline.VerifyFailed {
		it = step("builder", pipeline.VerifyFailed, map[string]string{"reason": "scenario 12 failed"}, nil, true)
	} else {
		step("builder", pipeline.PROpened, pr, []string{"M x.go"}, true)
		it = step("reviewer", pipeline.Bounce, map[string]string{"notes": "still: fix the nil check in handler.go"}, nil, false)
	}
	if it.Stage != pipeline.NeedsYou {
		t.Fatalf("not with the user: %s", it.Stage)
	}
	return it
}

func escalateNight(t *testing.T, s *Store, clk *clock.Fake, id int64, class, rec, deferred string) Event {
	t.Helper()
	clk.Advance(pipeline.OrchEscalateEvery) // past the escalation rate
	e, err := s.OrchEscalateNight(context.Background(), orch, id, "verify failed on a scenario this PR cannot reach", rec,
		"the scenario's area is not in the diff", class, deferred)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func tick(t *testing.T, s *Store) {
	t.Helper()
	if err := s.OrchTick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func nightEvents(t *testing.T, s *Store, id int64, kind string) []Event {
	t.Helper()
	d, err := s.GetItem(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, e := range d.Events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func stage(t *testing.T, s *Store, id int64) pipeline.Stage {
	t.Helper()
	d, err := s.GetItem(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return d.Item.Stage
}

// verify_flake: acted on at the grace period, not a second before; once per head sha.
func TestNightVerifyFlake(t *testing.T) {
	s, clk := open(t)
	nightMode(t, s, pipeline.NightAlways)
	it := toNeedsYou(t, s, pipeline.VerifyFailed)
	esc := escalateNight(t, s, clk, it.ID, pipeline.ClassVerifyFlake, "re-run verify", "")
	if esc.Data["class"] != pipeline.ClassVerifyFlake || esc.Data["night"] != true || esc.Data["act_at"] == nil {
		t.Fatalf("escalation %+v", esc.Data)
	}
	clk.Advance(pipeline.NightGrace - time.Second)
	tick(t, s)
	if st := stage(t, s, it.ID); st != pipeline.NeedsYou {
		t.Fatalf("acted before the grace: %s", st)
	}
	clk.Advance(time.Second)
	tick(t, s)
	if st := stage(t, s, it.ID); st != pipeline.Build {
		t.Fatalf("not acted at the grace: %s", st)
	}
	e := newest(t, s, it.ID)
	if e.Kind != KindOrchNightAction || e.Actor != NightActor || e.Data["class"] != pipeline.ClassVerifyFlake ||
		!strings.Contains(str(e.Data, "action"), "re-run verify") || !strings.Contains(str(e.Data, "undo"), "loop rebuild") ||
		e.Data["escalation"] != float64(esc.ID) || e.Data["head_sha"] != shaA {
		t.Fatalf("night action %+v", e)
	}
	rb := nightEvents(t, s, it.ID, "rebuild")
	if len(rb) != 1 || rb[0].Actor != NightActor || rb[0].Data["night_shift"] != pipeline.ClassVerifyFlake ||
		!strings.Contains(str(rb[0].Data, "notes"), "re-run verify") {
		t.Fatalf("rebuild %+v", rb)
	}
	// Verify fails again on the same head: the next one is the user's.
	c, _ := s.Claim(context.Background(), runner, pipeline.ClaimRequest{Runner: runner, Agent: "builder"})
	if c == nil || c.Item.ID != it.ID {
		t.Fatalf("build claim %+v", c)
	}
	if _, err := s.AddEvidence(context.Background(), runner, it.ID, "log", "v.log", strings.NewReader("FAIL\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(context.Background(), runner, it.ID, pipeline.VerifyFailed, map[string]string{"reason": "scenario 12 failed"}, nil); err != nil {
		t.Fatal(err)
	}
	again := escalateNight(t, s, clk, it.ID, pipeline.ClassVerifyFlake, "re-run verify", "")
	if again.Data["act_at"] != nil {
		t.Fatalf("an escalation the night shift will not act on says when it acts: %+v", again.Data)
	}
	clk.Advance(pipeline.NightGrace)
	tick(t, s)
	tick(t, s)
	if st := stage(t, s, it.ID); st != pipeline.NeedsYou {
		t.Fatalf("second verify flake on the head acted: %s", st)
	}
	stayed := nightEvents(t, s, it.ID, KindOrchNightStayed)
	if len(stayed) != 1 || !strings.Contains(str(stayed[0].Data, "why"), "once per head") {
		t.Fatalf("stayed %+v", stayed)
	}
	if n := len(nightEvents(t, s, it.ID, KindOrchNightAction)); n != 1 {
		t.Fatalf("%d night actions", n)
	}
}

// bounce_limit: one more build with the latest review notes, once per item per night.
func TestNightBounceLimitOncePerNight(t *testing.T) {
	s, clk := open(t)
	nightMode(t, s, pipeline.NightAlways)
	it := toNeedsYou(t, s, pipeline.Bounce)
	escalateNight(t, s, clk, it.ID, pipeline.ClassBounceLimit, "one more build with the notes", "")
	clk.Advance(pipeline.NightGrace)
	tick(t, s)
	rb := nightEvents(t, s, it.ID, "rebuild")
	if len(rb) != 1 || !strings.Contains(str(rb[0].Data, "notes"), "still: fix the nil check") {
		t.Fatalf("rebuild with the review notes: %+v", rb)
	}
	// Bounced again the same night: it waits for the next night.
	ctx := context.Background()
	for _, st := range []struct {
		agent string
		out   pipeline.Outcome
		f     map[string]string
		paths []string
	}{
		{"builder", pipeline.PROpened, map[string]string{"repo": "o/r", "pr": "7", "branch": "loop/1-x", "head_sha": shaA}, []string{"M x.go"}},
		{"reviewer", pipeline.Bounce, map[string]string{"notes": "and the error message"}, nil},
	} {
		if c, _ := s.Claim(ctx, runner, pipeline.ClaimRequest{Runner: runner, Agent: st.agent}); c == nil {
			t.Fatalf("claim %s", st.agent)
		}
		if _, err := s.AddEvidence(ctx, runner, it.ID, "log", "run.log", strings.NewReader("ok\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Advance(ctx, runner, it.ID, st.out, st.f, st.paths); err != nil {
			t.Fatal(err)
		}
	}
	escalateNight(t, s, clk, it.ID, pipeline.ClassBounceLimit, "one more build with the notes", "")
	clk.Advance(pipeline.NightGrace)
	tick(t, s)
	if st := stage(t, s, it.ID); st != pipeline.NeedsYou {
		t.Fatalf("second bounce_limit tonight acted: %s", st)
	}
	clk.Advance(24 * time.Hour) // the next night
	tick(t, s)
	if st := stage(t, s, it.ID); st != pipeline.Build {
		t.Fatalf("next night: %s", st)
	}
}

// scope_choice: answer with the recommendation and file the deferred part as a P3 item.
func TestNightScopeChoice(t *testing.T) {
	s, clk := open(t)
	nightMode(t, s, pipeline.NightAlways)
	it := add(t, s, "x", "")
	stuck(t, s, it.ID, pipeline.Question)
	escalateNight(t, s, clk, it.ID, pipeline.ClassScopeChoice, "doc-only for now", "the code change to the parser")
	clk.Advance(pipeline.NightGrace)
	tick(t, s)
	act := nightEvents(t, s, it.ID, KindOrchNightAction)
	if len(act) != 1 {
		t.Fatalf("night actions %+v", act)
	}
	filed := int64(act[0].Data["filed"].(float64))
	d, err := s.GetItem(context.Background(), filed)
	if err != nil || d.Item.Priority != pipeline.DeferredPriority || !strings.Contains(d.Item.Title, "the code change to the parser") ||
		!strings.Contains(d.Item.Ask, "#1") {
		t.Fatalf("filed %+v %v", d.Item, err)
	}
	ans := nightEvents(t, s, it.ID, "answered")
	if len(ans) != 1 || ans[0].Actor != NightActor || !strings.HasPrefix(str(ans[0].Data, "answer"), "doc-only for now") {
		t.Fatalf("answer %+v", ans)
	}
	// The night shift's item does not count against the orchestrator's 5 a day.
	var n int
	if err := s.r.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ?`, KindOrchAdded).Scan(&n); err != nil || n != 0 {
		t.Fatalf("orchestrator.added %d %v", n, err)
	}
}

// The user wins inside the grace period: an answer, or speaking to the orchestrator in the thread.
func TestNightUserWins(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	nightMode(t, s, pipeline.NightAlways)
	a := add(t, s, "a", "")
	stuck(t, s, a.ID, pipeline.Question)
	b := add(t, s, "b", "")
	stuck(t, s, b.ID, pipeline.Question)
	escalateNight(t, s, clk, a.ID, pipeline.ClassScopeChoice, "doc-only", "the rest")
	escalateNight(t, s, clk, b.ID, pipeline.ClassScopeChoice, "doc-only", "the rest")
	if _, _, err := s.OrchMessage(ctx, OrchestratorMessage{Channel: "C1", TS: "9.1", User: "U0APPROVER", Text: "wait, let me think", ItemID: b.ID, Approver: true}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(pipeline.NightGrace - pipeline.OrchEscalateEvery - time.Minute) // a's grace has a minute left
	tick(t, s)
	if _, err := s.Answer(ctx, "slack:U0APPROVER", a.ID, "do it all"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	tick(t, s)
	for _, id := range []int64{a.ID, b.ID} {
		if n := len(nightEvents(t, s, id, KindOrchNightAction)) + len(nightEvents(t, s, id, KindOrchNightStayed)); n != 0 {
			t.Fatalf("#%d: the night shift acted after the user", id)
		}
	}
	if st := stage(t, s, b.ID); st != pipeline.NeedsYou {
		t.Fatalf("b %s", st)
	}
}

// The never-list stays escalated: an item on the hold list, an escalation without a class, the
// night shift off, a class turned off, an escalation from the day.
func TestNightNeverList(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	nightMode(t, s, pipeline.NightAlways)
	held := toNeedsYou(t, s, pipeline.VerifyFailed)
	if _, err := s.w.Exec(`UPDATE items SET risk = 'hold', hold_reason = 'auth: internal/api/auth.go' WHERE id = ?`, held.ID); err != nil {
		t.Fatal(err)
	}
	escalateNight(t, s, clk, held.ID, pipeline.ClassVerifyFlake, "re-run verify", "")
	merge := add(t, s, "m", "")
	stuck(t, s, merge.ID, pipeline.Question)
	escalateNight(t, s, clk, merge.ID, pipeline.ClassScopeChoice, "merge the doc part now", "the rest")
	plain := add(t, s, "p", "")
	stuck(t, s, plain.ID, pipeline.Question)
	clk.Advance(pipeline.OrchEscalateEvery)
	if _, err := s.OrchEscalate(ctx, orch, plain.ID, "a product call", "local", "works offline"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	tick(t, s)
	for _, c := range []struct {
		id  int64
		why string
	}{{held.ID, "hold list"}, {merge.ID, "merges"}} {
		st := nightEvents(t, s, c.id, KindOrchNightStayed)
		if len(st) != 1 || !strings.Contains(str(st[0].Data, "why"), c.why) || stage(t, s, c.id) != pipeline.NeedsYou {
			t.Fatalf("#%d: stayed %+v", c.id, st)
		}
	}
	if n := len(nightEvents(t, s, plain.ID, KindOrchNightStayed)) + len(nightEvents(t, s, plain.ID, KindOrchNightAction)); n != 0 || stage(t, s, plain.ID) != pipeline.NeedsYou {
		t.Fatalf("an escalation without a class was touched")
	}

	// Night shift off, then the class off: nothing; on again, an escalation from the day stays.
	nightMode(t, s, pipeline.NightOff)
	off := add(t, s, "o", "")
	stuck(t, s, off.ID, pipeline.Question)
	escalateNight(t, s, clk, off.ID, pipeline.ClassScopeChoice, "doc-only", "the rest")
	clk.Advance(time.Hour)
	tick(t, s)
	nightMode(t, s, pipeline.NightAlways, pipeline.ClassScopeChoice)
	clk.Advance(time.Minute)
	tick(t, s)
	if n := len(nightEvents(t, s, off.ID, KindOrchNightStayed)) + len(nightEvents(t, s, off.ID, KindOrchNightAction)); n != 0 {
		t.Fatalf("acted while off")
	}
	nightMode(t, s, pipeline.NightAlways, []string{}...)
	clk.Advance(time.Minute)
	tick(t, s)
	st := nightEvents(t, s, off.ID, KindOrchNightStayed)
	if len(st) != 1 || !strings.Contains(str(st[0].Data, "why"), "night shift was off") || stage(t, s, off.ID) != pipeline.NeedsYou {
		t.Fatalf("day escalation: %+v", st)
	}
}

// An escalation's class is checked when it is made.
func TestNightEscalationClassChecked(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := add(t, s, "x", "")
	if _, err := s.OrchEscalateNight(ctx, orch, it.ID, "t", "r", "why", pipeline.ClassTicketLimit, ""); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("ticket_limit: %v", err)
	}
	if _, err := s.OrchEscalateNight(ctx, orch, 0, "t", "r", "why", pipeline.ClassVerifyFlake, ""); code(err) != pipeline.CodeMissingField {
		t.Fatalf("no item: %v", err)
	}
	if _, err := s.OrchEscalateNight(ctx, orch, it.ID, "t", "r", "why", pipeline.ClassScopeChoice, ""); code(err) != pipeline.CodeMissingField {
		t.Fatalf("scope without deferred: %v", err)
	}
}

// ticket_limit: the 6th process ticket at 02:00 passes and the 11th is refused; the 6th other
// ticket at night is refused; the 6th at 10:00 is refused.
func TestNightTicketLimit(t *testing.T) {
	ny, _ := time.LoadLocation(OrchDayZone)
	for _, c := range []struct {
		name     string
		at       time.Time
		repo     string
		allowed  int
		nightAct bool
	}{
		{"process at night", time.Date(2026, 10, 20, 2, 0, 0, 0, ny), "alexberardi/jarvis-loop", pipeline.OrchAddsPerNight, true},
		{"other at night", time.Date(2026, 10, 20, 2, 0, 0, 0, ny), "jarvis-server", pipeline.OrchAddsPerDay, false},
		{"process at 10:00", time.Date(2026, 10, 20, 10, 0, 0, 0, ny), "alexberardi/jarvis-loop", pipeline.OrchAddsPerDay, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			clk := clock.NewFake(c.at)
			s, err := Open(t.TempDir(), clk)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.SetRepos([]string{"alexberardi/jarvis-loop", "alexberardi/jarvis-server"})
			nightMode(t, s, pipeline.NightAuto)
			ctx := context.Background()
			for i := 1; i <= c.allowed+1; i++ {
				it, _, err := s.OrchAdd(ctx, orch, NewItem{Title: "t", Repo: c.repo, Priority: "P1"}, "sig "+strings.Repeat("x", i), "process")
				if i > c.allowed {
					if code(err) != pipeline.CodeLimit {
						t.Fatalf("ticket %d: %v", i, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("ticket %d: %v", i, err)
				}
				acts := nightEvents(t, s, it.ID, KindOrchNightAction)
				if want := c.nightAct && i > pipeline.OrchAddsPerDay; (len(acts) == 1) != want {
					t.Fatalf("ticket %d: night actions %+v", i, acts)
				}
			}
		})
	}
}

// "night" and "morning" override auto mode until the next edge.
func TestNightOverride(t *testing.T) {
	s, clk := open(t) // 08:00 New York
	ctx := context.Background()
	nightMode(t, s, pipeline.NightAuto)
	if st, _ := s.NightShift(ctx); st.On {
		t.Fatal("on at 08:00")
	}
	st, err := s.SetNightOverride(ctx, "slack:U0APPROVER", pipeline.OverrideNight, "", "")
	if err != nil || !st.On {
		t.Fatalf("night: %+v %v", st, err)
	}
	clk.Advance(14*time.Hour + 59*time.Minute) // 22:59
	if st, _ := s.NightShift(ctx); !st.On {
		t.Fatal("night ended early")
	}
	clk.Advance(9 * time.Hour) // 07:59 the next day: the auto night
	if _, err := s.SetNightOverride(ctx, "slack:U0APPROVER", pipeline.OverrideMorning, "", ""); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.NightShift(ctx); st.On {
		t.Fatal("morning did not turn it off")
	}
	if _, err := s.SetNightOverride(ctx, "slack:U0APPROVER", "noon", "", ""); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("noon: %v", err)
	}
}

// An escalation is about the question it was raised on: when the item leaves needs_you without the
// user (the orchestrator answers it) and comes back with a new verify failure on a new head, the old
// verify_flake escalation is not acted on for the new question.
func TestNightNewQuestion(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	nightMode(t, s, pipeline.NightAlways)
	it := toNeedsYou(t, s, pipeline.VerifyFailed)
	esc := escalateNight(t, s, clk, it.ID, pipeline.ClassVerifyFlake, "re-run verify", "")
	if esc.Data["act_at"] == nil {
		t.Fatalf("escalation %+v", esc.Data)
	}
	if _, err := s.OrchAnswer(ctx, orch, it.ID, pipeline.AnswerHarness, "re-run the harness", "harness error"); err != nil {
		t.Fatal(err)
	}
	c, err := s.Claim(ctx, runner, pipeline.ClaimRequest{Runner: runner, Agent: "builder"})
	if err != nil || c == nil || c.Item.ID != it.ID {
		t.Fatalf("build claim %+v %v", c, err)
	}
	if _, err := s.AddEvidence(ctx, runner, it.ID, "log", "v.log", strings.NewReader("FAIL\n")); err != nil {
		t.Fatal(err)
	}
	const shaB = "89abcdef0123456789abcdef0123456789abcdef"
	if _, err := s.Advance(ctx, runner, it.ID, pipeline.VerifyFailed, map[string]string{"reason": "scenario 40 failed", "head_sha": shaB}, nil); err != nil {
		t.Fatal(err)
	}
	if st := stage(t, s, it.ID); st != pipeline.NeedsYou {
		t.Fatalf("not back with the user: %s", st)
	}
	clk.Advance(pipeline.NightGrace)
	tick(t, s)
	tick(t, s)
	if st := stage(t, s, it.ID); st != pipeline.NeedsYou {
		t.Fatalf("the old escalation acted on the new question: %s", st)
	}
	if n := len(nightEvents(t, s, it.ID, KindOrchNightAction)); n != 0 {
		t.Fatalf("%d night actions", n)
	}
	for _, rb := range nightEvents(t, s, it.ID, "rebuild") {
		t.Fatalf("rebuild %+v", rb)
	}
}

// The night ticket cap is 10, the item's number.
func TestNightTicketCapIsTen(t *testing.T) {
	if pipeline.OrchAddsPerNight != 10 || pipeline.OrchAddsPerDay != 5 {
		t.Fatalf("caps: %d a night, %d a day", pipeline.OrchAddsPerNight, pipeline.OrchAddsPerDay)
	}
}
