package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// still waiting. A check the runner cannot run (no fence, no checkout) is left out of the report
// and stays pending: the runner's trouble is not the change's.

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
	logPath := filepath.Join(dir, "output.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return res, err
	}
	cmd, err := r.fence(cwd, c.Run, append(r.agentEnv(),
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
	case code != 0:
		res.State = pipeline.CheckFailed
	}
	if last := lastLine(out); last != "" {
		status += ": " + last
	}
	res.Evidence = clip(status+"\n"+out, 1400)
	r.Log.Info("proof command ended", "item", it.ID, "check", n, "state", res.State, "status", status)
	return res, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return clip(strings.TrimSpace(s), 200)
}

// checkProofsLoop runs CheckProofs every ProofEvery until ctx ends.
func (r *Runner) checkProofsLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if err := r.CheckProofs(ctx); err != nil && ctx.Err() == nil {
			r.Log.Warn("check proofs", "err", err)
		}
		t := r.Clock.NewTimer(ProofEvery)
		select {
		case <-ctx.Done():
		case <-t.C():
		}
		t.Stop()
	}
}
