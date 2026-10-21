package runner

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The rounds of a pipeline-regressions step: a scenario that fails every run on the change and
// passes every run on the base is a suspect; it is a regression only if the confirm rounds split
// the same way on both sides. A scenario failing on the base too is known; one that passes once in
// a confirm round on the change, or fails once on the base, is flaky.
func TestJudgePipeline(t *testing.T) {
	s := func(passed, runs int) pipelineScore { return pipelineScore{Passed: passed, Runs: runs} }
	type rounds = []map[string]pipelineScore
	for name, tc := range map[string]struct {
		rounds rounds
		want   pipelineVerdict
		sides  string
	}{
		"nothing failed every run": {
			rounds: rounds{{"who am I": s(1, 3), "timer": s(3, 3), "voice": {Skipped: true}}},
			sides:  "head:all:3",
		},
		"failing on the base too": {
			rounds: rounds{{"computer control off": s(0, 3), "timer": s(3, 3)}, {"computer control off": s(1, 3)}},
			want:   pipelineVerdict{Known: []string{"computer control off"}},
			sides:  "head:all:3 base:computer control off:3",
		},
		"new on the change (not on the base)": {
			rounds: rounds{{"brand new": s(0, 3)}, {}},
			want:   pipelineVerdict{Known: []string{"brand new"}},
			sides:  "head:all:3 base:brand new:3",
		},
		"flaky on the change": {
			rounds: rounds{{"who am I": s(0, 3), "known": s(0, 3)}, {"who am I": s(3, 3), "known": s(0, 3)}, {"who am I": s(2, 5)}},
			want:   pipelineVerdict{Known: []string{"known"}, Flaky: []string{"who am I"}},
			sides:  "head:all:3 base:known,who am I:3 head:who am I:5",
		},
		"flaky on the base": {
			rounds: rounds{{"who am I": s(0, 3)}, {"who am I": s(3, 3)}, {"who am I": s(0, 5)}, {"who am I": s(4, 5)}},
			want:   pipelineVerdict{Flaky: []string{"who am I"}},
			sides:  "head:all:3 base:who am I:3 head:who am I:5 base:who am I:5",
		},
		"a regression": {
			rounds: rounds{{"who am I": s(0, 3), "remember": s(0, 3)}, {"who am I": s(3, 3), "remember": s(3, 3)}, {"who am I": s(0, 5), "remember": s(1, 5)}, {"who am I": s(5, 5)}},
			want:   pipelineVerdict{Regressed: []string{"who am I"}, Flaky: []string{"remember"}},
			sides:  "head:all:3 base:remember,who am I:3 head:remember,who am I:5 base:who am I:5",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var sides []string
			i := 0
			run := func(side string, names []string, times int) (map[string]pipelineScore, error) {
				what := "all"
				if names != nil {
					what = strings.Join(names, ",")
				}
				sides = append(sides, fmt.Sprintf("%s:%s:%d", side, what, times))
				if i >= len(tc.rounds) {
					return nil, fmt.Errorf("round %d not expected", i+1)
				}
				i++
				return tc.rounds[i-1], nil
			}
			got, err := judgePipeline(run, 3, 5, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.Known, tc.want.Known) || !slices.Equal(got.Regressed, tc.want.Regressed) || !slices.Equal(got.Flaky, tc.want.Flaky) {
				t.Errorf("verdict %+v, want %+v", got, tc.want)
			}
			if s := strings.Join(sides, " "); s != tc.sides {
				t.Errorf("rounds %q, want %q", s, tc.sides)
			}
		})
	}
}

func TestPipelineFilter(t *testing.T) {
	if f := pipelineFilter([]string{"who am I?", "remember (a fact)"}); f != `^(who am I\?|remember \(a fact\))$` {
		t.Fatalf("filter %q", f)
	}
	if pipelineFilter(nil) != "" {
		t.Fatal("no names gives a filter")
	}
}

// pipelineStep gives the demo repo a pipeline-regressions step whose command stands in for
// tools/pipeline: round n's summary is summaries[n-1], and each round appends its side, repeat,
// filter and checkout ("change" when the build's feature.txt is there) to its evidence's
// rounds.txt (pipelineRounds).
func (g *rig) pipelineStep(summaries ...string) {
	g.t.Helper()
	fix := filepath.Join(g.r.Cfg.WorkRoot, "pipeline-fixtures")
	if err := os.MkdirAll(fix, 0o755); err != nil {
		g.t.Fatal(err)
	}
	for i, s := range summaries {
		if err := os.WriteFile(filepath.Join(fix, fmt.Sprintf("%d.json", i+1)), []byte(s), 0o644); err != nil {
			g.t.Fatal(err)
		}
	}
	g.verifyRepo(VerifyStep{Name: "pipeline scenarios", Pipeline: &PipelineVerify{}, Run: `set -e
n=$(basename "$LOOP_PIPELINE_OUT" | cut -d- -f2)
where=base; test -f feature.txt && where=change
echo "$LOOP_PIPELINE_SIDE $LOOP_PIPELINE_REPEAT '$LOOP_PIPELINE_FILTER' $where $LOOP_PIPELINE_SANDBOX" >> "$LOOP_EVIDENCE_DIR/rounds.txt"
echo "tools/pipeline: round $n"
cp ` + shellQuote(fix) + `/"$n.json" "$LOOP_PIPELINE_OUT"
exit 1`})
}

func (g *rig) pipelineRounds(id int64) string {
	g.t.Helper()
	return g.read(filepath.Join(g.lastRun(id, StageBuild), "verify", "verify-1-pipeline-scenarios", "evidence", "rounds.txt"))
}

// A scenario that fails 0/3 on the change and passes 3/3 on the base, but passes on the change's
// confirm round, is flaky: verify passes, and the PR's footer names it as not counted. A scenario
// failing on the base too is not the change's.
func TestPipelineVerifyFlakeIsNotARegression(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.pipelineStep(
		`[{"name": "who am I", "passed": 0, "runs": 3}, {"name": "computer control off", "passed": 0, "runs": 3}, {"name": "timer", "passed": 3, "runs": 3}, {"name": "voice", "passed": 0, "runs": 0, "skipped": "voice"}]`,
		`[{"name": "who am I", "passed": 3, "runs": 3}, {"name": "computer control off", "passed": 1, "runs": 3}]`,
		`[{"name": "who am I", "passed": 4, "runs": 5}]`,
	)
	id := g.toBuild("Add a feature")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Review || d.Item.PR != 7 {
		t.Fatalf("stage %s pr %d (question %q); log:\n%s", d.Item.Stage, d.Item.PR, d.Item.Question, g.logs.String())
	}
	if got, want := g.pipelineRounds(id), "head 3 '' change loop-1-verify\n"+
		"base 3 '^(computer control off|who am I)$' base loop-1-verify\n"+
		"head 5 '^(who am I)$' change loop-1-verify\n"; got != want {
		t.Errorf("rounds:\n%s\nwant:\n%s", got, want)
	}
	body := g.prs.bodies[7]
	if !strings.Contains(body, "- pipeline scenarios: passed (0s; flaky, not counted: who am I); `verify-1-pipeline-scenarios.log`") {
		t.Errorf("PR body:\n%s", body)
	}
	log := g.read(filepath.Join(g.lastRun(id, StageBuild), "verify", "verify-1-pipeline-scenarios", "verify-1-pipeline-scenarios.log"))
	for _, want := range []string{"tools/pipeline: round 1", "[pipeline verify] the base: main at ",
		"[pipeline verify] regressions: none; flaky, not counted: who am I; failing on the base too: computer control off"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if _, err := os.Stat(filepath.Join(g.r.Cfg.WorkRoot, "1-verify-base")); err == nil {
		t.Error("the base checkout is left behind")
	}
}

// A suspect that splits again in the confirm rounds is a regression: verify fails naming it, and
// nothing is pushed.
func TestPipelineVerifyConfirmedRegressionFails(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.pipelineStep(
		`[{"name": "who am I", "passed": 0, "runs": 3}, {"name": "timer", "passed": 3, "runs": 3}]`,
		`[{"name": "who am I", "passed": 3, "runs": 3}]`,
		`[{"name": "who am I", "passed": 0, "runs": 5}]`,
		`[{"name": "who am I", "passed": 5, "runs": 5}]`,
	)
	id := g.toBuild("Add a feature")
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Build || d.Item.BounceCount != 1 || d.Item.PR != 0 {
		t.Fatalf("stage %s bounces %d pr %d (question %q); log:\n%s", d.Item.Stage, d.Item.BounceCount, d.Item.PR, d.Item.Question, g.logs.String())
	}
	ev := g.advanced(id)
	reason, _ := ev.Data["reason"].(string)
	logTail, _ := ev.Data["log_tail"].(string)
	if reason != `Runner verify "pipeline scenarios" failed (regressed: who am I; 0s); nothing was pushed` ||
		!strings.Contains(logTail, "[pipeline verify] regressions: who am I; flaky, not counted: none") {
		t.Fatalf("event %s %+v", ev.Outcome, ev.Data)
	}
	if got := strings.Count(g.pipelineRounds(id), "\n"); got != 4 {
		t.Errorf("%d rounds:\n%s", got, g.pipelineRounds(id))
	}
}

// A round with no summary fails the step (not as a regression); a docs-only change skips a step
// whose when_changed names the code.
func TestPipelineVerifyNoSummaryAndSkip(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.pipelineStep() // no summary for round 1
	id := g.toBuild("Add a feature")
	g.runOnce()
	ev := g.advanced(id)
	if reason, _ := ev.Data["reason"].(string); !strings.Contains(reason, `failed (the change's round 1 exited 1 with no summary`) {
		t.Fatalf("event %s %+v", ev.Outcome, ev.Data)
	}

	g = newRig(t, fakeclaude.Good)
	g.pipelineStep()
	rc := g.r.Cfg.Repos["demo"]
	rc.Verify[0].WhenChanged = []string{"**/*.go", "go.mod"}
	g.r.Cfg.Repos["demo"] = rc
	id = g.toBuild("Add a feature")
	g.runOnce()
	if d := g.item(id); d.Item.Stage != pipeline.Review || !strings.Contains(g.prs.bodies[7], "- pipeline scenarios: skipped (no changed path matches **/*.go, go.mod)") {
		t.Fatalf("stage %s; body:\n%s", d.Item.Stage, g.prs.bodies[7])
	}
}
