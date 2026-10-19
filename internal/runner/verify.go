package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The runner's verify step (docs/phases/06b-all-repos.md, option 3 of
// docs/decisions/2026-10-15-runner-choices.md): after a build agent returns ready_for_pr and the
// runner has checked its commit against git, and before anything is pushed, the runner itself runs
// the repo's configured verify steps, outside the agent's sandbox. The commands come from the
// runner's config only; the agent can neither skip nor fake them, and a failure sends the build
// back with the log.

// VerifyLockWait is how long a run waits for another verify on this machine before giving up.
const VerifyLockWait = 30 * time.Minute

// verifyLogTail is how much of a step's output is kept as its log evidence and in the notes.
const (
	verifyLogLines = 80
	verifyLogBytes = 16 << 10
)

// errVerifyStopped: loopd asked to stop, the lease is gone, or the runner is shutting down. The
// build's commits stay in the runner's mirror; nothing is reported or pushed.
var errVerifyStopped = errors.New("verify stopped")

// VerifyResult is how one verify step went.
type VerifyResult struct {
	Name string
	// Skipped says why the step did not run ("" when it ran).
	Skipped  string
	ExitCode int
	TimedOut bool
	// Err is a problem running the step or collecting its output (the step counts as failed).
	Err     string
	Seconds int
	// Log is the tail of its output; LogFile that tail as an evidence file, Evidence the files the
	// step wrote to LOOP_EVIDENCE_DIR (named for upload: verify-<n>-<file>).
	Log      string
	LogFile  EvidenceFile
	Evidence []EvidenceFile
	// Masked is the verify_mask path a failed step could not read ("" for none): the fence hid it,
	// so the failure is the runner's config, not the change.
	Masked string
}

// Passed reports whether the step passed (a skipped step passes).
func (v VerifyResult) Passed() bool {
	return v.Skipped != "" || v.Err == "" && !v.TimedOut && v.ExitCode == 0
}

// Status is the step's result in a few words.
func (v VerifyResult) Status() string {
	switch {
	case v.Skipped != "":
		return "skipped (" + v.Skipped + ")"
	case v.Err != "":
		return "failed (" + v.Err + ")"
	case v.TimedOut:
		return fmt.Sprintf("timed out after %s", duration(v.Seconds))
	case v.ExitCode != 0:
		return fmt.Sprintf("failed (exit %d, %s)", v.ExitCode, duration(v.Seconds))
	}
	return fmt.Sprintf("passed (%s)", duration(v.Seconds))
}

// firstFailed is the first failed step, or nil.
func firstFailed(rs []VerifyResult) *VerifyResult {
	for i := range rs {
		if !rs[i].Passed() {
			return &rs[i]
		}
	}
	return nil
}

// trustedPaths are the changed paths (new and old) that are one of the repo's deny_write paths,
// under one, or match one as a glob, in order and once each.
func trustedPaths(deny, lines []string) []string {
	paths, err := pipeline.ParseChangedPaths(lines)
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range paths {
		for _, name := range []string{p.Path, p.OldPath} {
			if name != "" && isDenyWrite(deny, name) && !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	return out
}

func isDenyWrite(deny []string, name string) bool {
	for _, d := range deny {
		d = strings.TrimSuffix(filepath.ToSlash(filepath.Clean(d)), "/")
		if name == d || strings.HasPrefix(name, d+"/") || pipeline.GlobMatch(d, name) {
			return true
		}
	}
	return false
}

// trustRefusal is verify refusing a commit that changes trusted files without the approver's OK
// for it: the user must look, and may OK that one commit (pipeline.Trust).
type trustRefusal struct {
	head  string
	paths []string
}

func (e *trustRefusal) Error() string {
	return fmt.Sprintf("the change touches %s, one of the files the runner trusts (deny_write); verify does not run the agent's version of it, so the user must look", e.paths[0])
}

// touchesDenyWrite returns the first changed path (new or old) that is a deny_write path.
func touchesDenyWrite(deny, lines []string) string {
	if ps := trustedPaths(deny, lines); len(ps) > 0 {
		return ps[0]
	}
	return ""
}

// waitResult is how keepAlive ended.
type waitResult int

const (
	waitDone waitResult = iota
	waitTimeout
	waitStopped
)

// keepAlive waits for done while heartbeating the lease (with the verify step it waits on), up to
// limit (0: no limit). It returns waitStopped when loopd says stop, the lease is gone, or the run
// is cancelled (ru.done).
func (r *Runner) keepAlive(ctx context.Context, ru *run, note, step string, done <-chan struct{}, limit time.Duration) waitResult {
	stepStarted := r.Clock.Now()
	hb := r.Clock.NewTimer(r.heartbeatEvery())
	defer func() { hb.Stop() }()
	var limitC <-chan time.Time
	if limit > 0 {
		lt := r.Clock.NewTimer(limit)
		defer lt.Stop()
		limitC = lt.C()
	}
	for {
		select {
		case <-done:
			return waitDone
		case <-limitC:
			return waitTimeout
		case <-ru.done:
			return waitStopped
		case <-hb.C():
			stop, err := r.Client.Heartbeat(ctx, ru.lease, note, r.stepActivity("verify", step, ru.started, stepStarted))
			var ae *APIError
			if err == nil && stop || errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 {
				return waitStopped
			}
			if err != nil {
				r.Log.Warn("heartbeat", "item", ru.item.ID, "err", err)
			}
			hb = r.Clock.NewTimer(r.heartbeatEvery())
		}
	}
}

// acquireVerifyLock takes the machine's verify lock (one verify at a time: the GPU model, the
// emulator and the mobile build dir are single), heartbeating while it waits.
func (r *Runner) acquireVerifyLock(ctx context.Context, ru *run) (func(), error) {
	if f, err := tryLockFile(r.cfgOf(ru).VerifyLock); err != nil {
		return nil, fmt.Errorf("the verify lock %s: %w", r.cfgOf(ru).VerifyLock, err)
	} else if f != nil {
		return func() { closeLock(f) }, nil
	}
	r.Log.Info("waiting for the verify lock", "item", ru.item.ID, "lock", r.cfgOf(ru).VerifyLock)
	var f *os.File
	var lerr error
	got := make(chan struct{})
	go func() {
		f, lerr = lockFile(r.cfgOf(ru).VerifyLock)
		close(got)
	}()
	switch r.keepAlive(ctx, ru, "waiting for the verify lock", "waiting for the verify lock", got, VerifyLockWait) {
	case waitDone:
	case waitTimeout:
		go func() { <-got; closeLock(f) }()
		return nil, fmt.Errorf("another verify held %s for %s", r.cfgOf(ru).VerifyLock, VerifyLockWait)
	default:
		go func() { <-got; closeLock(f) }()
		return nil, errVerifyStopped
	}
	if lerr != nil {
		return nil, fmt.Errorf("the verify lock %s: %w", r.cfgOf(ru).VerifyLock, lerr)
	}
	return func() { closeLock(f) }, nil
}

func closeLock(f *os.File) {
	if f != nil {
		f.Close() // closing the descriptor releases the lock
	}
}

// runVerify runs the repo's verify steps on the build's commit, in order, stopping at the first
// failure. An error is the runner's problem (not the build's): a stop, the lock, a checkout, a
// change to a file verify trusts, or no time left.
func (r *Runner) runVerify(ctx context.Context, ru *run, head string, changed []string) ([]VerifyResult, error) {
	steps := ru.repo.Verify
	if len(steps) == 0 {
		return nil, nil
	}
	if err := r.ProbeFence(); err != nil {
		return nil, fmt.Errorf("verify cannot run: %w", err)
	}
	if paths := trustedPaths(ru.repo.DenyWrite, changed); len(paths) > 0 {
		if !ru.item.Trust.allows(head) {
			// Another head than the one OK'd: the OK carries only to the same trusted diff.
			sum := r.carriedTrust(ctx, ru, head, paths)
			if sum == "" {
				return nil, &trustRefusal{head: head, paths: paths}
			}
			r.Log.Info("the user's OK carries: the trusted files' diff is unchanged", "item", ru.item.ID, "from", ru.item.Trust.HeadSHA, "head", head)
			ru.trustCarried, ru.trustDiffSHA = head, sum
		}
		// The approver OK'd this commit (or its trusted diff): verify runs the agent's version once,
		// and the report says so (loopd uses the OK up).
		r.Log.Info("verify runs a trusted-file change on the user's OK", "item", ru.item.ID, "head", head, "by", ru.item.Trust.By)
		ru.trustUsed = head
	}
	// Stay inside loopd's per-run cap: a verify that would outlive it ends as the runner's own
	// failure, not an overtime.
	var deadline time.Time
	if ctl, err := r.Client.Control(ctx); err == nil && ctl.MaxRunMinutes > 0 {
		deadline = ru.started.Add(time.Duration(ctl.MaxRunMinutes)*time.Minute - time.Minute)
		if !r.Clock.Now().Before(deadline) {
			return nil, fmt.Errorf("the run's time cap (%d minutes) left no time to verify", ctl.MaxRunMinutes)
		}
	}
	release, err := r.acquireVerifyLock(ctx, ru)
	if err != nil {
		return nil, err
	}
	defer release()
	paths, _ := pipeline.ParseChangedPaths(changed)
	var out []VerifyResult
	for i, st := range steps {
		res := VerifyResult{Name: st.Name}
		if len(st.WhenChanged) > 0 && !anyMatch(st.WhenChanged, paths) {
			res.Skipped = "no changed path matches " + strings.Join(st.WhenChanged, ", ")
			out = append(out, res)
			continue
		}
		limit, capped := st.Timeout(), false
		if !deadline.IsZero() {
			left := deadline.Sub(r.Clock.Now())
			if left <= 0 {
				return out, fmt.Errorf("the run's time cap left no time for verify %q", st.Name)
			}
			if left < limit {
				limit, capped = left, true
			}
		}
		res, err := r.verifyStep(ctx, ru, i+1, st, head, limit)
		if err != nil {
			return out, err
		}
		if res.TimedOut && capped {
			return out, fmt.Errorf("verify %q ran into the run's time cap", st.Name)
		}
		out = append(out, res)
		if !res.Passed() {
			break
		}
	}
	return out, nil
}

func anyMatch(globs []string, paths []pipeline.ChangedPath) bool {
	for _, p := range paths {
		for _, g := range globs {
			if pipeline.GlobMatch(g, p.Path) || p.OldPath != "" && pipeline.GlobMatch(g, p.OldPath) {
				return true
			}
		}
	}
	return false
}

// verifyStep runs one step: in the worktree, or a fresh checkout of another configured repo at its
// base; output to a log in the run dir; files from LOOP_EVIDENCE_DIR collected.
func (r *Runner) verifyStep(ctx context.Context, ru *run, n int, st VerifyStep, head string, limit time.Duration) (VerifyResult, error) {
	res := VerifyResult{Name: st.Name}
	tag := fmt.Sprintf("verify-%d-%s", n, Slug(st.Name))
	dir := filepath.Join(ru.dir, "verify", tag)
	evDir := filepath.Join(dir, "evidence")
	if err := os.MkdirAll(evDir, 0o700); err != nil {
		return res, err
	}
	cwd := ru.wt
	if st.Repo != "" {
		rc, ok := r.cfgOf(ru).Repos[st.Repo]
		if !ok {
			return res, fmt.Errorf("verify %q: repo %q is not configured", st.Name, st.Repo)
		}
		mirror := r.mirrorPath(st.Repo)
		cwd = filepath.Join(r.cfgOf(ru).WorkRoot, fmt.Sprintf("%d-verify-%s", ru.item.ID, Slug(st.Repo)))
		unlock := r.lockMirror(mirror)
		err := r.Git.EnsureMirror(ctx, mirror, rc)
		if err == nil {
			err = r.Git.AddDetached(ctx, mirror, cwd, "refs/remotes/origin/"+rc.Base)
		}
		unlock()
		if err != nil {
			return res, fmt.Errorf("verify %q: checking out %s: %w", st.Name, st.Repo, err)
		}
		defer func() {
			unlock := r.lockMirror(mirror)
			r.Git.RemoveWorktree(context.WithoutCancel(ctx), mirror, cwd)
			unlock()
		}()
	}
	logPath := filepath.Join(dir, "output.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return res, err
	}
	cmd, err := r.fence(cwd, st.Run, append(r.agentEnv(),
		"LOOP_ITEM="+strconv.FormatInt(ru.item.ID, 10), "LOOP_HEAD="+head, "LOOP_WORKTREE="+ru.wt,
		"LOOP_EVIDENCE_DIR="+evDir, "LOOP_SANDBOX="+ru.sandbox, "LOOP_REPO="+ru.repo.Slug, "LOOP_BASE="+ru.repo.Base,
		"LOOP_MIRROR="+ru.mirror), []string{dir})
	if err != nil {
		logf.Close()
		return res, fmt.Errorf("verify %q cannot run fenced: %w", st.Name, err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	ownGroup(cmd)
	started := r.Clock.Now()
	r.Log.Info("verify starting", "item", ru.item.ID, "step", st.Name, "dir", cwd)
	if err := cmd.Start(); err != nil {
		logf.Close()
		res.Err = "could not start: " + err.Error()
		return res, nil
	}
	pid := cmd.Process.Pid
	exited := make(chan struct{})
	var state *os.ProcessState
	go func() {
		state, _ = cmd.Process.Wait()
		close(exited)
	}()
	w := r.keepAlive(ctx, ru, "verify: "+st.Name, st.Name, exited, limit)
	if w != waitDone {
		_ = signalGroup(pid, syscall.SIGTERM)
		grace := r.Clock.NewTimer(r.killGrace())
		select {
		case <-exited:
		case <-grace.C():
		}
		grace.Stop()
	}
	// Whatever the step left in its group goes with it (a harness it started with setsid does
	// not: its own command must take it down).
	_ = signalGroup(pid, syscall.SIGKILL)
	<-exited
	logf.Close()
	res.Seconds = int(r.Clock.Now().Sub(started).Seconds())
	if w == waitStopped {
		return res, errVerifyStopped
	}
	res.TimedOut = w == waitTimeout
	res.ExitCode = -1
	if state != nil {
		res.ExitCode = state.ExitCode()
	}
	b, _ := os.ReadFile(logPath)
	res.Log = logTail(string(b))
	if !res.Passed() {
		res.Masked = maskedRead(string(b), r.cfgOf(ru).VerifyMask)
	}
	tailPath := filepath.Join(dir, tag+".log")
	if err := os.WriteFile(tailPath, []byte(res.Log), 0o600); err != nil {
		return res, err
	}
	res.LogFile = EvidenceFile{Name: tag + ".log", Path: tailPath, Size: int64(len(res.Log))}
	files, skipped, err := EvidenceFiles(dir)
	if err != nil {
		res.Err = "its evidence: " + err.Error()
	}
	if len(skipped) > 0 {
		r.Log.Warn("verify evidence: skipped directories and dotfiles", "item", ru.item.ID, "step", st.Name, "skipped", strings.Join(skipped, ", "))
	}
	for _, f := range files {
		f.Name = tag + "-" + safeName(f.Name)
		res.Evidence = append(res.Evidence, f)
	}
	r.Log.Info("verify ended", "item", ru.item.ID, "step", st.Name, "status", res.Status())
	return res, nil
}

// maskedRead is the first mask path the log says could not be read: a file the fence binds
// /dev/null over gives "Permission denied", a path under a dir it puts a tmpfs on "No such file or
// directory" too. "" when the log names none.
func maskedRead(log string, mask []string) string {
	for _, line := range strings.Split(log, "\n") {
		low := strings.ToLower(line)
		denied := strings.Contains(low, "permission denied")
		missing := strings.Contains(low, "no such file or directory")
		if !denied && !missing {
			continue
		}
		for _, p := range mask {
			for rest := line; ; {
				i := strings.Index(rest, p)
				if i < 0 {
					break
				}
				rest = rest[i+len(p):]
				switch {
				case strings.HasPrefix(rest, ":") && denied, strings.HasPrefix(rest, "/"):
					return p
				}
			}
		}
	}
	return ""
}

// logTail is the end of a step's output: at most verifyLogLines lines and verifyLogBytes bytes.
func logTail(s string) string {
	s = tail(s, verifyLogLines)
	if len(s) > verifyLogBytes {
		s = "…" + s[len(s)-verifyLogBytes:]
	}
	return s
}

// uploadVerify attaches each step's log tail and evidence to the item, labelled as the runner's.
func (r *Runner) uploadVerify(ctx context.Context, ru *run, rs []VerifyResult) error {
	for _, v := range rs {
		if v.Skipped != "" {
			continue
		}
		label := fmt.Sprintf("runner verify %q: log, %s", v.Name, v.Status())
		if err := r.Client.Upload(ctx, ru.item.ID, v.LogFile.Path, v.LogFile.Name, label); err != nil {
			return err
		}
		for _, f := range v.Evidence {
			if err := r.Client.Upload(ctx, ru.item.ID, f.Path, f.Name, fmt.Sprintf("runner verify %q: %s", v.Name, f.Name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// VerifyFailedReason is the one line loopd, Slack and the user see about a failed verify: the step
// and its result. The end of its output goes separately (VerifyLogTail): to the next build's
// notes, never to Slack.
func VerifyFailedReason(v VerifyResult) string {
	return fmt.Sprintf("Runner verify %q %s; nothing was pushed", v.Name, v.Status())
}

// VerifyMaskedReason is the error for a step that failed on a masked path: only the path, never
// what the step printed.
func VerifyMaskedReason(v VerifyResult) string {
	return fmt.Sprintf("the runner's verify %q could not read %s: verify_mask hides it from fenced steps, "+
		"so this is the runner's config, not the change; nothing was pushed", v.Name, v.Masked)
}

// VerifyLogTail is the end of a failed step's output for the next build run.
func VerifyLogTail(v VerifyResult) string { return clip(tail(v.Log, 40), 3000) }

// maxTrustedDiff caps the trusted files' diff the runner attaches for the user.
const maxTrustedDiff = 1 << 20

// refuseTrusted reports verify's refusal of a trusted-file change: the item goes to the user with
// the commit, its trusted files and their diff stat, and the diff itself as evidence, so the user
// can look and OK that one commit (the Trust button, or `loop trust`). Nothing is pushed.
func (r *Runner) refuseTrusted(ctx context.Context, ru *run, out outcome, tr *trustRefusal) {
	rng := "refs/remotes/origin/" + ru.repo.Base + "..." + tr.head
	stat, err := r.Git.run(ctx, ru.wt, append([]string{"diff", "--stat=100", rng, "--"}, tr.paths...)...)
	if err != nil {
		stat = "(git diff --stat failed: " + clip(err.Error(), 200) + ")"
	}
	sum := ""
	if diff, err := r.trustedDiff(ctx, ru, tr.head, tr.paths); err != nil {
		r.Log.Warn("trusted files' diff", "item", ru.item.ID, "err", err)
	} else if diff != "" {
		sum = diffSHA256(diff) // of the uncut diff: a later head with the same one carries the OK
		f := filepath.Join(ru.dir, "trusted-files.diff")
		if len(diff) > maxTrustedDiff {
			diff = diff[:maxTrustedDiff] + "\n[cut at 1 MB]"
		}
		err := os.WriteFile(f, []byte(diff+"\n"), 0o600)
		if err == nil {
			err = r.Client.Upload(ctx, ru.item.ID, f, "trusted-files.diff", "the trusted files' diff verify refused at "+tr.head)
		}
		if err != nil {
			r.Log.Warn("attaching the trusted files' diff", "item", ru.item.ID, "err", err)
		}
	}
	msg := fmt.Sprintf("%s. Look at it (`loop show %d`, evidence trusted-files.diff); to let verify run commit %s once, use the Trust button or `loop trust %d --sha %s`",
		tr.Error(), ru.item.ID, tr.head, ru.item.ID, tr.head)
	r.advance(ctx, ru, "failed", r.metrics(ru, out, map[string]string{
		"error": msg, pipeline.FieldTrustSHA: tr.head, pipeline.FieldTrustPaths: strings.Join(tr.paths, ","),
		pipeline.FieldTrustDiffstat: clip(stat, 2000), pipeline.FieldTrustDiffSHA: sum}), nil)
}
