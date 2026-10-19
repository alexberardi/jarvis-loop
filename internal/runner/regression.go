package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime/multipart"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Regression runs (docs/decisions/2026-10-18-regression-trigger.md): the runner checks out the
// regression repo (jarvis-server) and the mobile app at their base, runs `scripts/e2e <suite>` in
// the fence the verify steps run in (it needs KVM, the harness cache and the emulator, which no
// agent sandbox has), holding the machine's verify lock, and reports the tests of its
// report.json with each failure's screenshot. No agent runs.

// StageRegression is the runner stage that claims regression runs.
const StageRegression = "regression"

// Regression is the run a claim carries (loopd's store.Regression).
type Regression struct {
	ID    int64  `json:"id"`
	Suite string `json:"suite"`
}

// RegressionReport is what the runner reports (loopd's store.RegressionReport).
type RegressionReport struct {
	Error      string           `json:"error,omitempty"`
	E2ERun     string           `json:"e2e_run,omitempty"`
	ServerHead string           `json:"server_head,omitempty"`
	MobileHead string           `json:"mobile_head,omitempty"`
	Tests      []RegressionTest `json:"tests"`
}

// RegressionTest is one test of the report; Screenshot names the file it was uploaded under.
type RegressionTest struct {
	Suite      string `json:"suite"`
	Title      string `json:"title"`
	File       string `json:"file,omitempty"`
	Line       int    `json:"line,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	MS         int64  `json:"ms"`
	Screenshot string `json:"screenshot,omitempty"`
	Recording  string `json:"recording,omitempty"`
	Repo       string `json:"repo,omitempty"`
}

// MaxRegressionShots caps the screenshots a run uploads (one per failure).
const MaxRegressionShots = 40

// E2EReport is scripts/e2e's report.json (schema 1, jarvis-server docs/dev/e2e.md): the fields
// the loop reads.
type E2EReport struct {
	Schema int    `json:"schema"`
	Run    string `json:"run"`
	Status string `json:"status"`
	Git    struct {
		SHA string `json:"sha"`
	} `json:"git"`
	Suites map[string]struct {
		Status string  `json:"status"`
		Error  *string `json:"error"`
	} `json:"suites"`
	Tests []struct {
		Suite       string   `json:"suite"`
		Title       string   `json:"title"`
		File        string   `json:"file"`
		Line        int      `json:"line"`
		Status      string   `json:"status"`
		DurationMS  int64    `json:"duration_ms"`
		Error       *string  `json:"error"`
		Screenshots []string `json:"screenshots"`
		// Attachments and Artifacts are a failure's other files (a voice case's reply.wav).
		Attachments []string `json:"attachments"`
		Artifacts   []string `json:"artifacts"`
	} `json:"tests"`
}

// ParseE2EReport reads a report.json.
func ParseE2EReport(raw []byte) (E2EReport, error) {
	var rep E2EReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return rep, fmt.Errorf("report.json: %w", err)
	}
	if rep.Schema != 1 {
		return rep, fmt.Errorf("report.json: schema %d, the loop reads 1", rep.Schema)
	}
	return rep, nil
}

// SuiteErrors says which suites did not run, and why ("" when all ran).
func (rep E2EReport) SuiteErrors() string {
	var out []string
	for name, s := range rep.Suites {
		if s.Status != "error" {
			continue
		}
		why := "it did not run"
		if s.Error != nil && strings.TrimSpace(*s.Error) != "" {
			why = strings.TrimSpace(*s.Error)
		}
		out = append(out, fmt.Sprintf("suite %s: %s", name, clip(oneLineText(why), 300)))
	}
	sort.Strings(out)
	return strings.Join(out, "; ")
}

// recordingExts are the files a failure's recording may be (a voice reply, a screen recording).
var recordingExts = []string{".wav", ".mp4", ".webm", ".mov"}

// RegressionTests are the report's tests for loopd: repos says which repo a suite's failure is
// filed in; shots are the files to upload (the upload name -> the file under the run's dir): the
// first screenshot of each failure's, at most MaxRegressionShots, and its first recording (an
// attachment or artifact that is audio or video), each a plain relative path. prefix goes before
// each upload name (an overnight run's lanes each have a report).
func (rep E2EReport) RegressionTests(repos map[string]string, prefix ...string) (tests []RegressionTest, shots map[string]string) {
	shots = map[string]string{}
	pre := strings.Join(prefix, "")
	plain := func(s string) (string, bool) {
		rel := filepath.Clean(s)
		return rel, s != "" && !filepath.IsAbs(rel) && rel != "." && rel != ".." && !strings.HasPrefix(rel, "../")
	}
	for i, t := range rep.Tests {
		rt := RegressionTest{Suite: t.Suite, Title: t.Title, File: t.File, Line: t.Line, Status: t.Status, MS: t.DurationMS, Repo: repos[t.Suite]}
		if t.Error != nil {
			rt.Error = *t.Error
		}
		failed := t.Status != "passed" && t.Status != "skipped"
		if failed && len(shots) < MaxRegressionShots {
			for _, s := range t.Screenshots {
				rel, ok := plain(s)
				if !ok {
					continue
				}
				name := fmt.Sprintf("%s%d-%s", pre, i+1, filepath.Base(rel))
				shots[name] = rel
				rt.Screenshot = name
				break
			}
		}
		if failed && len(shots) < MaxRegressionShots {
			for _, s := range append(slices.Clone(t.Attachments), t.Artifacts...) {
				rel, ok := plain(s)
				if !ok || !slices.Contains(recordingExts, strings.ToLower(filepath.Ext(rel))) {
					continue
				}
				name := fmt.Sprintf("%s%d-%s", pre, i+1, filepath.Base(rel))
				if _, dup := shots[name]; dup {
					continue
				}
				shots[name] = rel
				rt.Recording = name
				break
			}
		}
		tests = append(tests, rt)
	}
	return tests, shots
}

// RegressionCommand is the command line of a run: scripts/e2e's own (`--name` its sandbox,
// never default; `--mobile-repo` the app's checkout, when the runner has one).
func RegressionCommand(suite, sandbox, mobileDir string) string {
	cmd := "scripts/e2e " + suite + " --name " + shellQuote(sandbox)
	if mobileDir != "" && suite != pipeline.SuiteWeb {
		cmd += " --mobile-repo " + shellQuote(mobileDir)
	}
	return cmd
}

// RegressionResult finishes the regression run this runner holds.
func (c *Client) RegressionResult(ctx context.Context, id int64, rep RegressionReport) error {
	if rep.Tests == nil {
		rep.Tests = []RegressionTest{}
	}
	_, err := c.json(ctx, "POST", fmt.Sprintf("/regressions/%d/result", id), rep, nil)
	return err
}

// RegressionFile uploads a file of the regression run this runner holds.
func (c *Client) RegressionFile(ctx context.Context, id int64, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			fw, err := mw.CreateFormFile("file", name)
			if err != nil {
				return err
			}
			if _, err := io.Copy(fw, f); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	_, err = c.do(ctx, "POST", fmt.Sprintf("/regressions/%d/files", id), mw.FormDataContentType(), pr, nil)
	return err
}

// regression runs one claimed regression run and reports it. A stop (loopd's, or the runner
// shutting down) reports nothing: loopd puts the run back in the queue.
func (r *Runner) regression(ctx context.Context, cl *Claim, agent string) {
	log := r.Log.With("agent", agent)
	if cl.Reg == nil {
		log.Error("regression", "err", "loopd's claim has no run")
		return
	}
	reg := *cl.Reg
	log = log.With("regression", reg.ID, "suite", reg.Suite)
	ru := &run{stage: StageRegression, lease: cl.Lease.ID, agent: agent, cfg: r.Config(), started: r.Clock.Now(), done: ctx.Done()}
	ru.dir = uniqueRunDir(filepath.Join(ru.cfg.WorkRoot, "runs", fmt.Sprintf("R%d-regression-%s", reg.ID, ru.started.Format("20060102T150405Z"))))
	ru.sandbox = fmt.Sprintf("loop-reg-%d", reg.ID)
	report := func(rep RegressionReport) {
		rep.Error = clip(strings.TrimSpace(rep.Error), 1500)
		if err := r.Client.RegressionResult(context.WithoutCancel(ctx), reg.ID, rep); err != nil {
			log.Error("regression result", "err", err)
			return
		}
		log.Info("reported", "tests", len(rep.Tests), "error", rep.Error)
	}
	rep, err := r.runRegression(ctx, ru, reg, log)
	if errors.Is(err, errVerifyStopped) {
		log.Info("regression stopped; loopd queues it again")
		return
	}
	if err != nil {
		log.Error("regression", "err", err)
		rep.Error = err.Error()
	}
	report(rep)
}

// runRegression checks out, prepares and runs scripts/e2e, uploads the screenshots and the report,
// and returns what to report. An error is why there is no report (errVerifyStopped: stopped).
func (r *Runner) runRegression(ctx context.Context, ru *run, reg Regression, log *slog.Logger) (RegressionReport, error) {
	var rep RegressionReport
	cfg := ru.cfg.Regression
	if cfg == nil {
		return rep, errors.New("this runner has no regression config")
	}
	if err := os.MkdirAll(ru.dir, 0o700); err != nil {
		return rep, err
	}
	if err := r.ProbeFence(); err != nil {
		return rep, fmt.Errorf("the regression cannot run: %w", err)
	}
	ru.repoKey, ru.repo = cfg.Repo, ru.cfg.Repos[cfg.Repo]
	checkout := func(key string) (string, string, func(), error) {
		rc := ru.cfg.Repos[key]
		mirror := r.mirrorPath(key)
		dir := filepath.Join(ru.cfg.WorkRoot, fmt.Sprintf("R%d-regression-%s", reg.ID, Slug(key)))
		unlock := r.lockMirror(mirror)
		err := r.Git.EnsureMirror(ctx, mirror, rc)
		if err == nil {
			err = r.Git.AddDetached(ctx, mirror, dir, "refs/remotes/origin/"+rc.Base)
		}
		unlock()
		done := func() {
			unlock := r.lockMirror(mirror)
			r.Git.RemoveWorktree(context.WithoutCancel(ctx), mirror, dir)
			unlock()
		}
		if err != nil {
			done()
			return "", "", nil, fmt.Errorf("checking out %s: %w", rc.Slug, err)
		}
		head, _ := r.Git.Head(ctx, dir)
		return dir, head, done, nil
	}
	wt, head, done, err := r.checkoutHeartbeat(ctx, ru, "checking out "+ru.repo.Slug, func() (string, string, func(), error) { return checkout(cfg.Repo) })
	if err != nil {
		return rep, err
	}
	defer done()
	ru.wt, ru.repoDir, rep.ServerHead = wt, wt, head
	repos := map[string]string{pipeline.SuiteWeb: ru.repo.Slug, pipeline.SuiteMobile: ru.repo.Slug}
	var mobileDir string
	if cfg.MobileRepo != "" && reg.Suite != pipeline.SuiteWeb {
		mrc := ru.cfg.Repos[cfg.MobileRepo]
		dir, mhead, mdone, err := r.checkoutHeartbeat(ctx, ru, "checking out "+mrc.Slug, func() (string, string, func(), error) { return checkout(cfg.MobileRepo) })
		if err != nil {
			return rep, err
		}
		defer mdone()
		mobileDir, rep.MobileHead = dir, mhead
		repos[pipeline.SuiteMobile] = mrc.Slug
	}
	for _, c := range ru.repo.Prepare {
		if out, err := r.shell(ctx, wt, c, SetupTimeout); err != nil {
			return rep, fmt.Errorf("prepare %q: %v: %s", c, err, clip(tail(out, 5), 500))
		}
	}
	if err := r.setup(ctx, ru); err != nil {
		return rep, err
	}
	release, err := r.acquireVerifyLock(ctx, ru)
	if err != nil {
		return rep, err
	}
	defer release()

	writable := []string{ru.dir}
	if mobileDir != "" {
		writable = append(writable, mobileDir)
	}
	if reg.Suite == pipeline.SuiteOvernight {
		return r.runOvernight(ctx, ru, reg, rep, mobileDir, repos, writable, log)
	}
	cmdline := RegressionCommand(reg.Suite, ru.sandbox, mobileDir)
	exit, out, err := r.runFenced(ctx, ru, reg, cmdline, "e2e.log", "e2e "+reg.Suite, writable, cfg.Timeout(), log)
	if err != nil {
		return rep, err
	}
	runDir, raw, err := latestE2EReport(wt)
	if err != nil {
		return rep, fmt.Errorf("scripts/e2e exited %d with no report (%v); its log ends: %s", exit, err, clip(tail(out, 3), 400))
	}
	e2e, err := ParseE2EReport(raw)
	if err != nil {
		return rep, err
	}
	rep.E2ERun = e2e.Run
	rep.Error = e2e.SuiteErrors()
	tests, shots := e2e.RegressionTests(repos)
	rep.Tests = tests
	r.uploadShots(ctx, reg.ID, runDir, rep.Tests, shots, log)
	r.uploadReport(ctx, reg.ID, runDir, "", log)
	log.Info("regression ended", "exit", exit, "tests", len(tests), "suite_errors", rep.Error)
	return rep, nil
}

// runFenced runs cmdline in the run's checkout, in the verify steps' fence, its output in the run
// dir's logName, keeping the lease alive (note: what the board shows), for at most timeout. It
// returns the exit code and the log; an error is a stop (errVerifyStopped), the timeout, or a
// command that could not start.
func (r *Runner) runFenced(ctx context.Context, ru *run, reg Regression, cmdline, logName, note string, writable []string, timeout time.Duration, log *slog.Logger) (int, string, error) {
	logPath := filepath.Join(ru.dir, logName)
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return -1, "", err
	}
	cmd, err := r.fence(ru.wt, cmdline, append(r.agentEnv(), "LOOP_SANDBOX="+ru.sandbox, "LOOP_REPO="+ru.repo.Slug, "LOOP_BASE="+ru.repo.Base,
		"LOOP_REGRESSION="+strconv.FormatInt(reg.ID, 10)), writable)
	if err != nil {
		logf.Close()
		return -1, "", fmt.Errorf("%s cannot run fenced: %w", firstWord(cmdline), err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	ownGroup(cmd)
	log.Info("regression starting", "dir", ru.wt, "cmd", cmdline)
	if err := cmd.Start(); err != nil {
		logf.Close()
		return -1, "", fmt.Errorf("%s could not start: %w", firstWord(cmdline), err)
	}
	pid := cmd.Process.Pid
	exited := make(chan struct{})
	var state *os.ProcessState
	go func() {
		state, _ = cmd.Process.Wait()
		close(exited)
	}()
	w := r.keepAlive(ctx, ru, "regression: "+note, note, exited, timeout)
	if w != waitDone {
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
	if w == waitStopped {
		return -1, "", errVerifyStopped
	}
	exit := -1
	if state != nil {
		exit = state.ExitCode()
	}
	out, _ := os.ReadFile(logPath)
	if w == waitTimeout {
		return exit, string(out), fmt.Errorf("%s ran past the runner's %s limit (its log stays in the runner's run dir)", firstWord(cmdline), timeout)
	}
	return exit, string(out), nil
}

func firstWord(cmdline string) string {
	w, _, _ := strings.Cut(cmdline, " ")
	return w
}

// runDirShot resolves a screenshot's path in the run dir, symlinks included: the report's path is
// checked for "../" already, but a symlinked directory in .e2e/<run>/ could still lead out of it.
func runDirShot(runDir, rel string) (string, error) {
	root, err := filepath.EvalSymlinks(runDir)
	if err != nil {
		return "", err
	}
	path, err := filepath.EvalSymlinks(filepath.Join(runDir, rel))
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", fmt.Errorf("%s leads out of the run dir", rel)
	}
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", rel)
	}
	return path, nil
}

func (r *Runner) dropShot(tests []RegressionTest, name string) {
	for i := range tests {
		if tests[i].Screenshot == name {
			tests[i].Screenshot = ""
		}
		if tests[i].Recording == name {
			tests[i].Recording = ""
		}
	}
}

// uploadShots uploads the failures' screenshots and recordings (shots: upload name -> the file
// under runDir); one that cannot be is dropped from its test.
func (r *Runner) uploadShots(ctx context.Context, id int64, runDir string, tests []RegressionTest, shots map[string]string, log *slog.Logger) {
	names := slices.Sorted(maps.Keys(shots))
	for _, name := range names {
		rel := shots[name]
		path, err := runDirShot(runDir, rel)
		if err != nil {
			log.Warn("regression screenshot not uploaded", "file", rel, "err", err)
			r.dropShot(tests, name)
			continue
		}
		if err := r.Client.RegressionFile(ctx, id, path, name); err != nil {
			log.Warn("regression screenshot upload", "file", rel, "err", err)
			r.dropShot(tests, name)
		}
	}
}

// uploadReport uploads a report's report.md and report.json (prefix before their names).
func (r *Runner) uploadReport(ctx context.Context, id int64, runDir, prefix string, log *slog.Logger) {
	for _, f := range []string{"report.md", "report.json"} {
		if err := r.Client.RegressionFile(ctx, id, filepath.Join(runDir, f), prefix+f); err != nil {
			log.Warn("regression report upload", "file", f, "err", err)
		}
	}
}

// checkoutHeartbeat runs a checkout (a fetch can take a while) while keeping the lease alive.
func (r *Runner) checkoutHeartbeat(ctx context.Context, ru *run, note string, fn func() (string, string, func(), error)) (string, string, func(), error) {
	var dir, head string
	var done func()
	var err error
	finished := make(chan struct{})
	go func() {
		dir, head, done, err = fn()
		close(finished)
	}()
	if r.keepAlive(ctx, ru, "regression: "+note, note, finished, 0) != waitDone {
		go func() {
			<-finished
			if done != nil {
				done()
			}
		}()
		return "", "", nil, errVerifyStopped
	}
	return dir, head, done, err
}

// latestE2EReport is the newest .e2e/<run>/report.json in dir (a fresh checkout has one).
func latestE2EReport(dir string) (string, []byte, error) {
	matches, err := filepath.Glob(filepath.Join(dir, ".e2e", "*", "report.json"))
	if err != nil || len(matches) == 0 {
		return "", nil, errors.New("no .e2e/*/report.json")
	}
	sort.Strings(matches) // <run> is YYYYMMDD-HHMMSS-xxxxxx
	path := matches[len(matches)-1]
	raw, err := os.ReadFile(path)
	return filepath.Dir(path), raw, err
}
