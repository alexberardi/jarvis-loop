package store

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// claudeRun is a triage run's fields as the runner sends them: input 1000 burns 1000.
func claudeRun(input int64) map[string]string {
	return map[string]string{"area": "a", "size": "S", "risk": "normal", "resources": "", "run_seconds": "600",
		"model": "claude-opus-5-5", "total_cost_usd": "0.5000", "usage_input": strconv.FormatInt(input, 10), "usage_output": "0",
		"usage_cache_creation": "0", "usage_cache_read": "0"}
}

func claimReq(t *testing.T, s *Store, req pipeline.ClaimRequest) *Claim {
	t.Helper()
	req.Runner = runner
	if req.Agent == "" {
		req.Agent = "agent-1"
	}
	c, err := s.Claim(context.Background(), runner, req)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// triageWith claims the next item (it must be it) and finishes its triage with fields.
func triageWith(t *testing.T, s *Store, it pipeline.Item, f map[string]string) {
	t.Helper()
	if c := claimReq(t, s, pipeline.ClaimRequest{Stages: []pipeline.Stage{pipeline.Triage}}); c == nil || c.Item.ID != it.ID {
		t.Fatalf("triage claim: %+v", c)
	}
	evidence(t, s, it.ID)
	if _, err := s.Advance(context.Background(), runner, it.ID, pipeline.Reproduced, f, nil); err != nil {
		t.Fatal(err)
	}
}

func lastPace(t *testing.T, s *Store) Event {
	t.Helper()
	e, ok, err := s.LastEvent(context.Background(), KindUsagePace)
	if err != nil || !ok {
		t.Fatalf("no usage_pace event (%v)", err)
	}
	return e
}

func TestUsageLedgerAndReport(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	a, b := add(t, s, "the big one", "P2"), add(t, s, "the small one", "P2")
	triageWith(t, s, a, claudeRun(5000))
	triageWith(t, s, b, claudeRun(1000))
	clk.Advance(time.Minute)
	// A qwen build of b: its tokens are qwen's, not claude's.
	if c := claim(t, s, "agent-2"); c == nil {
		t.Fatal("no build claim")
	}
	evidence(t, s, a.ID)
	if _, err := s.Advance(ctx, runner, a.ID, pipeline.PROpened, map[string]string{"repo": "o/r", "pr": "7", "branch": "loop/1-x", "head_sha": shaA,
		"builder": "qwen", "qwen_requests": "3", "qwen_tokens_input": "9000", "qwen_tokens_output": "500", "run_seconds": "60"}, []string{"M x.go"}); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Claude.Burn5h != 6000 || rep.Claude.BurnWeek != 6000 {
		t.Fatalf("claude burn %d / %d, want 6000", rep.Claude.Burn5h, rep.Claude.BurnWeek)
	}
	if want := t0.Add(-10 * time.Minute); !rep.Claude.WindowStart.Equal(want) {
		t.Fatalf("window start %v, want %v (the first run's start)", rep.Claude.WindowStart, want)
	}
	if len(rep.Items) != 2 || rep.Items[0].ItemID != a.ID || rep.Items[0].Title != "the big one" || rep.Items[0].Burn != 5000 {
		t.Fatalf("items %+v", rep.Items)
	}
	if len(rep.Days) != 2 || rep.Days[0].Provider != "claude" || rep.Days[0].Runs != 2 || rep.Days[0].CostUSD != 1.0 ||
		rep.Days[1].Provider != "qwen" || rep.Days[1].Tokens != 9500 {
		t.Fatalf("days %+v", rep.Days)
	}
	var stages []string
	for _, g := range rep.Stages {
		stages = append(stages, g.Key+"/"+g.Provider)
	}
	if strings.Join(stages, ",") != "build/qwen,triage/claude" {
		t.Fatalf("stages %v", stages)
	}
	if rep.Pace.Level != pipeline.PaceNormal || rep.Claude.Limit5h != 0 {
		t.Fatalf("no limit known yet: %+v %+v", rep.Pace, rep.Claude)
	}
}

func TestUsageLimitPausesClaudeUntilTheReset(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	hit, other, qwen := add(t, s, "hits the limit", "P2"), add(t, s, "waits", "P2"), add(t, s, "a qwen piece", "P3")
	triageWith(t, s, hit, claudeRun(4000))
	triageWith(t, s, other, claudeRun(1000))
	triageWith(t, s, qwen, claudeRun(0))
	if _, err := s.w.Exec(`UPDATE items SET builder = 'qwen' WHERE id = ?`, qwen.ID); err != nil {
		t.Fatal(err)
	}
	if c := claim(t, s, "agent-1"); c == nil || c.Item.ID != hit.ID {
		t.Fatalf("build claim %+v", c)
	}
	reset := t0.Add(2 * time.Hour)
	f := claudeRun(3000)
	f["error"], f["reset"], f["limit_window"] = "rate limit: rejected", reset.Format(time.RFC3339), "5h"
	if _, err := s.Advance(ctx, runner, hit.ID, pipeline.UsageLimit, f, nil); err != nil {
		t.Fatal(err)
	}
	ctl, _ := s.Control(ctx)
	if ctl.ActiveAgents != 4 {
		t.Fatalf("active agents %d, want the user's 4", ctl.ActiveAgents)
	}
	e := lastPace(t, s)
	if e.Data["level"] != float64(pipeline.PacePaused) || e.Data["until"] != reset.Format(time.RFC3339) || e.Data["item"] != float64(hit.ID) || e.Data["burn"] != float64(8000) {
		t.Fatalf("pace event %+v", e.Data)
	}
	// Paused: the qwen build goes on, claude's build waits.
	if c := claim(t, s, "agent-2"); c == nil || c.Item.ID != qwen.ID {
		t.Fatalf("paused claim %+v, want the qwen item", c)
	}
	if c := claim(t, s, "agent-3"); c != nil {
		t.Fatalf("claude claim while paused: %+v", c)
	}
	// The learned limit is the window's burn when it hit.
	rep, _ := s.Usage(ctx)
	if rep.Claude.Limit5h != 8000 || len(rep.Hits) != 1 || !rep.Hits[0].ResetAt.Equal(reset) {
		t.Fatalf("learned %+v hits %+v", rep.Claude, rep.Hits)
	}
	// After the reset claims resume, and the change is recorded once.
	clk.Advance(2*time.Hour + time.Second)
	if c := claim(t, s, "agent-3"); c == nil || c.Item.ID != hit.ID {
		t.Fatalf("claim after the reset %+v", c)
	}
	if e := lastPace(t, s); e.Data["level"] != float64(pipeline.PaceNormal) || e.Data["from_level"] != float64(pipeline.PacePaused) {
		t.Fatalf("resume event %+v", e.Data)
	}
	var n int
	_ = s.r.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ?`, KindUsagePace).Scan(&n)
	if claim(t, s, "agent-4"); n != 2 {
		t.Fatalf("%d usage_pace events, want 2 (paused, resumed)", n)
	}
}

func TestBudgetPacesClaims(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if _, err := s.SetControl(ctx, user, ControlPatch{Budgets: map[string]pipeline.Budget{"claude": {Per5h: 10000}}}); err != nil {
		t.Fatal(err)
	}
	ctl, _ := s.Control(ctx)
	if ctl.Budgets["claude"].Per5h != 10000 {
		t.Fatalf("budget not stored: %+v", ctl.Budgets)
	}
	big, small, low := add(t, s, "P2 M", "P2"), add(t, s, "P2 S", "P2"), add(t, s, "P4 M", "P4")
	mid := claudeRun(3000)
	mid["size"] = "M"
	triageWith(t, s, big, mid)
	triageWith(t, s, small, claudeRun(3000))
	low4 := claudeRun(3000) // 9000 of 10000: 90%
	low4["size"] = "M"
	triageWith(t, s, low, low4)
	// Slow at 90%: the P2 M waits, the P2 S goes; a runner with qwen gets the P4 M routed to qwen.
	if c := claim(t, s, "agent-1"); c == nil || c.Item.ID != small.ID || c.Lease.Builder != "" {
		t.Fatalf("slow claim %+v, want the size S item", c)
	}
	if e := lastPace(t, s); e.Data["level"] != float64(pipeline.PaceSlow) || e.Data["agents"] != float64(2) || e.Data["percent"] != float64(90) {
		t.Fatalf("pace event %+v", e.Data)
	}
	c := claimReq(t, s, pipeline.ClaimRequest{Agent: "agent-2", Builders: []string{pipeline.BuilderQwen}})
	if c == nil || c.Item.ID != low.ID || c.Item.Builder != pipeline.BuilderQwen || c.Lease.Builder != pipeline.BuilderQwen {
		t.Fatalf("routed claim %+v", c)
	}
	d, _ := s.GetItem(ctx, low.ID)
	if d.Item.Builder != "" {
		t.Fatalf("the route is for this run only; item builder %q", d.Item.Builder)
	}
	if c := claim(t, s, "agent-3"); c != nil {
		t.Fatalf("the P2 M was claimed while slow: %+v", c)
	}
	// Removing the budget (the default: what got us limited, never yet) ends the pacing.
	if _, err := s.SetControl(ctx, user, ControlPatch{Budgets: map[string]pipeline.Budget{"claude": pipeline.DefaultBudgets()["claude"]}}); err != nil {
		t.Fatal(err)
	}
	if c := claim(t, s, "agent-3"); c == nil || c.Item.ID != big.ID {
		t.Fatalf("claim after the budget went: %+v", c)
	}
	var raw string
	_ = s.r.QueryRow(`SELECT budgets FROM control WHERE id = 1`).Scan(&raw)
	if raw != "" {
		t.Fatalf("default budgets stored as %q, want '' (the live defaults)", raw)
	}
}

func TestUsageBackfillFromEvents(t *testing.T) {
	s, _ := open(t)
	it := add(t, s, "x", "")
	triageWith(t, s, it, claudeRun(1234))
	body, err := os.ReadFile("migrations/0020_usage.sql")
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(body), "INSERT INTO usage_runs")
	if i < 0 {
		t.Fatal("no backfill in the migration")
	}
	if _, err := s.w.Exec(`DELETE FROM usage_runs`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(string(body)[i:]); err != nil {
		t.Fatal(err)
	}
	runs, err := usageRuns(s.r, "", t0.Add(-time.Hour))
	if err != nil || len(runs) != 1 {
		t.Fatalf("backfilled %+v %v", runs, err)
	}
	r := runs[0]
	if r.Provider != "claude" || r.Input != 1234 || r.CostUSD != 0.5 || r.Stage != "triage" || !r.Started.Equal(r.At.Add(-10*time.Minute)) {
		t.Fatalf("backfilled run %+v", r)
	}
}

// A run in a repo whose main went red burned its tokens when it finished: the ledger has it then,
// though its advance waits for the thaw, and once only after the thaw.
func TestUsageLedgerOfAQueuedAdvance(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	culprit := merged(t, s)
	it := addIn(t, s, "in the repo", culprit.Repo, "P2")
	if c := claimed(t, s, "t1"); c == nil || c.Item.ID != it.ID {
		t.Fatalf("claim %+v", c)
	}
	if _, err := s.MainCI(ctx, runner, culprit.ID, redData()); err != nil {
		t.Fatal(err)
	}
	evidence(t, s, it.ID)
	if got, err := s.Advance(ctx, runner, it.ID, pipeline.Reproduced, claudeRun(1000), nil); err != nil || got.Stage != pipeline.Triage {
		t.Fatalf("queued advance: %+v %v", got, err)
	}
	runs := func() []pipeline.UsageRun {
		t.Helper()
		r, err := usageRuns(s.r, "", t0.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := runs(); len(r) != 1 || r[0].ItemID != it.ID || r[0].Burn() != 1000 {
		t.Fatalf("ledger while frozen: %+v", r)
	}
	if _, err := s.Unfreeze(ctx, user, culprit.Repo); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetItem(ctx, it.ID); d.Item.Stage != pipeline.Build {
		t.Fatalf("after the thaw: %s", d.Item.Stage)
	}
	if r := runs(); len(r) != 1 {
		t.Fatalf("ledger after the thaw: %d runs", len(r))
	}
}
