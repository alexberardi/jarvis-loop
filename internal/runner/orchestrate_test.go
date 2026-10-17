package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
	"github.com/alexberardi/jarvis-loop/skills"
)

func TestValidateOrchResult(t *testing.T) {
	cases := []struct {
		name, raw, err string
		n              int
	}{
		{"empty is a turn", `{"actions": []}`, "", 0},
		{"every action", `{"actions": [
			{"action": "retry", "item_id": 3, "reason": "git lock"},
			{"action": "answer", "item_id": 4, "kind": "harness", "text": "skip it", "reason": "standing"},
			{"action": "priority", "item_id": 5, "priority": "P1", "reason": "r"},
			{"action": "link", "item_id": 5, "after": 3, "reason": "r"},
			{"action": "add", "title": "t", "ask": "a", "repo": "alex/demo", "priority": "P1", "depends_on": [3], "signature": "s", "reason": "r"},
			{"action": "escalate", "item_id": 6, "text": "t", "recommendation": "do x", "reason": "r"},
			{"action": "agents", "active_agents": 0, "reason": "every run fails"},
			{"action": "note", "text": "watching #3", "reason": "r"}]}`, "", 8},
		{"no actions list", `{}`, "no actions list", 0},
		{"a field it does not have", `{"actions": [], "summary": "x"}`, "unknown field", 0},
		{"an unknown action", `{"actions": [{"action": "approve", "item_id": 3, "reason": "done"}]}`, `unknown action "approve"`, 0},
		{"another action's field", `{"actions": [{"action": "retry", "item_id": 3, "text": "x", "reason": "r"}]}`, `unknown field "text"`, 0},
		{"no reason", `{"actions": [{"action": "retry", "item_id": 3}]}`, "reason is required", 0},
		{"a blank reason", `{"actions": [{"action": "note", "text": "x", "reason": "  "}]}`, "reason is required", 0},
		{"a required field", `{"actions": [{"action": "answer", "item_id": 3, "text": "x", "reason": "r"}]}`, "kind is required", 0},
		{"a wrong type", `{"actions": [{"action": "retry", "item_id": "3", "reason": "r"}]}`, "action 1 (retry)", 0},
		{"not JSON", `{"actions": [`, "out/result.json", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acts, err := ValidateOrchResult([]byte(tc.raw))
			if tc.err == "" {
				if err != nil || len(acts) != tc.n {
					t.Fatalf("got %d actions, %v", len(acts), err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("got %v, want %q", err, tc.err)
			}
		})
	}
	many := `{"actions": [` + strings.TrimSuffix(strings.Repeat(`{"action": "note", "text": "x", "reason": "r"},`, MaxOrchActions+1), ",") + `]}`
	if _, err := ValidateOrchResult([]byte(many)); err == nil {
		t.Fatal("too many actions")
	}
}

func TestClaimKinds(t *testing.T) {
	cases := []struct {
		stages      []string
		items, kind []string
	}{
		{[]string{"build"}, []string{"build"}, nil},
		{[]string{"build", "plan"}, []string{"build"}, []string{"item", "plan"}},
		{[]string{"orchestrate"}, nil, []string{"orchestrate"}},
		{[]string{"triage", "orchestrate", "plan"}, []string{"triage"}, []string{"item", "orchestrate", "plan"}},
	}
	for _, tc := range cases {
		var got struct {
			Stages []string `json:"stages"`
			Kinds  []string `json:"kinds"`
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewDecoder(r.Body).Decode(&got)
			w.WriteHeader(http.StatusNoContent)
		}))
		c := &Client{Base: srv.URL, Token: runnerToken}
		_, err := c.Claim(context.Background(), "a", nil, tc.stages)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got.Stages, tc.items) || !slices.Equal(got.Kinds, tc.kind) {
			t.Fatalf("%v: stages %v kinds %v", tc.stages, got.Stages, got.Kinds)
		}
	}
}

func orchRig(t *testing.T, scenario string) *rig {
	g := newRig(t, scenario)
	g.r.Cfg.Stages = []string{StageOrchestrate, StageBuild}
	g.r.Cfg.OrchestratorToken = orchToken
	g.r.Cfg.PlanTurnMinutes = 15
	return g
}

func (g *rig) orchTurns() []store.OrchTurn {
	g.t.Helper()
	ts, err := g.st.OrchTurns(g.ctx, 100)
	if err != nil {
		g.t.Fatal(err)
	}
	return ts
}

func (g *rig) orchRuns() []string {
	runs, _ := filepath.Glob(filepath.Join(g.r.Cfg.WorkRoot, "runs", "O*-orchestrate-*"))
	slices.Sort(runs)
	return runs
}

// A failed run triggers a turn; the agent's retry is refused (the backoff) and the refusal is in
// the next turn's snapshot and prompt; the second turn resumes the session; loopd rotates it the
// next day.
func TestOrchestrateTurn(t *testing.T) {
	g := orchRig(t, fakeclaude.Exit3)
	it := g.item(g.toBuild("x")).Item
	// loopd's first sweep queued a turn already (an open item, no turn yet): build first.
	g.r.Cfg.Stages = []string{StageBuild}
	if !g.runOnce() {
		t.Fatal("no build")
	}
	if d, _ := g.st.GetItem(g.ctx, it.ID); d.Item.Stage != pipeline.NeedsYou {
		t.Fatalf("the build should fail: %s\n%s", d.Item.Stage, g.logs.String())
	}
	os.WriteFile(filepath.Join(g.scripts, "orchestrator-orchestrate.json"), []byte(`{"play": "react"}`), 0o644)
	g.r.Cfg.Stages = []string{StageOrchestrate, StageBuild}
	if !g.runOnce() {
		t.Fatal("no orchestrate turn")
	}
	ts := g.orchTurns()
	if len(ts) != 1 || ts[0].Status != store.OrchDone || len(ts[0].Actions) != 2 || ts[0].Session != "fake-session-orchestrator-1" {
		t.Fatalf("turn %+v\n%s", ts, g.logs.String())
	}
	if a := ts[0].Actions[0]; a.Action != "retry" || a.ItemID != it.ID || a.OK || a.Status != 429 || a.Code != pipeline.CodeTooSoon {
		t.Fatalf("the retry inside the backoff: %+v", a)
	}
	if a := ts[0].Actions[1]; a.Action != "note" || !a.OK {
		t.Fatalf("the note: %+v", a)
	}
	notes, _ := g.st.Notebook(g.ctx, 0)
	if len(notes) != 1 || notes[0].Actor != "orchestrator:loop" {
		t.Fatalf("the note went through loopd as the orchestrator: %+v", notes)
	}
	run := g.orchRuns()[0]
	prompt, _ := os.ReadFile(filepath.Join(run, "brief.md"))
	if !strings.HasPrefix(string(prompt), OrchStatic) || !strings.Contains(string(prompt), "  - run_failed #1\n") {
		t.Fatalf("the first prompt:\n%s", prompt)
	}
	snap, _ := os.ReadFile(filepath.Join(g.r.Cfg.WorkRoot, "orchestrator", ".loop", "context", "snapshot.json"))
	if !strings.Contains(string(snap), `"triggered"`) || strings.Contains(string(snap), orchToken) {
		t.Fatalf("snapshot: %.300s", snap)
	}
	if argv := argvOf(t, run); slices.Contains(argv, "--resume") || argv[slices.Index(argv, "--settings")+1] != filepath.Join(g.r.Cfg.WorkRoot, "settings", "orchestrate.json") {
		t.Fatalf("argv %q", argv)
	}
	// The settings are the plan turns': read-only.
	if b, _ := os.ReadFile(filepath.Join(g.r.Cfg.WorkRoot, "settings", "orchestrate.json")); !strings.Contains(string(b), "loop-guard") && !strings.Contains(string(b), g.r.Cfg.Guard) {
		t.Fatalf("settings: %s", b)
	}

	// Past the backoff: the due trigger starts a turn, which resumes the session and sees its refusal.
	g.loopdClk.Advance(2 * time.Minute)
	if err := g.st.OrchTick(g.ctx); err != nil {
		t.Fatal(err)
	}
	if !g.runOnce() {
		t.Fatal("no second turn")
	}
	ts = g.orchTurns()
	if ts[0].Status != store.OrchDone || !ts[0].Actions[0].OK || ts[0].Actions[0].Action != "retry" {
		t.Fatalf("second turn %+v\n%s", ts[0], g.logs.String())
	}
	run = g.orchRuns()[1]
	prompt, _ = os.ReadFile(filepath.Join(run, "brief.md"))
	if !strings.HasPrefix(string(prompt), "## This turn\n") || !strings.Contains(string(prompt), "- retry #1: refused (429 too_soon)") || !strings.Contains(string(prompt), "  - due\n") {
		t.Fatalf("the second prompt:\n%s", prompt)
	}
	if argv := argvOf(t, run); argv[slices.Index(argv, "--resume")+1] != "fake-session-orchestrator-1" {
		t.Fatalf("argv %q", argv)
	}
	if d, _ := g.st.GetItem(g.ctx, it.ID); d.Item.Stage != pipeline.Build {
		t.Fatalf("retried: %s", d.Item.Stage)
	}
}

// A result with an action it does not have is refused once, and the turn asked again; a second
// refusal fails the turn (its session kept).
func TestOrchestrateRefusedResult(t *testing.T) {
	for _, play := range []string{fakeclaude.OrchInvalidOnce, fakeclaude.OrchInvalid} {
		t.Run(play, func(t *testing.T) {
			g := orchRig(t, play)
			os.WriteFile(filepath.Join(g.scripts, "orchestrator-orchestrate.json"), []byte(`{"play": "`+play+`"}`), 0o644)
			g.st.QueueOrchestrate(g.ctx, pipeline.TriggerMessage, "what's stuck?")
			if !g.runOnce() {
				t.Fatal("no turn")
			}
			ts := g.orchTurns()
			run := g.orchRuns()[0]
			if _, err := os.Stat(filepath.Join(run, "brief-1.md")); err != nil {
				t.Fatalf("the refused attempt's brief is kept: %v", err)
			}
			retry, _ := os.ReadFile(filepath.Join(run, "brief.md"))
			if !strings.Contains(string(retry), RetryHeading+"\n\naction 1: unknown action \"approve\"") {
				t.Fatalf("the retry prompt:\n%s", retry)
			}
			if play == fakeclaude.OrchInvalidOnce {
				if ts[0].Status != store.OrchDone || ts[0].Error != "" {
					t.Fatalf("turn %+v\n%s", ts[0], g.logs.String())
				}
				return
			}
			if ts[0].Status != store.OrchFailed || !strings.Contains(ts[0].Error, `unknown action "approve"`) || ts[0].Session == "" {
				t.Fatalf("turn %+v", ts[0])
			}
		})
	}
}

func TestOrchestrateConfig(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	c := *g.r.Cfg
	c.Stages = []string{StageOrchestrate}
	if err := c.normalize(); err == nil || !strings.Contains(err.Error(), "orchestrator_token") {
		t.Fatalf("stage orchestrate without a token: %v", err)
	}
	c.OrchestratorToken = c.Token
	if err := c.normalize(); err == nil {
		t.Fatal("the runner's own token as the orchestrator's")
	}
	c.OrchestratorToken = orchToken
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
}

// The orchestrator's first prompt of the day is its skill, then the result contract (a golden
// file, like the stages' static briefs): the same bytes every day, one cacheable prefix.
func TestOrchStaticIsTheSkillThenTheContract(t *testing.T) {
	path := filepath.Join("testdata", "brief_orchestrate.golden.md")
	if *update {
		if err := os.WriteFile(path, []byte(OrchStatic), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err, "(run go test ./internal/runner -run OrchStatic -update)")
	}
	if OrchStatic != string(want) {
		t.Errorf("%s differs from OrchStatic; review and rerun with -update", path)
	}
	skill := strings.TrimSpace(skills.Orchestrator[strings.Index(skills.Orchestrator, "\n---\n")+5:])
	if !strings.HasPrefix(OrchStatic, skill) || strings.HasPrefix(OrchStatic, "---") {
		t.Error("OrchStatic must start with the orchestrator's skill, without its frontmatter")
	}
	for _, s := range []string{"## The result contract", "## Answers you may give", "## Process issues", "**Never**", "`alexberardi/jarvis-loop`"} {
		if !strings.Contains(OrchStatic, s) {
			t.Errorf("OrchStatic has no %q", s)
		}
	}
	for _, k := range pipeline.AnswerKinds {
		if !strings.Contains(skills.Orchestrator, "**`"+k+"`**") {
			t.Errorf("the skill does not explain the answer kind %q", k)
		}
	}
	if strings.Contains(OrchStatic, TurnHeading) {
		t.Error("the skill contains the turn heading")
	}
}
