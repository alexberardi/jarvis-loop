package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// triagedAs triages it with a size and change kind ("": none).
func triagedAs(t *testing.T, s *Store, it pipeline.Item, size, kind string) {
	t.Helper()
	f := map[string]string{"area": "a", "size": size, "risk": "normal", "resources": ""}
	if kind != "" {
		f["change_kind"] = kind
	}
	triageWith(t, s, it, f)
}

// With claude's pace normal, a P3 size S build goes to qwen for a runner that has it; a second
// one, while that runs, goes to claude on another runner: one qwen build at a time across runners
// (docs/decisions/2026-10-19-qwen-routing.md).
func TestQwenRoutedUnpacedAndLimited(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	a, b, q := add(t, s, "first", "P3"), add(t, s, "second", "P3"), add(t, s, "a qwen item", "P4")
	triagedAs(t, s, a, "S", "")
	triagedAs(t, s, b, "M", "")
	triagedAs(t, s, q, "S", "")
	if _, err := s.SetBuilder(ctx, user, q.ID, "qwen"); err != nil {
		t.Fatal(err)
	}
	qwen := []string{pipeline.BuilderQwen}
	c := claimReq(t, s, pipeline.ClaimRequest{Agent: "agent-1", Builders: qwen})
	if c == nil || c.Item.ID != a.ID || c.Lease.Builder != pipeline.BuilderQwen {
		t.Fatalf("first claim %+v, want #%d routed to qwen", c, a.ID)
	}
	d, _ := s.GetItem(ctx, a.ID)
	claimed := d.Events[len(d.Events)-1]
	if d.Item.Builder != "" || claimed.Kind != "claimed" || claimed.Data["routed"] != "qwen" || claimed.Data["pace"] != nil {
		t.Fatalf("item builder %q, claimed %+v", d.Item.Builder, claimed.Data)
	}
	// Another runner with qwen: the P3 M builds with claude, and the qwen item waits for qwen.
	c2, err := s.Claim(ctx, "mac", pipeline.ClaimRequest{Runner: "mac", Agent: "agent-2", Builders: qwen})
	if err != nil || c2 == nil || c2.Item.ID != b.ID || c2.Lease.Builder != "" {
		t.Fatalf("second claim %+v %v, want #%d with claude", c2, err, b.ID)
	}
	c3, err := s.Claim(ctx, "mac", pipeline.ClaimRequest{Runner: "mac", Agent: "agent-3", Builders: qwen})
	if err != nil || c3 != nil {
		t.Fatalf("the qwen item was claimed while qwen was busy: %+v %v", c3, err)
	}
	// Its builder is fixed only before a build starts.
	if _, err := s.SetBuilder(ctx, user, a.ID, "claude"); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("SetBuilder while building: %v", err)
	}
}

// A qwen build past the time cap counts as a failed qwen build.
func TestQwenOvertimeCounts(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	it := add(t, s, "x", "P3")
	triagedAs(t, s, it, "S", "")
	c := claimReq(t, s, pipeline.ClaimRequest{Builders: []string{pipeline.BuilderQwen}})
	if c == nil || c.Lease.Builder != pipeline.BuilderQwen {
		t.Fatalf("claim %+v", c)
	}
	clk.Advance(46 * time.Minute)
	if _, err := s.Heartbeat(ctx, runner, c.Lease.ID, ""); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetItem(ctx, it.ID)
	if d.Item.Stage != pipeline.NeedsYou || d.Item.QwenFailures != 1 || d.Item.Builder != "" {
		t.Fatalf("overtime: %+v", d.Item)
	}
}

// A control row from before the qwen limit gets the default 1; a fresh one has it.
func TestQwenLimitMigration(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if ctl, _ := s.Control(ctx); ctl.Limits[pipeline.LimitQwen] != 1 {
		t.Fatalf("fresh limits %v", ctl.Limits)
	}
	body, err := os.ReadFile("migrations/0034_qwen_routing.sql")
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(body), "UPDATE control")
	if i < 0 {
		t.Fatal("no control update in the migration")
	}
	if _, err := s.w.Exec(`UPDATE control SET limits = json_remove(limits, '$.qwen')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(string(body[i:])); err != nil {
		t.Fatal(err)
	}
	if ctl, _ := s.Control(ctx); ctl.Limits[pipeline.LimitQwen] != 1 || ctl.Limits["gpu"] != 2 {
		t.Fatalf("migrated limits %v", ctl.Limits)
	}
}

// The usage report's qwen week: qwen's builds, the Claude spend they replaced (same-size claude
// builds' mean cost), and its first-review approvals.
func TestUsageQwenWeek(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	q, c := add(t, s, "built by qwen", "P3"), add(t, s, "built by claude", "P2")
	triagedAs(t, s, c, "S", "")
	triagedAs(t, s, q, "S", "")
	build := func(it pipeline.Item, builders []string, f map[string]string) {
		t.Helper()
		cl := claimReq(t, s, pipeline.ClaimRequest{Agent: "builder", Builders: builders, Stages: []pipeline.Stage{pipeline.Build}})
		if cl == nil || cl.Item.ID != it.ID {
			t.Fatalf("build claim %+v, want #%d", cl, it.ID)
		}
		evidence(t, s, it.ID)
		f["repo"], f["pr"], f["branch"], f["head_sha"] = "o/r", "7", "loop/x", shaA
		if _, err := s.Advance(ctx, runner, it.ID, pipeline.PROpened, f, []string{"M x.go"}); err != nil {
			t.Fatal(err)
		}
	}
	review := func(it pipeline.Item, out pipeline.Outcome, f map[string]string) {
		t.Helper()
		if cl := claimReq(t, s, pipeline.ClaimRequest{Agent: "reviewer", Stages: []pipeline.Stage{pipeline.Review}}); cl == nil || cl.Item.ID != it.ID {
			t.Fatalf("review claim %+v", cl)
		}
		if _, err := s.Advance(ctx, runner, it.ID, out, f, nil); err != nil {
			t.Fatal(err)
		}
	}
	build(c, nil, map[string]string{"builder": "claude", "usage_input": "1000", "total_cost_usd": "2.5000", "run_seconds": "600"})
	build(q, []string{pipeline.BuilderQwen}, map[string]string{"builder": "qwen", "qwen_tokens_input": "12000", "qwen_tokens_output": "800", "run_seconds": "1400"})
	review(c, pipeline.Bounce, map[string]string{"notes": "x"})
	review(q, pipeline.Approved, map[string]string{"summary": "ok", "head_sha": shaA})
	rep, err := s.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := pipeline.QwenWeek{Builds: 1, ReplacedUSD: 2.5, Reviewed: 1, FirstApproved: 1}
	if rep.Qwen != want {
		t.Fatalf("qwen week %+v, want %+v", rep.Qwen, want)
	}
}
