package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The pipeline-regressions verify step (docs/decisions/2026-10-21-pipeline-verify-flakes.md). The
// pipeline scenarios run the real (small) model, and about a dozen fail on any run, most of them
// on main too, so a step that fails on any failure says nothing. The rule is "a regression passes
// every run on the base and none on the change", and with a 9B model three runs a side turn one
// up by chance on most PRs. So the runner runs it itself, in rounds, each through the step's
// command (PipelineCommand by default) with LOOP_PIPELINE_SIDE, _FILTER, _REPEAT, _SANDBOX and
// _OUT set:
//
//  1. the change, every scenario, Repeat runs each: the candidates fail every run;
//  2. the base (a fresh checkout of the mirror's base branch), the candidates only: a suspect
//     passed every run there (one that fails on the base too is known, not the change's);
//  3. the change again, the suspects only, Confirm runs each: one that passes once is flaky;
//  4. the base again, the rest: one that still passes every run is a regression, else flaky.
//
// The step fails on the regressions only; the flaky ones are named in its status (the PR's
// footer) and log, and not counted.

// pipelineScore is one scenario's runs in one round.
type pipelineScore struct {
	Passed, Runs int
	Skipped      bool
}

func (s pipelineScore) allPassed() bool { return !s.Skipped && s.Runs > 0 && s.Passed == s.Runs }
func (s pipelineScore) allFailed() bool { return !s.Skipped && s.Runs > 0 && s.Passed == 0 }

// parsePipelineScores reads tools/pipeline run --json's summary.
func parsePipelineScores(raw []byte) (map[string]pipelineScore, error) {
	var sum PipelineSummary
	if err := json.Unmarshal(raw, &sum); err != nil {
		return nil, fmt.Errorf("the pipeline scenarios' summary: %w", err)
	}
	if len(sum) == 0 {
		return nil, fmt.Errorf("the pipeline scenarios' summary lists no scenario")
	}
	out := make(map[string]pipelineScore, len(sum))
	for _, sc := range sum {
		out[sc.Name] = pipelineScore{Passed: sc.Passed, Runs: sc.Runs, Skipped: sc.Skipped != ""}
	}
	return out, nil
}

// pipelineVerdict is what the rounds came to.
type pipelineVerdict struct {
	// Known failed every run on the change and not every run passed on the base (or the base has
	// no such scenario); Regressed and Flaky are the suspects that did and did not confirm.
	Known, Regressed, Flaky []string
}

// pipelineRound runs one round on a side: the names to run (nil: all) and how many times each.
type pipelineRound func(side string, names []string, repeat int) (map[string]pipelineScore, error)

// judgePipeline plays the rounds (see the top of this file) and logs each to log.
func judgePipeline(run pipelineRound, repeat, confirm int, log io.Writer) (pipelineVerdict, error) {
	var v pipelineVerdict
	head, err := run("head", nil, repeat)
	if err != nil {
		return v, err
	}
	var candidates []string
	for name, s := range head {
		if s.allFailed() {
			candidates = append(candidates, name)
		}
	}
	slices.Sort(candidates)
	fmt.Fprintf(log, "\n[pipeline verify] %d scenarios on the change; failed every run of %d: %s\n", len(head), repeat, listOrNone(candidates))
	if len(candidates) == 0 {
		return v, nil
	}
	base, err := run("base", candidates, repeat)
	if err != nil {
		return v, err
	}
	var suspects []string
	for _, name := range candidates {
		if base[name].allPassed() {
			suspects = append(suspects, name)
		} else {
			v.Known = append(v.Known, name)
		}
	}
	fmt.Fprintf(log, "[pipeline verify] on the base, %d runs each: passed every run (suspects): %s; not (failing on the base too, not counted): %s\n",
		repeat, listOrNone(suspects), listOrNone(v.Known))
	if len(suspects) == 0 {
		return v, nil
	}
	headAgain, err := run("head", suspects, confirm)
	if err != nil {
		return v, err
	}
	var still []string
	for _, name := range suspects {
		if headAgain[name].allFailed() {
			still = append(still, name)
		} else {
			v.Flaky = append(v.Flaky, name)
		}
	}
	fmt.Fprintf(log, "[pipeline verify] the suspects %d more times on the change: failed every run: %s; passed at least once (flaky): %s\n",
		confirm, listOrNone(still), listOrNone(v.Flaky))
	if len(still) == 0 {
		return v, nil
	}
	baseAgain, err := run("base", still, confirm)
	if err != nil {
		return v, err
	}
	var flakyOnBase []string
	for _, name := range still {
		if baseAgain[name].allPassed() {
			v.Regressed = append(v.Regressed, name)
		} else {
			flakyOnBase = append(flakyOnBase, name)
		}
	}
	v.Flaky = append(v.Flaky, flakyOnBase...)
	slices.Sort(v.Flaky)
	fmt.Fprintf(log, "[pipeline verify] and %d more times on the base: passed every run (regressions): %s; failed at least once (flaky): %s\n",
		confirm, listOrNone(v.Regressed), listOrNone(flakyOnBase))
	return v, nil
}

func listOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// pipelineFilter is a round's --filter: the names, regexp-quoted and anchored.
func pipelineFilter(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return "^(" + RerunFilter(names) + ")$"
}

// pipelineVerify runs a pipeline-regressions step and fills res: ExitCode 1 with Regressed, or
// passed (Flaky the suspects that did not confirm); exit 3 when a round's sandbox did not come up,
// Err when a round gave no summary. The base is a fresh checkout of the mirror's base branch,
// prepared as a build's worktree is. An error is the runner's (a stop, a checkout, the fence).
func (r *Runner) pipelineVerify(ctx context.Context, ru *run, st VerifyStep, head, evDir, dir string, log io.Writer, limit time.Duration, res *VerifyResult) error {
	repeat, confirm := st.Pipeline.Runs()
	deadline := r.Clock.Now().Add(limit)
	baseDir := ""
	defer func() {
		if baseDir != "" {
			unlock := r.lockMirror(ru.mirror)
			r.Git.RemoveWorktree(context.WithoutCancel(ctx), ru.mirror, baseDir)
			unlock()
		}
	}()
	checkoutBase := func() error {
		cwd := filepath.Join(r.cfgOf(ru).WorkRoot, fmt.Sprintf("%d-verify-base", ru.item.ID))
		unlock := r.lockMirror(ru.mirror)
		err := r.Git.AddDetached(ctx, ru.mirror, cwd, "refs/remotes/origin/"+ru.repo.Base)
		unlock()
		if err != nil {
			return fmt.Errorf("checking out the base %s: %w", ru.repo.Base, err)
		}
		baseDir = cwd
		for _, c := range ru.repo.Prepare {
			if out, err := r.shell(ctx, cwd, c, SetupTimeout); err != nil {
				return fmt.Errorf("prepare %q in the base checkout: %v: %s", c, err, clip(tail(out, 5), 500))
			}
		}
		base, _ := r.Git.Head(ctx, cwd)
		fmt.Fprintf(log, "\n[pipeline verify] the base: %s at %s\n", ru.repo.Base, base)
		return nil
	}
	n := 0
	round := func(side string, names []string, times int) (map[string]pipelineScore, error) {
		cwd := ru.wt
		if side == "base" && baseDir == "" {
			if err := checkoutBase(); err != nil {
				return nil, err
			}
		}
		if side == "base" {
			cwd = baseDir
		}
		left := deadline.Sub(r.Clock.Now())
		if left <= 0 {
			res.TimedOut = true
			return nil, errPipelineRound
		}
		n++
		filter := pipelineFilter(names)
		summary := filepath.Join(dir, fmt.Sprintf("pipeline-%d-%s.json", n, side))
		cmdline := st.Run
		if strings.TrimSpace(cmdline) == "" {
			cmdline = PipelineCommand(ru.verifySandbox, filter, times)
		}
		env := append(r.verifyEnv(ru, head, evDir, ru.verifySandbox),
			"LOOP_PIPELINE_SIDE="+side, "LOOP_PIPELINE_FILTER="+filter, "LOOP_PIPELINE_REPEAT="+strconv.Itoa(times),
			"LOOP_PIPELINE_SANDBOX="+ru.verifySandbox, "LOOP_PIPELINE_OUT="+summary)
		what := "every scenario"
		if names != nil {
			what = listOrNone(names)
		}
		fmt.Fprintf(log, "\n[pipeline verify] round %d, the %s, %d runs of %s\n", n, sideName(side), times, what)
		exit, timedOut, err := r.verifyProcess(ctx, ru, cwd, cmdline, env, []string{dir}, log, st.Name, left)
		if err != nil {
			return nil, err
		}
		if timedOut {
			res.TimedOut = true
			return nil, errPipelineRound
		}
		if exit == 3 {
			res.ExitCode = 3
			fmt.Fprintf(log, "[pipeline verify] round %d exited 3: its sandbox did not come up\n", n)
			return nil, errPipelineRound
		}
		raw, err := os.ReadFile(summary)
		var scores map[string]pipelineScore
		if err == nil {
			scores, err = parsePipelineScores(raw)
		}
		if err != nil {
			res.ExitCode = exit
			res.Err = fmt.Sprintf("the %s's round %d exited %d with no summary (%s)", sideName(side), n, exit, clip(err.Error(), 200))
			return nil, errPipelineRound
		}
		return scores, nil
	}
	v, err := judgePipeline(round, repeat, confirm, log)
	if errors.Is(err, errPipelineRound) {
		return nil
	}
	if err != nil {
		return err
	}
	res.Regressed, res.Flaky = v.Regressed, v.Flaky
	if len(v.Regressed) > 0 {
		res.ExitCode = 1
	}
	fmt.Fprintf(log, "\n[pipeline verify] regressions: %s; flaky, not counted: %s; failing on the base too: %s\n",
		listOrNone(v.Regressed), listOrNone(v.Flaky), listOrNone(v.Known))
	return nil
}

func sideName(side string) string {
	if side == "base" {
		return "base"
	}
	return "change"
}

// errPipelineRound ends the rounds on one that failed as a step (res says how).
var errPipelineRound = errors.New("a pipeline round failed")
