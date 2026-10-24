package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// QA proofs (docs/decisions/2026-10-18-qa-proofs.md): the runner with the land lane (the one that
// works on main) asks loopd for the qa items whose proof is due a check, runs each one's command
// checks fenced, as verify steps run, in a checkout of the repo's main, and reports how they went
// (proof_checked). loopd evaluates the usage checks itself and decides: verified, reopened, or
// still waiting. Before the command, the repo's Prepare and Setup commands run in the checkout, as
// before a build. A check the runner cannot run (no fence, no checkout) is left out of the report
// and stays pending: the runner's trouble is not the change's. One whose setup failed, or whose
// output shows it never got to test the change (envFailure), is reported as pipeline.CheckError:
// loopd records it on the item and never counts it toward a reopen.

// ProofCommandTimeout bounds one command check.
const ProofCommandTimeout = 10 * time.Minute

// ProofEvery is how often the runner asks loopd for due proofs (loopd says what is due: every
// pipeline.ProofRecheck for a pending one).
const ProofEvery = time.Minute

// CheckProofs checks every due proof of a repo this runner has. A failure leaves the item for the
// next pass.
func (r *Runner) CheckProofs(ctx context.Context) error {
	rows, err := r.Client.ProofsDue(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		key, repo, ok := r.Cfg.Repo(row.Repo)
		if !ok {
			continue // another runner's repo
		}
		log := r.Log.With("item", row.ID, "repo", repo.Slug)
		d, err := r.Client.Detail(ctx, row.ID)
		if err != nil {
			log.Warn("proof: read item", "err", err)
			continue
		}
		if d.Item.Proof == nil {
			continue
		}
		results := []pipeline.CheckResult{}
		for i, c := range d.Item.Proof.Checks {
			if c.Kind != pipeline.ProofCommand || c.State == pipeline.CheckPassed {
				continue
			}
			if runsGo(c.Run) {
				if why := r.goBroken(ctx); why != "" {
					// Not run, not reported: the check stays pending for a runner whose Go works (#591).
					log.Info("proof: go check left to another runner", "check", i, "go", why)
					continue
				}
			}
			res, err := r.proofCommand(ctx, key, repo, d.Item, i, c)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				log.Warn("proof: command check not run", "check", i, "err", err)
				continue
			}
			results = append(results, res)
		}
		err = r.Client.Event(ctx, row.ID, "proof_checked", map[string]any{"results": results})
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == "wrong_stage" {
			err = nil // another runner checked it, or the user already did
		}
		if err != nil {
			log.Warn("proof_checked event", "err", err)
			continue
		}
		log.Info("proof checked", "commands", len(results))
	}
	return nil
}

// proofCommand runs check n (a command) of item it, fenced, in a fresh checkout of the repo's
// main: exit 0 passes. Its evidence is how it ended and the tail of its output.
func (r *Runner) proofCommand(ctx context.Context, key string, repo RepoConfig, it Item, n int, c pipeline.ProofCheck) (pipeline.CheckResult, error) {
	res := pipeline.CheckResult{Check: n}
	dir := filepath.Join(r.Cfg.WorkRoot, "proofs", fmt.Sprintf("%d-%d", it.ID, n))
	if err := os.RemoveAll(dir); err != nil {
		return res, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return res, err
	}
	mirror := r.mirrorPath(key)
	cwd := filepath.Join(r.Cfg.WorkRoot, fmt.Sprintf("%d-proof", it.ID))
	unlock := r.lockMirror(mirror)
	err := r.Git.EnsureMirror(ctx, mirror, repo)
	if err == nil && it.MergeSHA != "" && !r.Git.isAncestor(ctx, mirror, it.MergeSHA, "refs/remotes/origin/"+repo.Base) {
		// The fetched view of main can lag the merge (#532): a command run against a main that
		// does not have the change yet fails for the runner's trouble, not the change's. Not run,
		// not reported: the check stays pending and the next pass retries.
		err = fmt.Errorf("%s's %s does not have the merge commit %s yet", repo.Slug, repo.Base, short7(it.MergeSHA))
	}
	if err == nil {
		err = r.Git.AddDetached(ctx, mirror, cwd, "refs/remotes/origin/"+repo.Base)
	}
	unlock()
	if err != nil {
		return res, fmt.Errorf("checking out %s's %s: %w", repo.Slug, repo.Base, err)
	}
	defer func() {
		unlock := r.lockMirror(mirror)
		r.Git.RemoveWorktree(context.WithoutCancel(ctx), mirror, cwd)
		unlock()
	}()
	// The repo's Prepare and Setup commands, as before a build or a regression run: without them
	// a command that needs what they fetch fails before it tests anything (#282).
	setupErr := func() error {
		for _, c := range repo.Prepare {
			if out, err := r.shell(ctx, cwd, c, SetupTimeout); err != nil {
				return fmt.Errorf("prepare %q: %v: %s", c, err, clip(tail(out, 5), 500))
			}
		}
		if err := r.runSetup(ctx, repo.Setup, cwd, dir); err != nil {
			return err
		}
		return r.buildProofVenv(ctx, cwd, dir, c.Run)
	}()
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if setupErr != nil {
		res.State, res.Evidence = pipeline.CheckError, clip(setupErr.Error(), 1400)
		r.Log.Warn("proof: command check could not run", "item", it.ID, "check", n, "err", setupErr)
		return res, nil
	}
	logPath := filepath.Join(dir, "output.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return res, err
	}
	cmd, err := r.fence(cwd, c.Run, append(venvEnv(r.agentEnv(), cwd),
		"LOOP_ITEM="+strconv.FormatInt(it.ID, 10), "LOOP_REPO="+repo.Slug, "LOOP_BASE="+repo.Base,
		"LOOP_MERGE_SHA="+it.MergeSHA, "LOOP_EVIDENCE_DIR="+dir), []string{dir})
	if err != nil {
		logf.Close()
		return res, fmt.Errorf("cannot run fenced: %w", err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	ownGroup(cmd)
	started := r.Clock.Now()
	if err := cmd.Start(); err != nil {
		logf.Close()
		return res, fmt.Errorf("could not start: %w", err)
	}
	pid := cmd.Process.Pid
	exited := make(chan struct{})
	var state *os.ProcessState
	go func() {
		state, _ = cmd.Process.Wait()
		close(exited)
	}()
	limit := r.Clock.NewTimer(ProofCommandTimeout)
	timedOut := false
	select {
	case <-exited:
	case <-limit.C():
		timedOut = true
	case <-ctx.Done():
	}
	limit.Stop()
	select {
	case <-exited:
	default:
		_ = signalGroup(pid, syscall.SIGTERM)
		grace := r.Clock.NewTimer(r.killGrace())
		select {
		case <-exited:
		case <-grace.C():
		}
		grace.Stop()
	}
	_ = signalGroup(pid, syscall.SIGKILL)
	<-exited
	logf.Close()
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	secs := int(r.Clock.Now().Sub(started).Seconds())
	code := -1
	if state != nil {
		code = state.ExitCode()
	}
	b, _ := os.ReadFile(logPath)
	out := strings.TrimSpace(tail(logTail(string(b)), 20))
	status := fmt.Sprintf("exit %d after %s", code, duration(secs))
	res.State = pipeline.CheckPassed
	switch {
	case timedOut:
		status = "timed out after " + duration(int(ProofCommandTimeout.Seconds()))
		res.State = pipeline.CheckFailed
	case code != 0 && envFailure(code, string(b), r.Clock.Now().Sub(started)):
		res.State = pipeline.CheckError
	case code != 0:
		res.State = pipeline.CheckFailed
	}
	if last := lastLine(out); last != "" {
		status += ": " + last
	}
	res.Evidence = clip(status+"\n"+out, 1400)
	if res.State == pipeline.CheckError {
		r.Log.Warn("proof: command check could not run", "item", it.ID, "check", n, "status", status)
	}
	r.Log.Info("proof command ended", "item", it.ID, "check", n, "state", res.State, "status", status)
	return res, nil
}

// envFailure tells whether a failed command never got to test the change: exit 127 (a tool not
// installed), go's "[setup failed]" (a package whose embedded or fetched files are missing), a
// broken toolchain ("no such tool", GOROOT, mise: #292, #443, #444, #510), pytest import or
// collection errors (#527), or an errors-only pytest summary in under a second (nothing was
// collected, so nothing ran). A "[build failed]" is not one: the merged change may have broken
// the build (docs/decisions/2026-10-19-proof-setup.md); so a failure whose output names none of
// these, however fast — the change may be what the command greps for.
func envFailure(code int, out string, ran time.Duration) bool {
	if code == 0 {
		return false
	}
	if code == 127 {
		return true
	}
	for _, sig := range []string{
		"[setup failed]",
		"no such tool", // go: no such tool "vet"
		"cannot find GOROOT",
		"mise ERROR", // mise could not provide the toolchain
		"ImportError while importing test module",
		"error during collection",
		"errors during collection",
	} {
		if strings.Contains(out, sig) {
			return true
		}
	}
	return ran < time.Second && pytestErrorsOnly.MatchString(out)
}

// pytestErrorsOnly is pytest's summary when only errors happened ("===== 8 errors in 0.05s ====="):
// no test passed or failed, so with a sub-second run nothing tested the change. A summary that
// counts failed tests ("2 failed, 1 error in …") does not match: those ran.
var pytestErrorsOnly = regexp.MustCompile(`=+ \d+ errors? in [0-9.]+s`)

// buildProofVenv builds the checkout's Python venv for a python proof command when the repo's
// setup made none (#527, docs/decisions/2026-10-23-proof-env-hardening.md): the venv, then the
// package (pyproject.toml, editable with its dev extra) or its requirements*.txt into it, appended
// to setup.log under the same SetupTimeout per command. A failure is a setup failure: the check
// errors and never strikes. A repo with nothing to install gets no venv: the command's own error
// says more than a bare venv would.
func (r *Runner) buildProofVenv(ctx context.Context, cwd, logDir, run string) error {
	if !pythonProof(run) {
		return nil
	}
	if fi, err := os.Stat(filepath.Join(cwd, ".venv", "bin")); err == nil && fi.IsDir() {
		return nil // the repo's setup made one; venvEnv activates it
	}
	var installs []string
	if _, err := os.Stat(filepath.Join(cwd, "pyproject.toml")); err == nil {
		// pip skips an extra the package does not declare, with a warning.
		installs = append(installs, `.venv/bin/pip install -q -e '.[dev]'`)
	} else if reqs, _ := filepath.Glob(filepath.Join(cwd, "requirements*.txt")); len(reqs) > 0 {
		for _, f := range reqs {
			installs = append(installs, ".venv/bin/pip install -q -r "+filepath.Base(f))
		}
	} else {
		return nil
	}
	return r.runSetup(ctx, append([]string{"python3 -m venv .venv"}, installs...), cwd, logDir)
}

// pythonProof tells whether a proof command wants Python: it names python, pytest or pip.
var pythonProof = regexp.MustCompile(`\b(python3?|pytest|pip3?)\b`).MatchString

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return clip(strings.TrimSpace(s), 200)
}

// checkProofsLoop runs CheckProofs (under ctx) every ProofEvery until loopCtx ends.
func (r *Runner) checkProofsLoop(loopCtx, ctx context.Context) {
	for loopCtx.Err() == nil {
		if err := r.CheckProofs(ctx); err != nil && ctx.Err() == nil {
			r.Log.Warn("check proofs", "err", err)
		}
		t := r.Clock.NewTimer(ProofEvery)
		select {
		case <-loopCtx.Done():
		case <-t.C():
		}
		t.Stop()
	}
}
