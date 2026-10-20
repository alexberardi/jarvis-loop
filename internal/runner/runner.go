package runner

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/guard"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Runner claims triage, build and review items (config stages) and runs one agent per item; it
// works land items itself, with no agent (land*.go).
type Runner struct {
	// Cfg is the config the runner started with; Config is the current one (Reload replaces it,
	// reload.go), and each run keeps the one it started with (run.cfg).
	Cfg    *Config
	Client *Client
	Clock  Clock
	Git    Git
	Log    *slog.Logger
	// Env is the environment the agent starts with (default: the runner's own), minus loopd's
	// and Slack's secrets.
	Env []string
	// PRs opens or updates the item's PR after the runner pushed its branch (GH, or DryRunPRs).
	PRs PRs
	// Push pushes the item's branch from the mirror; nil means Git.PushBranch.
	Push func(ctx context.Context, mirror, branch string) error
	// ForcePush pushes a rewritten branch with a lease pinned to expect, the PR's recorded head;
	// nil means Git.ForcePushBranch.
	ForcePush func(ctx context.Context, mirror, branch, expect string) error
	// VerifyPR checks the PR on GitHub; nil skips the check (the dry run, tests).
	VerifyPR func(ctx context.Context, slug string, pr int) (PRInfo, error)
	// Land asks GitHub what the land gates need and merges (GH; nil: this runner cannot land).
	Land LandGitHub
	// Close closes dropped items' PRs (GH; nil: this runner leaves them to another).
	Close PRCloser
	// Revision is the loop commit this runner was built from (BuildRevision; "" when unknown): it
	// reports deployed for the qa items whose merge it contains (deploy.go).
	Revision string
	// HeartbeatEvery and KillGrace default to 60 s and 10 s.
	HeartbeatEvery time.Duration
	KillGrace      time.Duration

	fenceOnce sync.Once
	fenceErr  error

	hbMu sync.Mutex
	hb   *harnessBroker // the harness broker's sessions (harness.go)

	paused   atomic.Value // string: why the runner stopped claiming
	mirrorMu sync.Map     // mirror path -> *sync.Mutex
	watching atomic.Int64 // main-CI watches running
	bg       sync.WaitGroup

	cur      atomic.Pointer[Config] // the config after a Reload (nil: Cfg)
	ctlOnce  sync.Once
	reloaded chan struct{} // a Reload happened: Run re-sizes its lanes
	drainC   chan struct{} // closed by Drain
	draining atomic.Bool
	waits    waitSet // what Run waits on, for the drain's log
}

// PRInfo is what GitHub says about a PR.
type PRInfo struct {
	HeadRefName string `json:"headRefName"`
	HeadRefOid  string `json:"headRefOid"`
	State       string `json:"state"`
}

func (r *Runner) heartbeatEvery() time.Duration {
	if r.HeartbeatEvery > 0 {
		return r.HeartbeatEvery
	}
	return 60 * time.Second
}

func (r *Runner) killGrace() time.Duration {
	if r.KillGrace > 0 {
		return r.KillGrace
	}
	return 10 * time.Second
}

// Paused says why the runner stopped claiming ("" when it has not).
func (r *Runner) Paused() string {
	s, _ := r.paused.Load().(string)
	return s
}

// Run works items until ctx ends: agent work in up to Cfg.Slots parallel slots, and land, which
// runs no agent, in up to Cfg.LandSlots lanes of their own next to them (loopd's land limit caps
// the total), so an approved item never waits for an agent slot, nor for another item's CI
// (docs/decisions/2026-10-16-land-lane.md, docs/decisions/2026-10-18-land-never-waits.md). A
// Reload re-sizes the lanes (slots, land slots, stages); after Drain, Run claims nothing and
// returns once its runs end. Only the end of ctx ends the runs themselves.
func (r *Runner) Run(ctx context.Context) error {
	r.ctl()
	claimCtx, stopClaims := context.WithCancel(ctx)
	defer stopClaims()
	go func() {
		select {
		case <-r.drainC:
			stopClaims()
		case <-claimCtx.Done():
		}
	}()
	var wg sync.WaitGroup
	// loop runs one background loop, named in the drain's log while Run waits for it.
	loop := func(name string, f func()) {
		wg.Add(1)
		done := r.waits.add(name, r.Clock.Now())
		go func() {
			defer wg.Done()
			defer done()
			f()
		}()
	}
	if r.Close != nil {
		loop("close-dropped loop", func() { r.closeDroppedLoop(claimCtx) })
	}
	if r.Revision != "" {
		loop("deployed-report loop", func() { r.reportDeployedLoop(claimCtx) })
	}
	if r.Land != nil {
		loop("held-watch loop", func() { r.heldWatchLoop(claimCtx) })
		// The runner that works on main checks the qa items' proofs there. A drain stops the
		// loop (claimCtx); a check already running finishes (ctx), as a live run does (#284).
		loop("proof loop", func() { r.checkProofsLoop(claimCtx, ctx) })
	}
	var mu sync.Mutex
	running := map[string]bool{}
	// lane claims in want()'s stages until it says nil (a Reload took the lane away) or claiming
	// ends; claims stop with claimCtx, the runs only with ctx.
	lane := func(key string, want func() []string) {
		mu.Lock()
		defer mu.Unlock()
		if running[key] {
			return
		}
		running[key] = true
		wg.Add(1)
		// gone ends the lane when a reload took it away; checked under mu, so a reload that gives
		// it back meanwhile either sees it running or starts it again.
		gone := func() bool {
			mu.Lock()
			defer mu.Unlock()
			if want() != nil {
				return false
			}
			delete(running, key)
			return true
		}
		go func() {
			defer wg.Done()
			for claimCtx.Err() == nil {
				stages := want()
				if stages == nil {
					if gone() {
						return
					}
					continue
				}
				claimed, err := r.claimAndRunIn(claimCtx, ctx, stages)
				if err != nil && claimCtx.Err() == nil {
					r.Log.Warn("claim", "err", err)
				}
				if claimed && err == nil {
					continue
				}
				t := r.Clock.NewTimer(time.Duration(r.Config().PollSeconds) * time.Second)
				select {
				case <-claimCtx.Done():
				case <-t.C():
				}
				t.Stop()
			}
			mu.Lock()
			delete(running, key)
			mu.Unlock()
		}()
	}
	for claimCtx.Err() == nil {
		agent, land := r.Lanes()
		for i := 0; agent != nil && i < r.Config().Slots; i++ {
			lane(fmt.Sprintf("agent-%d", i), func() []string {
				if agent, _ := r.Lanes(); agent != nil && i < r.Config().Slots {
					return agent
				}
				return nil
			})
		}
		for i := 0; land != nil && i < r.Config().LandSlots; i++ {
			lane(fmt.Sprintf("land-%d", i), func() []string {
				if _, land := r.Lanes(); land != nil && i < r.Config().LandSlots {
					return land
				}
				return nil
			})
		}
		select {
		case <-claimCtx.Done():
		case <-r.reloaded:
		}
	}
	if r.Draining() {
		r.Log.Info("draining: waiting for the live runs to end", "on", strings.Join(r.waits.list(r.Clock.Now()), "; "))
		stop := make(chan struct{})
		defer close(stop)
		go r.logDrainWaits(stop)
	}
	wg.Wait()
	r.bg.Wait()
	return nil
}

// Lanes splits the stages this runner claims into the agent lane's (nil when there are none: an
// empty stages list means any stage to loopd) and the land lane's (nil without land).
func (r *Runner) Lanes() (agent, land []string) {
	for _, st := range r.Config().Stages {
		if st == StageLand {
			land = []string{StageLand}
		} else {
			agent = append(agent, st)
		}
	}
	return agent, land
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ClaimAndRun claims one item in the stages this runner runs and works it to the end. It reports
// whether it claimed one.
func (r *Runner) ClaimAndRun(ctx context.Context) (bool, error) {
	return r.ClaimAndRunIn(ctx, r.Config().Stages)
}

// ClaimAndRunIn is ClaimAndRun in some of the stages (a lane's). A paused runner claims only land,
// which runs no agent; a draining one claims nothing.
func (r *Runner) ClaimAndRunIn(ctx context.Context, stages []string) (bool, error) {
	return r.claimAndRunIn(ctx, ctx, stages)
}

// claimAndRunIn claims with claimCtx and works the claim with ctx.
func (r *Runner) claimAndRunIn(claimCtx, ctx context.Context, stages []string) (bool, error) {
	if r.Draining() {
		return false, nil
	}
	if why := r.Paused(); why != "" {
		if !slices.Contains(stages, StageLand) {
			return false, nil
		}
		stages = []string{StageLand}
	}
	cfg := r.Config()
	agent := cfg.Name + "-" + randHex(4)
	cl, err := r.Client.ClaimFor(claimCtx, agent, cfg, stages)
	if err != nil || cl == nil {
		return false, err
	}
	defer r.waits.add(fmt.Sprintf("#%d %s", cl.Item.ID, cl.Stage), r.Clock.Now())()
	if cl.Stage == StageLand {
		r.land(ctx, cl, agent) // a script, not an agent
		return true, nil
	}
	if cl.Kind == pipeline.WorkPlan {
		r.planTurn(ctx, cl, agent)
		return true, nil
	}
	if cl.Kind == pipeline.WorkOrchestrate {
		r.orchestrateTurn(ctx, cl, agent)
		return true, nil
	}
	if cl.Kind == pipeline.WorkRegression {
		r.regression(ctx, cl, agent)
		return true, nil
	}
	r.work(ctx, cl, agent)
	return true, nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug is a branch-safe form of a title.
func Slug(title string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		s = "item"
	}
	return s
}

// run is one item's run: where things are.
type run struct {
	// cfg is the runner's config when the run started: a Reload applies to the next claim.
	cfg  *Config
	item Item
	// trustUsed is the commit verify ran on the approver's trust OK (reported with the outcome).
	trustUsed string
	// carry is a review's delta from the head the user approved, when that is not the head
	// reviewed (carry.go).
	carry *ApprovalDelta
	// trustCarried is the head verify carried the OK to (its trusted files' diff is the one the
	// approver OK'd, digest trustDiffSHA), reported with the outcome for loopd to check.
	trustCarried string
	trustDiffSHA string
	stage        string
	lease        string
	agent        string
	repoKey      string // "" for a triage run of an item with no repo: every configured repo is checked out
	repo         RepoConfig
	branch       string
	// wt is the agent's working directory: the item's worktree (build), the PR's head (review),
	// or a plain directory with the repos checked out under repos/<key>/ (triage).
	wt string
	// repoDir is where the item's repo is checked out (wt, or wt/repos/<key> in triage; "" when
	// triage has several).
	repoDir   string
	checkouts []Checkout
	scratch   string
	dir       string // the run's own dir: settings, guard config, logs (never writable by the agent)
	mirror    string
	sandbox   string
	// verifySandbox is the runner's verify's own sandbox name, apart from the agent's (#327).
	verifySandbox string
	settings      string
	started       time.Time

	// A plan turn's (plan.go): the plan and the turn (stage is the turn's persona), the session
	// it resumes ("" for a new one) and its time cap (plan_turn_minutes).
	plan   *pipeline.Plan
	turn   *pipeline.PlanTurn
	resume string
	maxRun time.Duration
	// An orchestrate turn's (orchestrate.go); stage is StageOrchestrate.
	orch *OrchTurn

	// builder is who runs the agent: pipeline.BuilderQwen (OpenCode, a build run of a qwen item)
	// or pipeline.BuilderClaude (every other run); pickBuilder sets it. qwenRequests are the
	// requests a qwen run's proxy forwarded.
	builder      string
	qwenRequests int64

	// A Claude build's session (buildsession.go): the record of the item's interrupted session
	// found at the claim, the one this run resumed (nil for a new session) and the branch's head
	// the run started from.
	prevSession *buildSession
	resumedFrom *buildSession
	startHead   string
	// preMerge is the evidence line of the runner's merge of the base before the build ("": none).
	preMerge string

	reported int // refusals already sent
	refusals int
	// rejected is why the runner refused the agent's result, before publishing anything; work
	// runs the stage once more (retried) with it in the brief. spent is the time the earlier
	// attempt used, which the retry's time cap leaves out.
	rejected string
	retried  bool
	spent    time.Duration
	// noResult: the agent exited cleanly with no result.json; work resumes its session once
	// (resumedForResult), which counts as the stage's one retry.
	noResult, resumedForResult bool
	// first is the refused attempt's numbers, which the run's metrics and footer add to the
	// retry's.
	first streamStats
	// done closes when the run must stop (the runner shutting down): its reports go on, but a
	// verify step is ended.
	done <-chan struct{}
}

func (r *Runner) lockMirror(path string) func() {
	m, _ := r.mirrorMu.LoadOrStore(path, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// newRun works out where an item's run happens.
func (r *Runner) newRun(cl *Claim, agent string) (*run, error) {
	it := cl.Item
	stage := cl.Stage
	if stage == "" {
		stage = it.Stage
	}
	ru := &run{item: it, stage: stage, lease: cl.Lease.ID, agent: agent, started: r.Clock.Now(), cfg: r.Config()}
	if !slices.Contains(AgentStages, stage) {
		return ru, fmt.Errorf("runner %s does not run %q agents", ru.cfg.Name, stage)
	}
	key, repo, ok := ru.cfg.Repo(it.Repo)
	switch {
	case ok:
		ru.repoKey, ru.repo = key, repo
	case stage == StageTriage && it.Repo == "":
		// Triage decides the repo: every configured repo is checked out.
	default:
		return ru, fmt.Errorf("runner %s has no repo %q configured", ru.cfg.Name, it.Repo)
	}
	ru.branch = it.Branch
	if !strings.HasPrefix(ru.branch, "loop/") {
		ru.branch = fmt.Sprintf("loop/%d-%s", it.ID, Slug(it.Title))
	}
	ru.wt = filepath.Join(ru.cfg.WorkRoot, fmt.Sprintf("%d-%s", it.ID, stage))
	// Inside the worktree (ignored by git), so the agent needs no extra directory: an --add-dir
	// would differ per item.
	ru.scratch = filepath.Join(ru.wt, ".loop", "scratch")
	// Two runs of a stage within a second (a routed item triaged again, a build after a failed
	// verify) keep separate logs.
	ru.dir = uniqueRunDir(filepath.Join(ru.cfg.WorkRoot, "runs", fmt.Sprintf("%d-%s-%s", it.ID, stage, ru.started.Format("20060102T150405Z"))))
	// One settings file per repo and stage, the same bytes for every item (prompt caching).
	ru.settings = filepath.Join(ru.cfg.WorkRoot, "settings", stage+".json")
	if ru.repoKey != "" {
		ru.mirror = r.mirrorPath(ru.repoKey)
		ru.settings = filepath.Join(ru.cfg.WorkRoot, "settings", Slug(ru.repoKey)+"-"+stage+".json")
	}
	ru.sandbox = fmt.Sprintf("loop-%d", it.ID)
	ru.verifySandbox = ru.sandbox + "-verify"
	return ru, nil
}

func (r *Runner) mirrorPath(key string) string {
	return filepath.Join(r.Config().WorkRoot, "mirrors", Slug(key)+".git")
}

func (r *Runner) work(ctx context.Context, cl *Claim, agent string) {
	log := r.Log.With("item", cl.Item.ID, "agent", agent)
	ru, err := r.newRun(cl, agent)
	if err != nil {
		r.advance(ctx, ru, "failed", map[string]string{"error": err.Error()}, nil)
		return
	}
	cleanupCtx := context.WithoutCancel(ctx)
	ru.done = ctx.Done()
	defer r.cleanup(cleanupCtx, ru, log)
	r.pickBuilder(ctx, ru)
	if ru.claudeBuild() {
		ru.prevSession = r.loadBuildSession(ru)
	}

	brief, err := r.prepare(ctx, ru)
	if err == nil && ru.resumedFrom != nil {
		// The session has the brief already: its prompt is a note on the interruption alone.
		err = os.WriteFile(filepath.Join(ru.dir, "brief.md"), []byte(ResumeNote(ru.resumedFrom)), 0o644)
	}
	if err != nil {
		log.Error("prepare", "err", err)
		r.advance(ctx, ru, "failed", map[string]string{"error": "the runner could not prepare the run: " + clip(err.Error(), 1500)}, nil)
		return
	}
	if raw := r.restoreTrustedResult(ctx, ru, log); raw != nil {
		// The user OK'd the branch's head after verify refused it: verify runs that commit, with
		// the result its build left, and the agent does not run (a new commit would need a new OK).
		log.Info("verify runs the commit the user OK'd, without the agent", "head", ru.item.Trust.HeadSHA)
		r.reportBuild(cleanupCtx, ru, outcome{}, raw)
		if ru.rejected == "" {
			return
		}
		log.Warn("the kept result was refused; the agent runs", "why", ru.rejected)
		r.dropTrustedResult(ru)
		ru.rejected = ""
		os.Remove(filepath.Join(ru.wt, ".loop", "result.json"))
	}
	log.Info("agent starting", "worktree", ru.wt, "branch", ru.branch, "mode", r.cfgOf(ru).Mode, "builder", ru.builder, "resume", ru.resume)
	out := r.runAgentHarness(ctx, ru, brief)
	log.Info("agent ended", "reason", out.reason, "exit", out.exitCode, "detail", out.detail)
	if ru.resumedFrom != nil && r.sessionGone(ru, out) {
		// claude no longer has the session (expired, its store cleared): a new one, with the whole
		// brief, in the same claim.
		log.Warn("the build's session to resume is gone; starting a new one", "session", ru.resume)
		r.dropBuildSession(ru)
		ru.resume, ru.resumedFrom = "", nil
		keepFirstAttempt(ru.dir, log)
		if err := r.writeBrief(ru, brief); err == nil {
			err = r.writeArgv(ru)
		}
		if err != nil {
			r.fail(cleanupCtx, ru, out, "the build's session could not be resumed, and a new one could not start: "+err.Error())
			return
		}
		out = r.runAgentHarness(ctx, ru, brief)
		log.Info("agent ended", "reason", out.reason, "exit", out.exitCode, "detail", out.detail)
	}
	// Reporting and saving work must finish even when the runner is shutting down.
	r.report(cleanupCtx, ru, out, log)
	if ru.noResult {
		// No result.json after a clean exit (#208): resume the same session once, with a note, in
		// place of the refused-result retry.
		ru.noResult, ru.retried, ru.spent = false, true, r.Clock.Now().Sub(ru.started)
		msg := fmt.Sprintf("the agent exited (code %d) without writing .loop/result.json", out.exitCode)
		if ctx.Err() != nil || r.maxRun(ctx, ru)-ru.spent < MinRetryTime {
			r.fail(cleanupCtx, ru, out, msg)
			return
		}
		log.Warn("no result; resuming the session once", "session", out.stats.sessionID)
		ru.first, ru.resume, ru.resumedForResult = out.stats, out.stats.sessionID, true
		keepFirstAttempt(ru.dir, log)
		err := os.WriteFile(filepath.Join(ru.dir, "brief.md"), []byte(NoResultNote), 0o644)
		if err == nil {
			err = r.writeArgv(ru)
		}
		if err != nil {
			r.fail(cleanupCtx, ru, out, msg+"; resuming its session failed: "+err.Error())
			return
		}
		out = r.runAgentHarness(ctx, ru, brief)
		log.Info("agent ended (resumed for its result)", "reason", out.reason, "exit", out.exitCode, "detail", out.detail)
		r.report(cleanupCtx, ru, out, log)
		return
	}
	why := ru.rejected
	if why == "" {
		return
	}
	// The result was refused before anything was published: run the stage once more, with the
	// reason in the brief, unless the runner is stopping or the run's time is nearly used up.
	ru.rejected, ru.retried, ru.spent = "", true, r.Clock.Now().Sub(ru.started)
	if ctx.Err() != nil || r.maxRun(ctx, ru)-ru.spent < MinRetryTime {
		r.fail(cleanupCtx, ru, out, rejectedMsg(ru.stage, why, false))
		return
	}
	log.Warn("result refused; running the stage once more", "why", why)
	ru.first = out.stats
	if ru.builder == pipeline.BuilderQwen && out.stats.sessionID != "" {
		ru.resume = out.stats.sessionID // the retry continues the qwen session
	} else if ru.resume != "" && ru.claudeBuild() {
		ru.resume = "" // a claude retry starts a new session, with the whole brief
		if err := r.writeArgv(ru); err != nil {
			r.fail(cleanupCtx, ru, out, rejectedMsg(ru.stage, why, false)+"; the retry could not start: "+err.Error())
			return
		}
	}
	keepFirstAttempt(ru.dir, log)
	brief = RetryBrief(brief, why)
	if err := r.writeBrief(ru, brief); err != nil {
		r.fail(cleanupCtx, ru, out, rejectedMsg(ru.stage, why, false)+"; the retry could not start: "+err.Error())
		return
	}
	os.Remove(filepath.Join(ru.wt, ".loop", "result.json"))
	out = r.runAgentHarness(ctx, ru, brief)
	log.Info("agent ended (retry)", "reason", out.reason, "exit", out.exitCode, "detail", out.detail)
	r.report(cleanupCtx, ru, out, log)
}

// keepFirstAttempt renames the refused attempt's transcript, stderr and brief in the run dir to
// stream-1.jsonl, stderr-1.log and brief-1.md, so the retry's files do not overwrite them.
func keepFirstAttempt(dir string, log *slog.Logger) {
	for _, f := range [][2]string{{"stream.jsonl", "stream-1.jsonl"}, {"stderr.log", "stderr-1.log"}, {"brief.md", "brief-1.md"}} {
		if err := os.Rename(filepath.Join(dir, f[0]), filepath.Join(dir, f[1])); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Warn("keeping the first attempt's files", "file", f[0], "err", err)
		}
	}
}

// MinRetryTime: a refused result is retried only when at least this much of the run's time cap
// is left.
const MinRetryTime = 5 * time.Minute

// rejectedMsg is what the user reads when the runner could not accept a stage's result: a runner
// error, not a question.
func rejectedMsg(stage, why string, retried bool) string {
	msg := fmt.Sprintf("the %s's result couldn't be accepted: %s", stage, why)
	if retried {
		msg += " (again, after one retry)"
	}
	return msg
}

// reject reports a result the runner cannot accept. The first time it only records why, and work
// runs the stage again; so call it only before anything was pushed, posted or uploaded.
func (r *Runner) reject(ctx context.Context, ru *run, out outcome, why string) {
	if !ru.retried {
		ru.rejected = why
		return
	}
	r.fail(ctx, ru, out, rejectedMsg(ru.stage, why, true))
}

// evidence lists the run's evidence files; what it skipped (a tool's state, dotfiles) is logged,
// and refused only when the result names it.
func (r *Runner) evidence(ru *run, v Result) ([]EvidenceFile, error) {
	files, skipped, err := EvidenceFiles(filepath.Join(ru.wt, ".loop"))
	if err != nil {
		return nil, err
	}
	if len(skipped) > 0 {
		r.Log.Warn("evidence: skipped directories and dotfiles", "item", ru.item.ID, "stage", ru.stage, "skipped", strings.Join(skipped, ", "))
	}
	if err := checkNamedNotSkipped(v, skipped); err != nil {
		return nil, err
	}
	return files, nil
}

// writeBrief writes the brief the agent reads (the run dir's copy is its stdin).
func (r *Runner) writeBrief(ru *run, brief string) error {
	for _, p := range []string{filepath.Join(ru.wt, ".loop", "brief.md"), filepath.Join(ru.dir, "brief.md")} {
		if err := os.WriteFile(p, []byte(brief), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// prepare makes the working directory (per stage), the context files, the brief, the guard
// config and the settings.
func (r *Runner) prepare(ctx context.Context, ru *run) (string, error) {
	os.RemoveAll(ru.dir)
	if err := os.MkdirAll(ru.dir, 0o700); err != nil {
		return "", err
	}
	info := RunInfo{Slug: ru.repo.Slug, Base: ru.repo.Base, Branch: ru.branch, Worktree: ru.wt, Scratch: ru.scratch, Sandbox: ru.sandbox,
		RepoGroup: ru.repo.Group}
	if ru.hasHarness() {
		info.Harness = ru.repo.Harness
		info.HarnessBin = filepath.Join(r.cfgOf(ru).harnessBinDir(), "loop-harness")
	}
	if ru.stage == StageBuild {
		for _, v := range ru.repo.Verify {
			info.Verify = append(info.Verify, v.Name)
		}
	}
	var err error
	switch ru.stage {
	case StageTriage:
		err = r.prepareTriage(ctx, ru, &info)
	case StageReview:
		err = r.prepareReview(ctx, ru, &info)
	default:
		err = r.prepareBuild(ctx, ru, &info)
	}
	if err != nil {
		return "", err
	}
	for _, d := range []string{filepath.Join(ru.wt, ".loop", "evidence"), ru.scratch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}
	brief := Brief(ru.stage, ru.item, info)
	if err := r.writeBrief(ru, brief); err != nil {
		return "", err
	}
	if ru.repoDir != "" {
		for _, c := range ru.repo.Prepare {
			if out, err := r.shell(ctx, ru.repoDir, c, 20*time.Minute); err != nil {
				return "", fmt.Errorf("prepare %q: %v: %s", c, err, clip(out, 500))
			}
		}
		if err := r.setup(ctx, ru); err != nil {
			return "", err
		}
	}
	home := Home()
	deny := ru.repo.DenyWrite
	if ru.stage == StageTriage {
		// The checkouts sit under repos/<key>/: their git files and the repos' own deny paths.
		deny = nil
		for _, c := range ru.checkouts {
			deny = append(deny, c.Dir+"/.git")
			for _, d := range r.cfgOf(ru).Repos[c.Key].DenyWrite {
				deny = append(deny, c.Dir+"/"+d)
			}
		}
	}
	gc := guard.Config{
		Item: ru.item.ID, Worktree: ru.wt, Scratch: ru.scratch, SandboxDir: filepath.Join(ru.wt, ".sandbox", ru.sandbox),
		Branch: ru.branch, Remote: "origin", Home: home,
		Protected: r.cfgOf(ru).Protected(), Secrets: r.cfgOf(ru).Secrets(), Words: []string{r.cfgOf(ru).UserEmulator},
		LoopdAddrs: r.cfgOf(ru).LoopdAddrs(), DenyWrite: deny, ReadOnly: ru.stage != StageBuild,
		Report: filepath.Join(ru.dir, "refusals.jsonl"), Armed: filepath.Join(ru.dir, "armed"),
	}
	b, _ := json.MarshalIndent(gc, "", "  ")
	if err := os.WriteFile(filepath.Join(ru.dir, "guard.json"), b, 0o600); err != nil {
		return "", err
	}
	// The same bytes for every item of the repo and stage (see
	// docs/decisions/2026-10-15-prompt-caching.md); written atomically, as parallel slots share
	// the file.
	in := SettingsInput{
		Mode: r.cfgOf(ru).Mode, Guard: r.cfgOf(ru).Guard, Home: home, Protected: r.cfgOf(ru).Protected(), Secrets: r.cfgOf(ru).Secrets(),
		LoopdAddrs: r.cfgOf(ru).LoopdAddrs(), AllowedDomains: r.cfgOf(ru).AllowedDomains, ExcludedCommands: ru.repo.ExcludedCommands,
		ReadOnly: ru.stage != StageBuild,
	}
	if ru.hasHarness() {
		in.Harness = true
	}
	settings, err := GenerateSettings(in)
	if err != nil {
		return "", err
	}
	if err := writeAtomic(ru.settings, settings); err != nil {
		return "", err
	}
	if _, err := os.Stat(r.cfgOf(ru).Guard); err != nil {
		return "", fmt.Errorf("the guard binary %s: %w", r.cfgOf(ru).Guard, err)
	}
	if err := r.writeArgv(ru); err != nil {
		return "", err
	}
	return brief, nil
}

// writeArgv keeps the run's command line with its log (the harness checks it is the same for
// every run of a stage, but for a resumed session).
func (r *Runner) writeArgv(ru *run) error {
	argv, _ := json.Marshal(r.agentCommand(ru).Args[1:])
	return os.WriteFile(filepath.Join(ru.dir, "argv.json"), argv, 0o600)
}

// recorded is what the loop recorded for the item's branch: the PR's head, and the heads the user
// OK'd since (a Trust that still allows its commit, an approval).
func recorded(it Item) Recorded {
	rec := Recorded{Prev: it.HeadSHA}
	if it.Trust != nil && it.Trust.allows(it.Trust.HeadSHA) {
		rec.Heads = append(rec.Heads, it.Trust.HeadSHA)
	}
	if it.Approval != nil && it.Approval.HeadSHA != "" {
		rec.Heads = append(rec.Heads, it.Approval.HeadSHA)
	}
	return rec
}

// prepareBuild makes the item's worktree on its branch (continuing an earlier run's commits),
// with triage's notes and, after a bounce, the review's in .loop/review/ (and, after a held item
// came back, why).
func (r *Runner) prepareBuild(ctx context.Context, ru *run, info *RunInfo) error {
	log := r.Log.With("item", ru.item.ID, "agent", ru.agent)
	if s := ru.prevSession; s != nil && s.Reason == "" {
		// The run never ended as far as the runner knows (it crashed, or was killed): save what it
		// left in the old worktree before that is replaced.
		r.saveCrashedWork(ctx, ru, s, log)
	}
	unlock := r.lockMirror(ru.mirror)
	err := r.Git.EnsureMirror(ctx, ru.mirror, ru.repo)
	ahead := 0
	stale := ""
	if err == nil {
		stale = r.Git.revParse(ctx, ru.mirror, "refs/heads/"+ru.branch)
		ahead, err = r.Git.AddWorktree(ctx, ru.mirror, ru.wt, ru.branch, ru.repo.Base, recorded(ru.item))
	}
	if err == nil && stale != "" && r.Git.hasRef(ctx, ru.mirror, BackupRef(ru.branch, stale)) &&
		r.Git.revParse(ctx, ru.mirror, "refs/heads/"+ru.branch) != stale {
		log.Info("reset the mirror's branch to the PR's recorded head", "branch", ru.branch, "old", stale,
			"new", ru.item.HeadSHA, "backup", BackupRef(ru.branch, stale))
	}
	unlock()
	if err != nil {
		return err
	}
	ru.repoDir = ru.wt
	info.Resumed = ahead
	// The sandbox keeps .claude/ read-only for the agent, so a merge of a base that changed it
	// is the runner's (premerge.go); before startHead, so a resume and the result see the merge.
	if pm, err := r.Git.PreMerge(ctx, ru.wt, ru.repo.Base, ru.branch); err != nil {
		log.Warn("could not merge the base before the build; the agent merges it", "err", err)
	} else if pm.SHA != "" {
		info.PreMerge, ru.preMerge = &pm, pm.Evidence(ru.repo.Base)
		log.Info(ru.preMerge, "branch", ru.branch)
	}
	if ru.claudeBuild() {
		ru.startHead, _ = r.Git.Head(ctx, ru.wt)
	}
	if s := ru.prevSession; s != nil {
		if why := s.resumable(ru.stage, ru.startHead); why != "" {
			log.Info("not resuming the build's session", "session", s.Session, "why", why)
			r.dropBuildSession(ru)
		} else {
			ru.resume, ru.resumedFrom = s.Session, s
		}
	}
	info.Triage, info.TriageErr = r.fetchEvidence(ctx, ru, filepath.Join(".loop", "triage"))
	for _, f := range info.Triage { // the newest CI failure log (land's, or triage's for a red main)
		if strings.HasSuffix(f.Name, "-"+CIFailureLog) {
			info.CILog = &TriageFile{Name: ".loop/triage/" + f.Name, Label: f.Label}
		}
	}
	if ru.item.BounceCount > 0 {
		r.reviewContext(ctx, ru, info, filepath.Join(".loop", "review", "notes.md"), filepath.Join(".loop", "review", "pr-comments.md"))
	}
	r.heldNotes(ctx, ru, info, filepath.Join(".loop", "review", "held.md"))
	r.answersContext(ctx, ru, info, filepath.Join(".loop", "context", "answers.md"))
	r.planContext(ctx, ru, info)
	return nil
}

// SetupTimeout is the time limit of each of a repo's setup commands.
const SetupTimeout = 20 * time.Minute

// setup runs the repo's setup commands (npm ci, a venv) in the checkout, as the runner, outside
// the agent's sandbox, before the agent starts; their output goes to setup.log in the run dir.
// A failure fails the run (the user sees the log's tail).
func (r *Runner) setup(ctx context.Context, ru *run) error {
	return r.runSetup(ctx, ru.repo.Setup, ru.repoDir, ru.dir)
}

// runSetup runs setup commands in dir, logged in logDir's setup.log.
func (r *Runner) runSetup(ctx context.Context, cmds []string, dir, logDir string) error {
	if len(cmds) == 0 {
		return nil
	}
	logf, err := os.OpenFile(filepath.Join(logDir, "setup.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	for _, c := range cmds {
		fmt.Fprintf(logf, "$ %s\n", c)
		out, err := r.shell(ctx, dir, c, SetupTimeout)
		logf.WriteString(out)
		if err != nil {
			fmt.Fprintf(logf, "[failed: %v]\n", err)
			return fmt.Errorf("setup %q: %v: %s", c, err, clip(tail(out, 20), 800))
		}
	}
	return nil
}

// tail is the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + "." + randHex(4)
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// shell runs a prepare, setup or cleanup command under the fence (fence.go): it runs files from
// a tree the agent may have changed.
func (r *Runner) shell(ctx context.Context, dir, command string, limit time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	fc, err := r.fence(dir, command, r.agentEnv(), nil)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, fc.Path, fc.Args[1:]...)
	cmd.Dir, cmd.Env = fc.Dir, fc.Env
	ownGroup(cmd)
	cmd.Cancel = func() error { return signalGroup(cmd.Process.Pid, syscall.SIGKILL) }
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// agentEnv is the environment for the agent and the runner's helper commands: no loopd or Slack
// secret.
func (r *Runner) agentEnv() []string { return scrubEnv(r.Env) }

// scrubEnv is src (nil: the runner's own environment) without loopd's and Slack's secrets.
func scrubEnv(src []string) []string {
	if src == nil {
		src = os.Environ()
	}
	var out []string
	for _, kv := range src {
		k, _, _ := strings.Cut(kv, "=")
		if k == "LOOP_TOKEN" || k == "LOOP_URL" || strings.HasPrefix(k, "SLACK_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Why a run ended.
const (
	endExited       = "exited"
	endStopped      = "stopped"
	endTimeCap      = "time_cap"
	endShutdown     = "shutdown"
	endModeMismatch = "mode_mismatch"
	endNotArmed     = "guard_not_armed"
	endStartFailed  = "start_failed"
	endGitTampered  = "git_tampered"
)

type outcome struct {
	reason   string
	detail   string
	exitCode int
	stats    streamStats
	// stderr is the end of the agent's stderr.log, on one line: why an agent that printed
	// nothing died.
	stderr string
}

type streamStats struct {
	initMode    string
	sessionID   string
	lastText    string
	toolResults int
	limit       *LimitHit
	result      *resultEvent
	// model is the session's (the init event's); rateLimit the CLI's last rate-limit reading.
	model     string
	rateLimit *rateLimit
	act       liveActivity
	// A qwen run's tokens (OpenCode's step_finish events); claude's are in result.
	qwenInput, qwenOutput int64
}

type resultEvent struct {
	Subtype   string   `json:"subtype"`
	IsError   bool     `json:"is_error"`
	NumTurns  int      `json:"num_turns"`
	Duration  int64    `json:"duration_ms"`
	CostUSD   float64  `json:"total_cost_usd"`
	Result    string   `json:"result"`
	Errors    []string `json:"errors"`
	Denials   []any    `json:"permission_denials"`
	ErrStatus *int     `json:"api_error_status"`
	Usage     struct {
		Input         int64 `json:"input_tokens"`
		Output        int64 `json:"output_tokens"`
		CacheCreation int64 `json:"cache_creation_input_tokens"`
		CacheRead     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// CacheHitRatio is cache reads over all input tokens (reads, writes and uncached input).
func CacheHitRatio(input, creation, read int64) float64 {
	total := input + creation + read
	if total == 0 {
		return 0
	}
	return float64(read) / float64(total)
}

// handle reads one stream-json line; it returns a reason to end the run early, or "".
func (r *Runner) handle(ru *run, st *streamStats, line []byte) (string, string) {
	if hit, ok := MatchUsageLimit(line); ok {
		if st.limit == nil {
			st.limit = &hit
		} else {
			st.limit.merge(hit)
		}
	}
	var ev struct {
		Type           string     `json:"type"`
		Subtype        string     `json:"subtype"`
		PermissionMode string     `json:"permissionMode"`
		SessionID      string     `json:"session_id"`
		Model          string     `json:"model"`
		RateLimit      *rateLimit `json:"rate_limit_info"`
		Message        *struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return "", ""
	}
	switch {
	case ev.Type == "system" && ev.Subtype == "init":
		st.initMode = ev.PermissionMode
		st.sessionID = ev.SessionID
		st.model = ev.Model
		r.recordBuildSession(ru, ev.SessionID)
		if want := PermissionMode(r.cfgOf(ru).Mode); ev.PermissionMode != want {
			return endModeMismatch, fmt.Sprintf("the session started in %q, not %q", ev.PermissionMode, want)
		}
	case ev.Type == "assistant" && ev.Message != nil:
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			toolUse
		}
		if json.Unmarshal(ev.Message.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					st.lastText = clip(strings.TrimSpace(b.Text), 1500)
				}
				if b.Type == "tool_use" {
					st.act.observe(ru.wt, b.toolUse)
				}
			}
		}
	case ev.Type == "user" && ev.Message != nil:
		var blocks []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(ev.Message.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type != "tool_result" {
					continue
				}
				st.toolResults++
				if st.toolResults == 1 {
					if _, err := os.Stat(filepath.Join(ru.dir, "armed")); err != nil {
						return endNotArmed, "a tool ran but loop-guard never did: the generated settings were not applied"
					}
				}
			}
		}
	case ev.Type == "rate_limit_event" && ev.RateLimit != nil:
		st.rateLimit = ev.RateLimit
	case ev.Type == "result":
		var res resultEvent
		if json.Unmarshal(line, &res) == nil {
			st.result = &res
			if res.Result != "" {
				st.lastText = clip(strings.TrimSpace(res.Result), 1500)
			}
		}
	}
	return "", ""
}

// agentCommand is the claude command for a run: the same argv for every item of a stage (a plan
// turn adds the session it resumes); the worktree as cwd; the per-run guard config in the
// environment.
func (r *Runner) agentCommand(ru *run) *exec.Cmd {
	cmd := exec.Command(r.cfgOf(ru).Claude, ClaudeArgs(AgentArgs{Mode: r.cfgOf(ru).Mode, Settings: ru.settings, Model: r.cfgOf(ru).Model, Resume: ru.resume})...)
	cmd.Dir = ru.wt
	env := r.agentEnv()
	if ru.hasHarness() {
		env = prependPath(env, r.cfgOf(ru).harnessBinDir())
	}
	cmd.Env = append(env,
		guard.ConfigEnv+"="+filepath.Join(ru.dir, "guard.json"),
		// Loop agents keep no auto memory (a per-worktree memory dir would be new every run) and
		// fetch no claude.ai connectors (no MCP tools at all).
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
		"ENABLE_CLAUDEAI_MCP_SERVERS=false",
	)
	return cmd
}

// maxRun is the run's time cap: loopd's max_run_minutes (45 by default), or a plan turn's own.
func (r *Runner) maxRun(ctx context.Context, ru *run) time.Duration {
	maxRun := 45 * time.Minute
	if ctl, err := r.Client.Control(ctx); err == nil && ctl.MaxRunMinutes > 0 {
		maxRun = time.Duration(ctl.MaxRunMinutes) * time.Minute
	}
	if ru.maxRun > 0 && ru.maxRun < maxRun {
		maxRun = ru.maxRun // a plan turn's own cap
	}
	return maxRun
}

// hasHarness: the run's agent gets a harness session (harness.go).
func (ru *run) hasHarness() bool {
	return ru.repo.Harness != nil && (ru.stage == StageBuild || ru.stage == StageReview)
}

// prependPath puts dir first on env's PATH.
func prependPath(env []string, dir string) []string {
	out := append([]string(nil), env...)
	for i, kv := range out {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			out[i] = "PATH=" + dir + string(os.PathListSeparator) + v
			return out
		}
	}
	return append(out, "PATH="+dir)
}

// runAgentHarness is runAgent with the repo's harness session, when it has one: open while the
// agent runs, ended as soon as it stops (lease lost and shutdown too), before the runner's own
// verify, which starts the harness itself.
func (r *Runner) runAgentHarness(ctx context.Context, ru *run, brief string) outcome {
	hs, err := r.startHarness(ru)
	if err != nil {
		return outcome{reason: endStartFailed, exitCode: -1, detail: "the harness session: " + err.Error()}
	}
	if hs != nil {
		defer hs.end(context.WithoutCancel(ctx))
	}
	return r.runAgent(ctx, ru, brief)
}

// runAgent runs claude for one item, in its own process group, with heartbeats and the time cap.
func (r *Runner) runAgent(ctx context.Context, ru *run, brief string) (out outcome) {
	out = outcome{reason: endExited, exitCode: -1}
	stdin, err := os.Open(filepath.Join(ru.dir, "brief.md"))
	if err != nil {
		return outcome{reason: endStartFailed, detail: err.Error()}
	}
	defer stdin.Close()
	stderr, err := os.OpenFile(filepath.Join(ru.dir, "stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return outcome{reason: endStartFailed, detail: err.Error()}
	}
	defer stderr.Close()
	defer func() { out.stderr = stderrTail(stderr.Name(), 600) }()
	cmd := r.agentCommand(ru)
	handle := func(line []byte) (string, string) { return r.handle(ru, &out.stats, line) }
	if ru.builder == pipeline.BuilderQwen {
		oc, stop, err := r.openCodeCommand(ru)
		if err != nil {
			return outcome{reason: endStartFailed, detail: "the qwen builder could not start: " + err.Error()}
		}
		defer stop()
		// No guard runs inside OpenCode: before the runner's git reads the worktree again, its
		// plumbing must be as it was (the fence keeps most of it read-only; this is the rest).
		plumbing := gitPlumbing(ru.wt, ru.mirror)
		defer func() {
			changed, err := restorePlumbing(plumbing)
			if len(changed) == 0 && err == nil {
				return
			}
			out.reason, out.detail = endGitTampered, fmt.Sprintf("the qwen build changed the worktree's git plumbing (%s); the runner put it back and runs no git on this work", strings.Join(changed, ", "))
			if err != nil {
				out.detail = fmt.Sprintf("the qwen build changed the worktree's git plumbing (%s) and the runner could not put it all back (%v); stop the runner and check %s", strings.Join(changed, ", "), err, ru.mirror)
				r.paused.Store(out.detail)
			}
		}()
		cmd = oc
		handle = func(line []byte) (string, string) { r.handleOpenCode(&out.stats, line); return "", "" }
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return outcome{reason: endStartFailed, detail: err.Error()}
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, pw, stderr
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return outcome{reason: endStartFailed, detail: err.Error()}
	}
	pw.Close()
	pid := cmd.Process.Pid

	lines := make(chan []byte, 64)
	go func() {
		defer close(lines)
		defer pr.Close()
		logf, _ := os.OpenFile(filepath.Join(ru.dir, "stream.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if logf != nil {
			defer logf.Close()
		}
		br := bufio.NewReaderSize(pr, 1<<20)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				if logf != nil {
					logf.Write(line)
				}
				lines <- line
			}
			if err != nil {
				return
			}
		}
	}()
	exited := make(chan *os.ProcessState, 1)
	go func() {
		ps, _ := cmd.Process.Wait()
		exited <- ps
	}()

	maxRun := r.maxRun(ctx, ru)
	// Stop a little before loopd's own cap, so the run is saved and reported by the runner. A
	// retry has what the first attempt left.
	capAt := maxRun - 30*time.Second
	if capAt <= 0 {
		capAt = maxRun
	}
	capAt -= ru.spent
	hb := r.Clock.NewTimer(r.heartbeatEvery())
	capT := r.Clock.NewTimer(capAt)
	var killT Timer
	defer func() {
		hb.Stop()
		capT.Stop()
		if killT != nil {
			killT.Stop()
		}
	}()
	terminating := false
	terminate := func(reason, detail string) {
		if terminating {
			return
		}
		terminating = true
		out.reason, out.detail = reason, detail
		_ = signalGroup(pid, syscall.SIGTERM)
		killT = r.Clock.NewTimer(r.killGrace())
	}
	done := ctx.Done()
	for {
		var killC <-chan time.Time
		if killT != nil {
			killC = killT.C()
		}
		select {
		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			if why, detail := handle(line); why != "" {
				terminate(why, detail)
			}
		case ps := <-exited:
			if ps != nil {
				out.exitCode = ps.ExitCode()
			}
			// Whatever the agent left running in its group goes with it.
			_ = signalGroup(pid, syscall.SIGKILL)
			drain := time.NewTimer(5 * time.Second)
		drainLoop:
			for lines != nil {
				select {
				case line, ok := <-lines:
					if !ok {
						break drainLoop
					}
					if why, detail := handle(line); why != "" && out.reason == endExited {
						out.reason, out.detail = why, detail
					}
				case <-drain.C:
					break drainLoop
				}
			}
			drain.Stop()
			return out
		case <-hb.C():
			r.flushRefusals(ctx, ru)
			stop, err := r.Client.Heartbeat(ctx, ru.lease, fmt.Sprintf("agent running: %d tool calls", out.stats.toolResults),
				r.agentActivity(ru, &out.stats))
			var ae *APIError
			if err == nil && stop {
				terminate(endStopped, "loopd asked to stop")
			} else if errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 {
				terminate(endStopped, "the lease is gone: "+ae.Error())
			} else {
				if err != nil {
					r.Log.Warn("heartbeat", "item", ru.item.ID, "err", err)
				}
				hb = r.Clock.NewTimer(r.heartbeatEvery())
			}
		case <-capT.C():
			terminate(endTimeCap, fmt.Sprintf("the run hit the time cap (%d minutes)", int(maxRun.Minutes())))
		case <-killC:
			_ = signalGroup(pid, syscall.SIGKILL)
			killT = nil
		case <-done:
			done = nil
			terminate(endShutdown, "the runner is shutting down")
		}
	}
}

// flushRefusals sends new guard refusals to loopd as guard_refused events.
func (r *Runner) flushRefusals(ctx context.Context, ru *run) {
	b, err := os.ReadFile(filepath.Join(ru.dir, "refusals.jsonl"))
	if err != nil {
		return
	}
	// Only whole lines: the guard may be appending one right now.
	lines := strings.Split(string(b), "\n")
	lines = lines[:len(lines)-1]
	for n, line := range lines {
		if n < ru.reported {
			continue
		}
		ru.reported = n + 1
		var ref guard.Refusal
		if json.Unmarshal([]byte(line), &ref) != nil {
			continue
		}
		ru.refusals++
		if ru.refusals > 25 || ru.plan != nil || ru.orch != nil {
			// The rest are in the run's log; 25 events are enough to see a pattern. A plan turn's
			// stay there too: loopd's events API is per item.
			continue
		}
		err := r.Client.Event(ctx, ru.item.ID, "guard_refused", map[string]any{
			"rule": ref.Rule, "tool": ref.Tool, "reason": clip(ref.Reason, 300), "target": clip(ref.Target, 300), "at": ref.At})
		if err != nil {
			r.Log.Warn("guard_refused event", "item", ru.item.ID, "err", err)
		}
	}
}

func (r *Runner) advance(ctx context.Context, ru *run, outcome string, fields map[string]string, paths []string) {
	r.flushRefusals(ctx, ru)
	if fields == nil {
		fields = map[string]string{}
	}
	fields["runner_agent"] = ru.agent
	if ru.trustUsed != "" {
		fields[pipeline.FieldTrustUsed] = ru.trustUsed
	}
	if ru.trustCarried != "" {
		fields[pipeline.FieldTrustCarried] = ru.trustCarried
		fields[pipeline.FieldTrustDiffSHA] = ru.trustDiffSHA
	}
	if err := r.Client.Advance(ctx, ru.item.ID, outcome, fields, paths); err != nil {
		r.Log.Error("advance", "item", ru.item.ID, "outcome", outcome, "err", err)
		return
	}
	r.Log.Info("reported", "item", ru.item.ID, "outcome", outcome)
}

func (out outcome) lastOutput() string {
	if out.stats.lastText == "" {
		if out.stderr != "" {
			return "(no output); stderr: " + out.stderr
		}
		return "(no output)"
	}
	return out.stats.lastText
}

// stderrTail is the last n bytes or so of an agent's stderr log, its whitespace folded to single
// spaces.
func stderrTail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8RuneStart(s[i]) {
		i++
	}
	return "…" + s[i:]
}

func (r *Runner) metrics(ru *run, out outcome, f map[string]string) map[string]string {
	f["run_seconds"] = strconv.Itoa(int(r.Clock.Now().Sub(ru.started).Seconds()))
	f["exit_code"] = strconv.Itoa(out.exitCode)
	if ru.builder != "" && ru.stage == StageBuild {
		// Who built, for the digest's comparison (docs/decisions/2026-10-16-qwen-builder.md).
		f["builder"] = ru.builder
	}
	if ru.builder == pipeline.BuilderQwen {
		f["qwen_requests"] = strconv.FormatInt(ru.qwenRequests, 10)
		f["qwen_tokens_input"] = strconv.FormatInt(out.stats.qwenInput+ru.first.qwenInput, 10)
		f["qwen_tokens_output"] = strconv.FormatInt(out.stats.qwenOutput+ru.first.qwenOutput, 10)
	}
	if ru.preMerge != "" {
		f["pre_merge"] = ru.preMerge
	}
	if out.stats.initMode != "" {
		f["permission_mode"] = out.stats.initMode
	}
	if out.stats.sessionID != "" {
		f["session_id"] = out.stats.sessionID
	}
	if ru.first.sessionID != "" {
		f["first_session_id"] = ru.first.sessionID
	}
	// The usage ledger's (docs/decisions/2026-10-18-usage-awareness.md): the model, and the CLI's
	// last rate-limit reading, whose allowed_warning is the early sign of a limit.
	if m := firstNonEmpty(out.stats.model, ru.first.model); m != "" {
		f["model"] = m
	}
	if rl := out.stats.rateLimit; rl != nil {
		f["rate_limit_status"] = rl.Status
		if rl.Type != "" {
			f["rate_limit_type"] = rl.Type
		}
		if rl.ResetsAt > 0 {
			f["rate_limit_resets_at"] = unixReset(int64(rl.ResetsAt))
		}
		if rl.Utilization != nil {
			f["rate_limit_utilization"] = strconv.FormatFloat(*rl.Utilization, 'f', -1, 64)
		}
	}
	if ru.claudeBuild() {
		// Resumed vs new sessions, for the flow panel.
		f["resumed"] = strconv.FormatBool(ru.resumedFrom != nil)
		if ru.resumedFrom != nil {
			f["resumed_from"] = ru.resumedFrom.Session
		}
	}
	if res := totalResult(ru, out); res != nil {
		u := res.Usage
		f["num_turns"] = strconv.Itoa(res.NumTurns)
		f["duration_ms"] = strconv.FormatInt(res.Duration, 10)
		f["total_cost_usd"] = strconv.FormatFloat(res.CostUSD, 'f', 4, 64)
		f["usage_input"] = strconv.FormatInt(u.Input, 10)
		f["usage_output"] = strconv.FormatInt(u.Output, 10)
		f["usage_cache_creation"] = strconv.FormatInt(u.CacheCreation, 10)
		f["usage_cache_read"] = strconv.FormatInt(u.CacheRead, 10)
		f["cache_hit_ratio"] = strconv.FormatFloat(CacheHitRatio(u.Input, u.CacheCreation, u.CacheRead), 'f', 2, 64)
	}
	if ru.refusals > 0 {
		f["guard_refusals"] = strconv.Itoa(ru.refusals)
	}
	if ru.retried {
		f["result_retried"] = "true"
	}
	if ru.resumedForResult {
		f["result_resumed"] = "true"
	}
	return f
}

// totalResult is the run's result numbers: the agent's, plus a refused first attempt's when the
// stage was retried (cost, turns, duration and usage are summed). Nil when neither has one.
func totalResult(ru *run, out outcome) *resultEvent {
	a, b := ru.first.result, out.stats.result
	if a == nil || b == nil {
		if b != nil {
			return b
		}
		return a
	}
	t := *b
	t.NumTurns += a.NumTurns
	t.Duration += a.Duration
	t.CostUSD += a.CostUSD
	t.Usage.Input += a.Usage.Input
	t.Usage.Output += a.Usage.Output
	t.Usage.CacheCreation += a.Usage.CacheCreation
	t.Usage.CacheRead += a.Usage.CacheRead
	return &t
}

func (r *Runner) saveWIP(ctx context.Context, ru *run, why string, log *slog.Logger) {
	s, err := r.Git.SaveWIP(ctx, ru.wt, ru.branch, ru.repo.Base, fmt.Sprintf("WIP: loop item #%d (%s)", ru.item.ID, why))
	if s.DroppedMerge {
		log.Warn("WIP: dropped the unresolved merge of the base (the next build's pre-merge redoes it)", "branch", ru.branch)
	}
	if err != nil {
		log.Error("WIP push", "err", err)
		return
	}
	log.Info("WIP saved", "pushed", s.Pushed, "branch", ru.branch)
}

// report turns how the run ended into loopd calls.
// LowCacheTurns and LowCacheRatio: a run longer than this many turns with a cache hit ratio
// below this is recorded as low_cache_hit (a warning for the digest; nothing changes).
const (
	LowCacheTurns = 5
	LowCacheRatio = 0.5
)

func (r *Runner) report(ctx context.Context, ru *run, out outcome, log *slog.Logger) {
	r.flushRefusals(ctx, ru)
	if res := out.stats.result; res != nil && res.NumTurns > LowCacheTurns {
		u := res.Usage
		if ratio := CacheHitRatio(u.Input, u.CacheCreation, u.CacheRead); ratio < LowCacheRatio {
			log.Warn("low prompt-cache hit ratio", "ratio", ratio, "turns", res.NumTurns)
			if err := r.Client.Event(ctx, ru.item.ID, "low_cache_hit", map[string]any{
				"cache_hit_ratio": strconv.FormatFloat(ratio, 'f', 2, 64), "num_turns": strconv.Itoa(res.NumTurns),
				"usage_input": u.Input, "usage_cache_creation": u.CacheCreation, "usage_cache_read": u.CacheRead,
				"session_id": out.stats.sessionID}); err != nil {
				log.Warn("low_cache_hit event", "err", err)
			}
		}
	}
	switch out.reason {
	case endStopped, endShutdown, endTimeCap:
		// Interrupted: the next build resumes the session.
	default:
		r.dropBuildSession(ru)
	}
	switch out.reason {
	case endStopped, endShutdown:
		// The item returns to its queue (loopd ended the lease, or it will expire); keep the work.
		r.flushRefusals(ctx, ru)
		if ru.stage == StageBuild {
			r.saveWIP(ctx, ru, out.reason, log)
			r.buildInterrupted(ctx, ru, out.reason)
		}
		return
	case endTimeCap:
		detail := out.detail
		if ru.stage == StageBuild {
			r.saveWIP(ctx, ru, "time cap", log)
			r.buildInterrupted(ctx, ru, out.reason)
			detail += "; work so far is on " + ru.branch
		}
		r.advance(ctx, ru, "failed", r.metrics(ru, out, map[string]string{"error": detail + ". Last output: " + out.lastOutput()}), nil)
		return
	case endModeMismatch:
		r.paused.Store(out.detail)
		log.Error("pausing: the agent did not get the permission mode it was started with", "detail", out.detail)
		r.advance(ctx, ru, "failed", r.metrics(ru, out, map[string]string{"error": "boundary check failed: " + out.detail +
			". Auto mode may be unavailable; the runner stopped claiming until it is restarted (use mode \"strict\" to run without it)."}), nil)
		return
	case endNotArmed, endStartFailed, endGitTampered:
		r.advance(ctx, ru, "failed", r.metrics(ru, out, map[string]string{"error": "boundary check failed (" + out.reason + "): " + out.detail}), nil)
		return
	}
	if hit := out.stats.limit; hit != nil {
		f := map[string]string{"error": hit.Error}
		if hit.Reset != "" {
			f["reset"] = hit.Reset
		}
		if hit.Window != "" {
			f["limit_window"] = hit.Window
		}
		r.advance(ctx, ru, "usage_limit", r.metrics(ru, out, f), nil)
		return
	}
	raw, err := readResult(filepath.Join(ru.wt, ".loop", "result.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && out.reason == endExited && out.exitCode == 0 && out.stats.sessionID != "" && !ru.retried {
			// The agent ended its turn without a result, as one waiting on a background command
			// does: work resumes its session once.
			ru.noResult = true
			return
		}
		msg := fmt.Sprintf("the agent exited (code %d) without a usable .loop/result.json (%v)", out.exitCode, err)
		if ru.resumedForResult {
			msg = fmt.Sprintf("the agent exited (code %d) after its session was resumed once, still without a usable .loop/result.json (%v)", out.exitCode, err)
		}
		r.fail(ctx, ru, out, msg)
		return
	}
	switch ru.stage {
	case StageTriage:
		r.reportTriage(ctx, ru, out, raw)
	case StageReview:
		r.reportReview(ctx, ru, out, raw)
	default:
		r.reportBuild(ctx, ru, out, raw)
	}
}

// fail reports a run that ended without a usable result: the item goes to the user.
func (r *Runner) fail(ctx context.Context, ru *run, out outcome, msg string) {
	r.advance(ctx, ru, "failed", r.metrics(ru, out, map[string]string{"error": clip(msg, 1200) + ". Last output: " + out.lastOutput()}), nil)
}

// reportBuild checks a build result against git, pushes the branch, opens or updates the PR and
// reports pr_opened (or the agent's question).
func (r *Runner) reportBuild(ctx context.Context, ru *run, out outcome, raw []byte) {
	fail := func(msg string) { r.fail(ctx, ru, out, msg) }
	unlock := r.lockMirror(ru.mirror)
	facts, ferr := r.Git.Facts(ctx, ru.mirror, ru.wt, ru.repo.Base, ru.branch)
	unlock()
	if ferr != nil {
		fail("the runner could not read git: " + ferr.Error())
		return
	}
	v, err := ValidateResult(raw, Expect{Stage: StageBuild}, facts, filepath.Join(ru.wt, ".loop"))
	if err != nil {
		r.reject(ctx, ru, out, err.Error())
		return
	}
	if v.Outcome == "question" {
		r.advance(ctx, ru, "question", r.metrics(ru, out, map[string]string{"question": clip(v.Question, 2000)}), nil)
		return
	}
	files, err := r.evidence(ru, v.Result)
	if err != nil {
		r.reject(ctx, ru, out, "evidence: "+err.Error())
		return
	}
	// The runner's own verify steps, before anything is pushed (verify.go).
	vres, err := r.runVerify(ctx, ru, facts.HeadSHA, facts.ChangedPaths)
	if errors.Is(err, errVerifyStopped) {
		r.Log.Info("verify stopped; the build's commits stay in the mirror", "item", ru.item.ID)
		return
	}
	var tr *trustRefusal
	if errors.As(err, &tr) {
		r.saveTrustedResult(ru, tr.head, raw, files)
		r.refuseTrusted(ctx, ru, out, tr)
		return
	}
	if err != nil {
		fail("the runner's verify did not run: " + err.Error())
		return
	}
	if bad := firstFailed(vres); bad != nil {
		if err := r.uploadVerify(ctx, ru, vres); err != nil {
			fail("uploading the verify output: " + err.Error())
			return
		}
		if bad.Masked != "" {
			// Not a bounce: the next build would hit the same mask. To the user.
			fail(VerifyMaskedReason(*bad))
			return
		}
		r.advance(ctx, ru, "verify_failed", r.metrics(ru, out, map[string]string{
			"reason": VerifyFailedReason(*bad), "verify_step": bad.Name, "log_tail": VerifyLogTail(*bad), "head_sha": facts.HeadSHA}), nil)
		return
	}
	pr, action, err := r.publish(ctx, ru, v, files, out, vres)
	if err != nil {
		fail(err.Error())
		return
	}
	prVerified := "skipped"
	if r.VerifyPR != nil {
		info, err := r.VerifyPR(ctx, ru.repo.Slug, pr)
		if err != nil {
			fail(fmt.Sprintf("could not check PR #%d on GitHub: %v", pr, err))
			return
		}
		if info.HeadRefName != ru.branch || info.HeadRefOid != facts.HeadSHA || info.State != "OPEN" {
			fail(fmt.Sprintf("PR #%d is %s %s at %s, not %s at %s", pr, info.State, info.HeadRefName, info.HeadRefOid, ru.branch, facts.HeadSHA))
			return
		}
		prVerified = "true"
	}
	labels := evidenceLabels(v.Result)
	for _, f := range files {
		if err := r.Client.Upload(ctx, ru.item.ID, f.Path, f.Name, labels[f.Name]); err != nil {
			fail("uploading evidence " + f.Name + ": " + err.Error())
			return
		}
	}
	if err := r.uploadVerify(ctx, ru, vres); err != nil {
		fail("uploading the verify output: " + err.Error())
		return
	}
	fields := map[string]string{
		"repo": ru.repo.Slug, "pr": strconv.Itoa(pr), "branch": ru.branch, "head_sha": facts.HeadSHA,
		"summary": clip(v.Summary, 2000), "not_verified": clip(strings.Join(v.NotVerified, "; "), 1000), "pr_verified": prVerified,
		"pr_action": action,
	}
	if v.NeedsReview {
		fields["needs_review"] = "true"
	}
	if v.ProofChecks != nil {
		fields["proof"] = string(v.Proof)
	}
	r.advance(ctx, ru, "pr_opened", r.metrics(ru, out, fields), facts.ChangedPaths)
}

func evidenceLabels(v Result) map[string]string {
	labels := map[string]string{}
	for _, e := range v.Evidence {
		labels[filepath.Base(e.File)] = e.Label
	}
	return labels
}

// publish pushes the item's branch (only it) and opens its PR, or updates the open one, with the
// runner's own credentials; it returns the PR number and "opened" or "updated". The agent did
// neither. A branch the build rewrote (a reviewed rebase) over the PR's recorded head is pushed
// with a lease pinned to that head and recorded as branch_rewritten; any other push is never
// forced, so a remote commit the loop never recorded fails it.
func (r *Runner) publish(ctx context.Context, ru *run, v Valid, files []EvidenceFile, out outcome, vres []VerifyResult) (int, string, error) {
	if r.PRs == nil {
		return 0, "", errors.New("the runner has no way to open PRs (configuration)")
	}
	push := r.Push
	if push == nil {
		push = r.Git.PushBranch
	}
	forcePush := r.ForcePush
	if forcePush == nil {
		forcePush = r.Git.ForcePushBranch
	}
	unlock := r.lockMirror(ru.mirror)
	var err error
	rewritten, rangeDiff := "", ""
	if prev := ru.item.HeadSHA; r.Git.Rewrites(ctx, ru.mirror, ru.branch, prev) {
		rewritten = prev
		rangeDiff, _ = r.Git.RangeDiff(ctx, ru.mirror, ru.repo.Base, prev, v.HeadSHA)
		err = forcePush(ctx, ru.mirror, ru.branch, prev)
	} else {
		err = push(ctx, ru.mirror, ru.branch)
	}
	remote := ""
	if err == nil {
		remote, err = r.Git.RemoteHead(ctx, ru.mirror, ru.branch)
	}
	unlock()
	if err != nil {
		return 0, "", fmt.Errorf("the runner could not push %s: %v", ru.branch, err)
	}
	if remote != v.HeadSHA {
		return 0, "", fmt.Errorf("after the push, %s on the remote is %q, not HEAD %s", ru.branch, remote, v.HeadSHA)
	}
	if rewritten != "" {
		r.Log.Info("pushed a rewritten branch with a lease", "item", ru.item.ID, "branch", ru.branch, "old", rewritten, "new", remote)
		if err := r.Client.Event(ctx, ru.item.ID, "branch_rewritten",
			map[string]any{"old": rewritten, "new": remote, "range_diff": clip(rangeDiff, 2000)}); err != nil {
			r.Log.Warn("could not record the rewrite", "item", ru.item.ID, "err", err)
		}
	}
	st := r.runStats(ru, out)
	body := filepath.Join(ru.dir, "pr-body.md")
	if err := os.WriteFile(body, []byte(PRBody(v.PRBody, ru.item.ID, files, evidenceLabels(v.Result), st, vres, v.ProofChecks)), 0o600); err != nil {
		return 0, "", err
	}
	spec := PRSpec{Slug: ru.repo.Slug, Head: ru.branch, Base: ru.repo.Base, Title: v.PRTitle, BodyFile: body}
	pr, err := r.PRs.Find(ctx, ru.repo.Slug, ru.branch)
	if err != nil {
		return 0, "", fmt.Errorf("could not look for an open PR for %s: %v", ru.branch, err)
	}
	if pr > 0 {
		if err := r.PRs.Edit(ctx, pr, spec); err != nil {
			return 0, "", fmt.Errorf("could not update PR #%d: %v", pr, err)
		}
		return pr, "updated", nil
	}
	pr, err = r.PRs.Create(ctx, spec)
	if err != nil {
		return 0, "", fmt.Errorf("could not open the PR for %s: %v", ru.branch, err)
	}
	if pr <= 0 {
		return 0, "", fmt.Errorf("opening the PR for %s returned no number", ru.branch)
	}
	return pr, "opened", nil
}

// runStats are the run's numbers for a PR footer or a review comment.
func (r *Runner) runStats(ru *run, out outcome) RunStats {
	st := RunStats{Seconds: int(r.Clock.Now().Sub(ru.started).Seconds())}
	if res := totalResult(ru, out); res != nil {
		u := res.Usage
		st = RunStats{Turns: res.NumTurns, CacheHit: CacheHitRatio(u.Input, u.CacheCreation, u.CacheRead), Seconds: st.Seconds,
			CostUSD: res.CostUSD, HasResult: true}
	}
	st.PreMerge = ru.preMerge
	return st
}

// fetchEvidence copies the item's evidence so far (triage's notes and repro first) into rel (a
// directory under the working directory) for the agent to read. A failure is reported in the
// brief, not fatal.
func (r *Runner) fetchEvidence(ctx context.Context, ru *run, rel string) ([]TriageFile, string) {
	evs, err := r.Client.Evidence(ctx, ru.item.ID)
	if err != nil {
		return nil, err.Error()
	}
	if len(evs) == 0 {
		return nil, ""
	}
	dir := filepath.Join(ru.wt, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err.Error()
	}
	var out []TriageFile
	for _, e := range evs {
		name := fmt.Sprintf("%d-%s", e.N, safeName(e.Name))
		if err := r.Client.DownloadEvidence(ctx, ru.item.ID, e.N, filepath.Join(dir, name)); err != nil {
			return out, fmt.Sprintf("evidence %d (%s): %v", e.N, e.Name, err)
		}
		out = append(out, TriageFile{Name: name, Label: e.Label})
	}
	return out, ""
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// safeName is a file name with no path in it.
func safeName(n string) string {
	n = strings.Trim(unsafeName.ReplaceAllString(filepath.Base(n), "_"), "._")
	if n == "" {
		n = "file"
	}
	if len(n) > 80 {
		n = n[:80]
	}
	return n
}

func readResult(p string) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("result.json is not a regular file")
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20))
}

// cleanup removes what the runner made for this item: the harness sandbox (with the harness
// files restored from the base branch first, so no agent edit runs with the runner's rights), the
// worktree and the scratch dir. The run dir stays as the run's log. It never touches anything
// else.
func (r *Runner) cleanup(ctx context.Context, ru *run, log *slog.Logger) {
	if _, err := os.Stat(ru.repoDir); ru.repoDir != "" && err == nil && len(ru.repo.Cleanup) > 0 {
		restored := true
		if len(ru.repo.CleanupRestore) > 0 {
			args := append([]string{"checkout", "refs/remotes/origin/" + ru.repo.Base, "--"}, ru.repo.CleanupRestore...)
			if _, err := r.Git.run(ctx, ru.repoDir, args...); err != nil {
				log.Error("cleanup: restoring the harness files failed; not running cleanup commands", "err", err)
				restored = false
			}
		}
		for _, c := range ru.repo.Cleanup {
			if !restored {
				break
			}
			c = strings.ReplaceAll(c, "{sandbox}", ru.sandbox)
			if out, err := r.shell(ctx, ru.repoDir, c, 5*time.Minute); err != nil {
				log.Warn("cleanup command", "cmd", c, "err", err, "out", clip(out, 300))
			}
		}
	}
	if ru.stage == StageTriage {
		for _, c := range ru.checkouts {
			mirror := r.mirrorPath(c.Key)
			unlock := r.lockMirror(mirror)
			if err := r.Git.RemoveWorktree(ctx, mirror, filepath.Join(ru.wt, c.Dir)); err != nil {
				log.Warn("remove worktree", "err", err)
			}
			unlock()
		}
		if err := removeTree(ru.wt); err != nil {
			log.Warn("remove worktree", "err", err)
		}
	} else if ru.mirror != "" {
		unlock := r.lockMirror(ru.mirror)
		if err := r.Git.RemoveWorktree(ctx, ru.mirror, ru.wt); err != nil {
			log.Warn("remove worktree", "err", err)
		}
		unlock()
	}
	log.Info("cleaned up", "worktree", ru.wt)
}

// GHVerify checks a PR with the gh CLI (the runner's own GitHub credentials).
func GHVerify(ctx context.Context, slug string, pr int) (PRInfo, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", strconv.Itoa(pr), "--repo", slug, "--json", "headRefName,headRefOid,state")
	out, err := cmd.Output()
	if err != nil {
		return PRInfo{}, err
	}
	var info PRInfo
	return info, json.Unmarshal(out, &info)
}
