package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The overnight regression (docs/decisions/2026-10-19-overnight-regression.md): one claim runs
// every lane in turn in the run's checkout, under the verify lock and the verify steps' fence:
// scripts/e2e all (web and mobile, the scripted model), scripts/e2e voice (the voice lane, the real
// model) and the pipeline scenarios (tools/pipeline on a sandbox of their own, the real model);
// then each lane's failures once more (--filter): a test that passes on the rerun is flaky. A lane
// that does not run is the run's error, next to the others' tests.

// LanePipeline is the pipeline scenarios' lane (suite) in the report.
const LanePipeline = "pipeline"

// overnightSlack is what the runner keeps of loopd's cap for the reports and the uploads.
const overnightSlack = 30 * time.Minute

// overnightLane is one lane of the run: its command (sandbox: the sandbox it names) and how
// its rerun narrows it to some tests.
type overnightLane struct {
	name string
	// cmd is its command line; a rerun gives the suite whose failures it reruns ("": the lane's
	// own) and the filter.
	cmd func(sandbox, suite, filter string) string
	// e2e: it writes .e2e/<run>/report.json; else (pipeline) its JSON summary in the run dir.
	e2e bool
}

func overnightLanes(mobileDir string) []overnightLane {
	return []overnightLane{
		{name: "e2e", e2e: true, cmd: func(sb, suite, filter string) string {
			if suite == "" {
				suite = pipeline.SuiteAll
			}
			c := RegressionCommand(suite, sb, mobileDir)
			if filter != "" {
				c += " --filter " + shellQuote(filter)
			}
			return c
		}},
		{name: "voice", e2e: true, cmd: func(sb, _, filter string) string {
			c := "scripts/e2e voice --name " + shellQuote(sb)
			if filter != "" {
				c += " --filter " + shellQuote(filter)
			}
			return c
		}},
		{name: LanePipeline, cmd: func(sb, _, filter string) string { return PipelineCommand(sb, filter) }},
	}
}

// PipelineCommand runs the pipeline scenarios (jarvis-server's tools/pipeline, the real model
// through the tap) on a sandbox of their own, their JSON summary on stdout's file
// $LOOP_PIPELINE_OUT, and takes the sandbox down however they end.
func PipelineCommand(sandbox, filter string) string {
	run := "go run ./tools/pipeline run --sandbox " + shellQuote(sandbox) + " --json"
	if filter != "" {
		run += " --filter " + shellQuote(filter)
	}
	return "scripts/sandbox up --name " + shellQuote(sandbox) + " || exit 3; " +
		run + ` > "$LOOP_PIPELINE_OUT"; rc=$?; scripts/sandbox down --name ` + shellQuote(sandbox) + " --purge; exit $rc"
}

// PipelineSummary is tools/pipeline run --json's output: a scenario each.
type PipelineSummary []struct {
	Name     string   `json:"name"`
	Passed   int      `json:"passed"`
	Runs     int      `json:"runs"`
	Failures []string `json:"failures"`
	Skipped  string   `json:"skipped"`
}

// PipelineTests are the summary's scenarios as tests of lane pipeline, filed in repo.
func PipelineTests(raw []byte, repo string) ([]RegressionTest, error) {
	var sum PipelineSummary
	if err := json.Unmarshal(raw, &sum); err != nil {
		return nil, fmt.Errorf("the pipeline scenarios' summary: %w", err)
	}
	tests := make([]RegressionTest, 0, len(sum))
	for _, sc := range sum {
		t := RegressionTest{Suite: LanePipeline, Title: sc.Name, File: "tools/pipeline/scenarios", Repo: repo}
		switch {
		case sc.Skipped != "":
			t.Status = "skipped"
		case sc.Runs > 0 && sc.Passed == sc.Runs:
			t.Status = "passed"
		default:
			t.Status = "failed"
			t.Error = clip(strings.Join(sc.Failures, "\n"), 3000)
			if t.Error == "" {
				t.Error = fmt.Sprintf("%d of %d runs passed", sc.Passed, sc.Runs)
			}
		}
		tests = append(tests, t)
	}
	return tests, nil
}

// laneResult is what one run of a lane gave: its tests, the files to upload, its report's dir and
// scripts/e2e's run id (e2e lanes), or why it gave nothing.
type laneResult struct {
	tests  []RegressionTest
	shots  map[string]string
	runDir string
	run    string
	errs   string
	err    error
}

// RerunFilter is the --filter of a lane's rerun: its failed tests' titles, regexp-quoted (for the
// web suite the test's own name, after the file's: Playwright greps the full title).
func RerunFilter(titles []string) string {
	var parts []string
	for _, t := range titles {
		if i := strings.LastIndex(t, " › "); i >= 0 {
			t = t[i+len(" › "):]
		}
		parts = append(parts, regexp.QuoteMeta(t))
	}
	return strings.Join(parts, "|")
}

// runOvernight runs the overnight run's lanes, then reruns their failures, and returns the report.
// rep has the heads it runs.
func (r *Runner) runOvernight(ctx context.Context, ru *run, reg Regression, rep RegressionReport, mobileDir string, repos map[string]string, writable []string, log *slog.Logger) (RegressionReport, error) {
	cfg := ru.cfg.Regression
	budget := func() time.Duration {
		left := pipeline.OvernightMaxRun - overnightSlack - r.Clock.Now().Sub(ru.started)
		return min(cfg.Timeout(), left)
	}
	repos[LanePipeline], repos["voice"] = ru.repo.Slug, ru.repo.Slug
	var errs, runs []string
	var tests []RegressionTest
	for i, lane := range overnightLanes(mobileDir) {
		res := r.runLane(ctx, ru, reg, lane, fmt.Sprintf("%s-%d", ru.sandbox, i), "", "", repos, writable, budget(), log)
		if errors.Is(res.err, errVerifyStopped) {
			return rep, errVerifyStopped
		}
		if res.err != nil {
			errs = append(errs, fmt.Sprintf("lane %s: %s", lane.name, oneLineText(res.err.Error())))
			continue
		}
		if res.errs != "" {
			errs = append(errs, res.errs)
		}
		if res.run != "" {
			runs = append(runs, res.run)
		}
		r.uploadShots(ctx, reg.ID, res.runDir, res.tests, res.shots, log)
		if lane.e2e {
			r.uploadReport(ctx, reg.ID, res.runDir, lane.name+"-", log)
		}
		// The rerun: the lane's failures once more, a suite at a time; a pass is flaky.
		failed := map[string][]string{}
		var suites []string
		for _, t := range res.tests {
			if t.Status == "passed" || t.Status == "skipped" {
				continue
			}
			suite := t.Suite
			if !lane.e2e || lane.name != "e2e" {
				suite = ""
			}
			if _, ok := failed[suite]; !ok {
				suites = append(suites, suite)
			}
			failed[suite] = append(failed[suite], t.Title)
		}
		for _, suite := range suites {
			if budget() <= 0 {
				break
			}
			again := r.runLane(ctx, ru, reg, lane, fmt.Sprintf("%s-%dr", ru.sandbox, i), suite, RerunFilter(failed[suite]), repos, writable, budget(), log)
			if errors.Is(again.err, errVerifyStopped) {
				return rep, errVerifyStopped
			}
			if again.err != nil {
				log.Warn("overnight rerun", "lane", lane.name, "suite", suite, "err", again.err)
			}
			MarkFlaky(res.tests, again.tests)
		}
		tests = append(tests, res.tests...)
	}
	rep.Tests, rep.E2ERun, rep.Error = tests, strings.Join(runs, ","), strings.Join(errs, "; ")
	log.Info("overnight regression ended", "tests", len(tests), "errors", rep.Error)
	return rep, nil
}

// MarkFlaky marks each failed test of first that passed in rerun (same suite and title) flaky,
// its error the first run's.
func MarkFlaky(first, rerun []RegressionTest) {
	passed := map[string]bool{}
	for _, t := range rerun {
		if t.Status == "passed" {
			passed[t.Suite+"\x00"+t.Title] = true
		}
	}
	for i := range first {
		t := &first[i]
		if t.Status == "passed" || t.Status == "skipped" || !passed[t.Suite+"\x00"+t.Title] {
			continue
		}
		t.Status = "flaky"
		t.Error = clip("first run: "+t.Error, 4000)
	}
}

// runLane runs one lane (filter: a rerun's) and reads what it gave.
func (r *Runner) runLane(ctx context.Context, ru *run, reg Regression, lane overnightLane, sandbox, suite, filter string, repos map[string]string, writable []string, timeout time.Duration, log *slog.Logger) laneResult {
	var res laneResult
	if timeout <= 0 {
		res.err = errors.New("not run: the overnight run's time is spent")
		return res
	}
	what := lane.name
	if filter != "" {
		what += "-rerun"
		if suite != "" {
			what = suite + "-rerun"
		}
	}
	before, _, _ := latestE2EReport(ru.wt)
	cmdline := lane.cmd(sandbox, suite, filter)
	summary := filepath.Join(ru.dir, what+".json")
	if !lane.e2e {
		cmdline = "LOOP_PIPELINE_OUT=" + shellQuote(summary) + "; export LOOP_PIPELINE_OUT; " + cmdline
	}
	exit, out, err := r.runFenced(ctx, ru, reg, cmdline, what+".log", "overnight "+what, writable, timeout, log)
	if err != nil {
		res.err = err
		return res
	}
	if !lane.e2e {
		raw, err := os.ReadFile(summary)
		if err == nil {
			res.tests, err = PipelineTests(raw, repos[LanePipeline])
		}
		if err != nil || len(res.tests) == 0 {
			res.err = fmt.Errorf("exited %d with no summary (%v); its log ends: %s", exit, err, clip(tail(out, 3), 400))
		}
		return res
	}
	runDir, raw, err := latestE2EReport(ru.wt)
	if err != nil || runDir == before {
		res.err = fmt.Errorf("scripts/e2e exited %d with no report; its log ends: %s", exit, clip(tail(out, 3), 400))
		return res
	}
	e2e, err := ParseE2EReport(raw)
	if err != nil {
		res.err = err
		return res
	}
	prefix := lane.name + "-"
	if filter != "" {
		prefix = what + "-"
	}
	res.runDir, res.run, res.errs = runDir, e2e.Run, e2e.SuiteErrors()
	res.tests, res.shots = e2e.RegressionTests(repos, prefix)
	return res
}
