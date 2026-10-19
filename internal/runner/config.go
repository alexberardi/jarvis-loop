// Package runner is the loop runner (docs/phases/04-runner-build.md, 05-triage-review.md): it
// claims triage, build and review items from loopd, prepares a working directory from bare
// mirrors it owns (triage: the repos checked out; build: the item's branch; review: the PR's
// head), runs one headless `claude -p` per item inside four boundary layers, validates what the
// agent claims against git, and reports. The agent never holds a loopd or GitHub credential.
package runner

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Config is the runner's config file (0600: it holds the runner's loopd token).
type Config struct {
	LoopdURL string `json:"loopd_url"`
	Token    string `json:"token"`
	// OrchestratorToken is loopd's orchestrator token (docs/decisions/2026-10-17-orchestrate-turns.md):
	// a runner with stage orchestrate executes the orchestrator's actions with it. Like Token, no
	// agent ever sees it (this file is a secret to every agent and verify step).
	OrchestratorToken string   `json:"orchestrator_token,omitempty"`
	Name              string   `json:"name"`
	Capabilities      []string `json:"capabilities"`
	// OnlyResources, when set, makes the runner claim only items that need one of them (each one
	// of its capabilities): a Mac runner with ["macos"] leaves everything else to the dev box.
	OnlyResources []string `json:"only_resources,omitempty"`
	// Slots is how many agent runs this runner works at once; loopd's active-agents number still
	// caps the total. Land, with no agent, has its own lanes next to them.
	Slots int `json:"slots"`
	// LandSlots is how many land runs this runner works at once (default 3), each in its own lane
	// next to the agent slots; loopd's land limit still caps the total. A land run mostly waits on
	// CI, so one lane would land one item per CI run (#213).
	LandSlots int `json:"land_slots,omitempty"`
	// Stages are the stages this runner claims: triage, build and review (the default, all
	// three), or a subset; land, plan (plan turns, docs/phases/07-planning.md), orchestrate and
	// regression (regression.go) only when named.
	Stages   []string `json:"stages,omitempty"`
	WorkRoot string   `json:"work_root"`
	// Claude and Guard are the binaries; Guard defaults to loop-guard next to the runner.
	Claude string `json:"claude"`
	Guard  string `json:"guard"`
	// LoopHarness is the loop-harness binary (default: next to the runner), which agents of repos
	// with a harness get on their PATH and the keeper fence ends in.
	LoopHarness string `json:"loop_harness,omitempty"`
	// Mode is "auto" (the classifier) or "strict" (dontAsk and an allowlist).
	Mode        string                `json:"mode"`
	Model       string                `json:"model,omitempty"`
	PollSeconds int                   `json:"poll_seconds"`
	Repos       map[string]RepoConfig `json:"repos"`
	// Extra protected paths (never named by a command, never written) and secrets (never read).
	ProtectedPaths []string `json:"protected_paths,omitempty"`
	SecretPaths    []string `json:"secret_paths,omitempty"`
	// LoopdTokens and LoopdData are protected and secret when set (loopd on the same machine).
	LoopdTokens string `json:"loopd_tokens_file,omitempty"`
	LoopdData   string `json:"loopd_data_dir,omitempty"`
	// UserEmulator is the user's own emulator serial, which no command may name.
	UserEmulator string `json:"user_emulator"`
	// AllowedDomains extends the sandbox's network allowlist.
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	// VerifyPR checks the PR on GitHub (gh pr view) against git before reporting. Default true.
	VerifyPR *bool `json:"verify_pr,omitempty"`
	// LandCIWait is how long a land run waits for CI (or GitHub's mergeability) on the PR's head
	// before giving up; LandMainWait how long it watches main's CI after a merge. Go durations,
	// default "30m" each.
	LandCIWait   string `json:"land_ci_wait,omitempty"`
	LandMainWait string `json:"land_main_wait,omitempty"`
	// PlanTurnMinutes caps one plan turn (stage plan): the agent is stopped and the turn fails
	// after it, as an item's run is at loopd's max_run_minutes. Default 15.
	PlanTurnMinutes int `json:"plan_turn_minutes,omitempty"`
	// VerifyLock is the lock file that keeps the repos' verify steps one at a time on this
	// machine (the GPU model, the emulator and the mobile build dir are single). Default
	// ~/.cache/jarvis-loop/verify.lock, shared by every runner of the user.
	VerifyLock string `json:"verify_lock,omitempty"`
	// VerifyMask are the files and directories hidden from setup, prepare, cleanup and verify
	// commands (which run the agent's code outside its sandbox): default the credential stores,
	// loopd's tokens and data, the Slack tokens and this config (fence.go). Missing paths are
	// skipped. Bwrap is the bubblewrap binary (default /usr/bin/bwrap), the fence on Linux;
	// SandboxExec is sandbox-exec (default /usr/bin/sandbox-exec), the fence on macOS.
	VerifyMask []string `json:"verify_mask,omitempty"`
	// VerifyWritable are the paths under HOME those commands may write besides their working dir,
	// the runner's fence caches and a private /tmp (fence.go): default ~/.cache/jarvis-harness and
	// ~/.android. The rest of HOME is read-only to them.
	VerifyWritable []string `json:"verify_writable,omitempty"`
	Bwrap          string   `json:"bwrap,omitempty"`
	SandboxExec    string   `json:"sandbox_exec,omitempty"`
	// Qwen is the Qwen builder for items whose builder is qwen (qwen.go); nil: they build with
	// claude.
	Qwen *QwenConfig `json:"qwen,omitempty"`
	// Regression is the end-to-end regression a runner with stage regression runs (regression.go,
	// docs/decisions/2026-10-18-regression-trigger.md).
	Regression *RegressionConfig `json:"regression,omitempty"`

	path             string
	ciWait, mainWait time.Duration
}

// RepoConfig is one repository the runner works on, keyed by the item's repo field.
type RepoConfig struct {
	// Local is the user's clone, used only as a source of git objects; never written.
	Local string `json:"local,omitempty"`
	// Remote is the GitHub URL the mirror fetches from and the agent pushes to (https, so the
	// gh credential helper works through the sandbox proxy).
	Remote string `json:"remote"`
	// Slug is owner/name, what the agent's result must name.
	Slug string `json:"slug"`
	Base string `json:"base"`
	// Description is one line on what the repo is for: triage picks an item's repo from these.
	Description string `json:"description,omitempty"`
	// Prepare commands run by the runner in the fresh worktree before the agent (from the base
	// branch's own files), e.g. fetching the sherpa libs.
	Prepare []string `json:"prepare,omitempty"`
	// Cleanup commands run by the runner when the lease ends, after restoring the harness files
	// they use to the base branch's version; {sandbox} is the item's sandbox name.
	Cleanup        []string `json:"cleanup,omitempty"`
	CleanupRestore []string `json:"cleanup_restore,omitempty"`
	// ExcludedCommands run outside the OS sandbox (sandbox.excludedCommands). Empty by default;
	// see docs/decisions/2026-10-15-runner-choices.md before adding any.
	ExcludedCommands []string `json:"excluded_commands,omitempty"`
	// DenyWrite are worktree-relative paths no tool may change. Verify does not run on a change
	// that touches one (it would run the agent's version of a file the runner trusts).
	DenyWrite []string `json:"deny_write,omitempty"`
	// Group is the kind of repo (server, loop, mobile, docs, installer, package), shown to triage
	// and in the brief.
	Group string `json:"group,omitempty"`
	// Aliases are other words for the repo ("mobile", "node app"): triage sees them, and the runner
	// accepts them where it accepts the key.
	Aliases []string `json:"aliases,omitempty"`
	// Setup commands run by the runner in the checkout before the agent, after Prepare, outside
	// the agent's sandbox, logged in the run dir: dependencies (npm ci, a venv) are then present.
	Setup []string `json:"setup,omitempty"`
	// Verify are the runner's own steps after a build, before it pushes (verify.go): they come
	// from this config only, never from the agent or the repo's PR.
	Verify []VerifyStep `json:"verify,omitempty"`
	// Harness lets build and review agents use the repo's harness through the runner
	// (harness.go, docs/decisions/2026-10-16-harness-broker.md); nil: no harness session.
	Harness *HarnessConfig `json:"harness,omitempty"`
	// QA: every item of this repo waits in qa for the user's own check once merged (land sends
	// needs_qa with merged), as for the mobile app, which reaches TestFlight and Play only after
	// merge (docs/decisions/2026-10-15-qa.md).
	QA bool `json:"qa,omitempty"`
	// QAAfterDeploy (implies QA): the repo's change runs only once deployed, as the loop's own
	// binaries do, so the user is asked to check only when a runner built from a commit that
	// contains the merge is running (docs/decisions/2026-10-16-qa-after-deploy.md).
	QAAfterDeploy bool `json:"qa_after_deploy,omitempty"`
}

// Groups are the kinds of repo a config may name.
var Groups = []string{"server", "loop", "mobile", "docs", "installer", "package"}

// DefaultBwrap is where bubblewrap is.
const DefaultBwrap = "/usr/bin/bwrap"

// DefaultSandboxExec is where macOS's sandbox-exec is.
const DefaultSandboxExec = "/usr/bin/sandbox-exec"

// DefaultVerifyTimeout is a verify step's time limit when its config gives none.
const DefaultVerifyTimeout = 20 * time.Minute

// VerifyStep is one of a repo's verify steps.
type VerifyStep struct {
	// Name is how the PR footer, the evidence labels and the next build's notes name the step.
	Name string `json:"name"`
	// Run is a bash command line. Its environment has LOOP_ITEM, LOOP_HEAD, LOOP_WORKTREE,
	// LOOP_EVIDENCE_DIR, LOOP_SANDBOX, LOOP_REPO, LOOP_BASE and LOOP_MIRROR.
	Run string `json:"run"`
	// Repo, when set, runs the step in a fresh checkout of that configured repo (key) at its base
	// branch instead of the worktree: e.g. jarvis-server's mobile-shot for jarvis-node-mobile.
	Repo string `json:"repo,omitempty"`
	// TimeoutText is a Go duration (default 20m).
	TimeoutText string `json:"timeout,omitempty"`
	// WhenChanged are globs: the step runs only when a changed path matches one (e.g. web/**).
	WhenChanged []string `json:"when_changed,omitempty"`

	timeout time.Duration
}

// Timeout is the step's time limit.
func (v VerifyStep) Timeout() time.Duration {
	if v.timeout > 0 {
		return v.timeout
	}
	return DefaultVerifyTimeout
}

// RegressionConfig is where the regression runs: Repo's scripts/e2e (jarvis-server), at its base
// branch, with MobileRepo's base checked out for the mobile flows (jarvis-node-mobile).
type RegressionConfig struct {
	// Repo and MobileRepo are repo keys of this config. Without MobileRepo, scripts/e2e fetches
	// the app itself.
	Repo       string `json:"repo"`
	MobileRepo string `json:"mobile_repo,omitempty"`
	// TimeoutText is a Go duration (default 150m): the run is ended after it.
	TimeoutText string `json:"timeout,omitempty"`

	timeout time.Duration
}

// DefaultRegressionTimeout is a regression run's time limit when the config gives none: inside
// loopd's own cap on it (pipeline.RegressionMaxRun).
const DefaultRegressionTimeout = 150 * time.Minute

// Timeout is the run's time limit.
func (c RegressionConfig) Timeout() time.Duration {
	if c.timeout > 0 {
		return c.timeout
	}
	return DefaultRegressionTimeout
}

// HarnessConfig is a repo's harness for its agents: Up starts it (in the keeper fence, which
// keeps what it starts alive until the agent's run ends), Down stops it. Both are bash command
// lines with the verify steps' environment (LOOP_SANDBOX is the item's sandbox name).
type HarnessConfig struct {
	Up   string `json:"up"`
	Down string `json:"down"`
	// TimeoutText is a Go duration (default 20m): the limit of up, down and each run.
	TimeoutText string `json:"timeout,omitempty"`

	timeout time.Duration
}

// Timeout is the limit of one harness command.
func (h HarnessConfig) Timeout() time.Duration {
	if h.timeout > 0 {
		return h.timeout
	}
	return DefaultVerifyTimeout
}

// LoadConfig reads and checks the config file. It refuses a file group or others can read.
func LoadConfig(path string) (*Config, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is mode %o: it holds a token, make it 0600", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c.path = abs
	return &c, c.normalize()
}

// Home is the user's home directory.
func Home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "/"
	}
	return h
}

// Expand expands a leading ~ and makes the path absolute.
func Expand(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" {
		return Home()
	}
	if strings.HasPrefix(p, "~/") {
		p = filepath.Join(Home(), p[2:])
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// DefaultLandSlots is a runner's land lanes when its config does not say (loopd's default land
// limit).
const DefaultLandSlots = 3

func (c *Config) normalize() error {
	if c.Slots <= 0 {
		c.Slots = 1
	}
	if c.LandSlots <= 0 {
		c.LandSlots = DefaultLandSlots
	}
	if c.PollSeconds <= 0 {
		c.PollSeconds = 30
	}
	if c.WorkRoot == "" {
		c.WorkRoot = "~/.cache/jarvis-loop/work"
	}
	c.WorkRoot = Expand(c.WorkRoot)
	if c.Claude == "" {
		c.Claude = "claude"
	}
	if c.Guard == "" {
		if exe, err := os.Executable(); err == nil {
			c.Guard = filepath.Join(filepath.Dir(exe), "loop-guard")
		}
	}
	c.Guard = Expand(c.Guard)
	if c.LoopHarness == "" {
		if exe, err := os.Executable(); err == nil {
			c.LoopHarness = filepath.Join(filepath.Dir(exe), "loop-harness")
		}
	}
	c.LoopHarness = Expand(c.LoopHarness)
	if c.Mode == "" {
		c.Mode = ModeAuto
	}
	if len(c.Stages) == 0 {
		c.Stages = append([]string(nil), AgentStages...)
	}
	for _, st := range c.Stages {
		if !slices.Contains(RunnerStages, st) {
			return fmt.Errorf("config: stages may be %s, not %q", strings.Join(RunnerStages, ", "), st)
		}
	}
	for _, w := range []struct {
		name, val string
		to        *time.Duration
	}{{"land_ci_wait", c.LandCIWait, &c.ciWait}, {"land_main_wait", c.LandMainWait, &c.mainWait}} {
		*w.to = 30 * time.Minute
		if w.val == "" {
			continue
		}
		d, err := time.ParseDuration(w.val)
		if err != nil || d <= 0 {
			return fmt.Errorf("config: %s must be a positive duration like \"30m\", not %q", w.name, w.val)
		}
		*w.to = d
	}
	c.Stages = uniq(c.Stages)
	for _, o := range c.OnlyResources {
		if !slices.Contains(c.Capabilities, o) {
			return fmt.Errorf("config: only_resources %q is not one of the capabilities", o)
		}
	}
	switch {
	case c.PlanTurnMinutes < 0:
		return fmt.Errorf("config: plan_turn_minutes must be positive, not %d", c.PlanTurnMinutes)
	case c.PlanTurnMinutes == 0:
		c.PlanTurnMinutes = DefaultPlanTurnMinutes
	}
	if c.VerifyLock == "" {
		c.VerifyLock = "~/.cache/jarvis-loop/verify.lock"
	}
	c.VerifyLock = Expand(c.VerifyLock)
	if c.Bwrap == "" {
		c.Bwrap = DefaultBwrap
	}
	if c.SandboxExec == "" {
		c.SandboxExec = DefaultSandboxExec
	}
	if c.Qwen != nil {
		if err := c.Qwen.normalize(); err != nil {
			return err
		}
	}
	if c.VerifyMask == nil {
		c.VerifyMask = DefaultVerifyMask(Home())
	}
	for i, p := range c.VerifyMask {
		c.VerifyMask[i] = Expand(p)
	}
	c.VerifyMask = uniq(append(c.VerifyMask, c.ownSecrets()...))
	if c.VerifyWritable == nil {
		c.VerifyWritable = DefaultVerifyWritable(Home())
	}
	for i, p := range c.VerifyWritable {
		c.VerifyWritable[i] = Expand(p)
	}
	if c.UserEmulator == "" {
		c.UserEmulator = "emulator-5554"
	}
	if c.VerifyPR == nil {
		t := true
		c.VerifyPR = &t
	}
	c.LoopdTokens, c.LoopdData = Expand(c.LoopdTokens), Expand(c.LoopdData)
	for i, p := range c.ProtectedPaths {
		c.ProtectedPaths[i] = Expand(p)
	}
	for i, p := range c.SecretPaths {
		c.SecretPaths[i] = Expand(p)
	}
	switch {
	case c.Name == "":
		return fmt.Errorf("config: name is required")
	case len(c.Token) < 16:
		return fmt.Errorf("config: token is missing or too short")
	case slices.Contains(c.Stages, StageOrchestrate) && len(c.OrchestratorToken) < 16:
		return fmt.Errorf("config: stage orchestrate needs orchestrator_token (loopd's orchestrator token)")
	case c.OrchestratorToken != "" && c.OrchestratorToken == c.Token:
		return fmt.Errorf("config: orchestrator_token must be the orchestrator's own token, not the runner's")
	case c.Mode != ModeAuto && c.Mode != ModeStrict:
		return fmt.Errorf("config: mode must be %q or %q", ModeAuto, ModeStrict)
	case len(c.Repos) == 0:
		return fmt.Errorf("config: no repos")
	}
	u, err := url.Parse(c.LoopdURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("config: loopd_url %q is not an http(s) URL", c.LoopdURL)
	}
	words := map[string]string{} // key, slug or alias (lower case) -> repo key
	claim := func(word, key string) error {
		w := strings.ToLower(strings.TrimSpace(word))
		if w == "" {
			return fmt.Errorf("config: repo %s has an empty alias", key)
		}
		if other, ok := words[w]; ok && other != key {
			return fmt.Errorf("config: %q names both repo %s and repo %s", word, other, key)
		}
		words[w] = key
		return nil
	}
	for name, r := range c.Repos {
		if r.Remote == "" || r.Slug == "" || !strings.Contains(r.Slug, "/") {
			return fmt.Errorf("config: repo %s needs remote and slug (owner/name)", name)
		}
		for _, w := range []string{name, r.Slug} {
			if err := claim(w, name); err != nil {
				return err
			}
		}
	}
	for name, r := range c.Repos {
		if r.Base == "" {
			r.Base = "main"
		}
		r.Local = Expand(r.Local)
		r.QA = r.QA || r.QAAfterDeploy
		if r.Group != "" && !slices.Contains(Groups, r.Group) {
			return fmt.Errorf("config: repo %s: group must be one of %s, not %q", name, strings.Join(Groups, ", "), r.Group)
		}
		for _, a := range r.Aliases {
			if err := claim(a, name); err != nil {
				return err
			}
		}
		for _, cmd := range append(append([]string{}, r.Prepare...), r.Setup...) {
			if strings.TrimSpace(cmd) == "" {
				return fmt.Errorf("config: repo %s has an empty prepare or setup command", name)
			}
		}
		for i, v := range r.Verify {
			switch {
			case strings.TrimSpace(v.Name) == "":
				return fmt.Errorf("config: repo %s: verify step %d needs a name", name, i+1)
			case strings.TrimSpace(v.Run) == "":
				return fmt.Errorf("config: repo %s: verify step %q needs run", name, v.Name)
			}
			if v.Repo != "" {
				if _, ok := c.Repos[v.Repo]; !ok {
					return fmt.Errorf("config: repo %s: verify step %q runs in repo %q, which is not configured (use its key)", name, v.Name, v.Repo)
				}
			}
			if v.TimeoutText != "" {
				d, err := time.ParseDuration(v.TimeoutText)
				if err != nil || d <= 0 {
					return fmt.Errorf("config: repo %s: verify step %q: timeout must be a positive duration like \"20m\", not %q", name, v.Name, v.TimeoutText)
				}
				r.Verify[i].timeout = d
			}
			for _, g := range v.WhenChanged {
				if (pipeline.HoldRule{Name: "x", Glob: g}).Validate() != nil {
					return fmt.Errorf("config: repo %s: verify step %q: when_changed %q must be a relative path pattern", name, v.Name, g)
				}
			}
		}
		if h := r.Harness; h != nil {
			if strings.TrimSpace(h.Up) == "" || strings.TrimSpace(h.Down) == "" {
				return fmt.Errorf("config: repo %s: harness needs up and down", name)
			}
			if h.TimeoutText != "" {
				d, err := time.ParseDuration(h.TimeoutText)
				if err != nil || d <= 0 {
					return fmt.Errorf("config: repo %s: harness timeout must be a positive duration like \"20m\", not %q", name, h.TimeoutText)
				}
				h.timeout = d
			}
		}
		c.Repos[name] = r
	}
	return c.normalizeRegression()
}

func (c *Config) normalizeRegression() error {
	g := c.Regression
	if g == nil {
		if slices.Contains(c.Stages, StageRegression) {
			return fmt.Errorf("config: stage regression needs a regression section (its repo)")
		}
		return nil
	}
	if _, ok := c.Repos[g.Repo]; !ok {
		return fmt.Errorf("config: regression.repo %q is not a configured repo (use its key)", g.Repo)
	}
	if _, ok := c.Repos[g.MobileRepo]; g.MobileRepo != "" && !ok {
		return fmt.Errorf("config: regression.mobile_repo %q is not a configured repo (use its key)", g.MobileRepo)
	}
	// A runner with the emulator takes mobile runs: without its own checkout, scripts/e2e would look
	// for jarvis-node-mobile outside the run's checkouts.
	if g.MobileRepo == "" && slices.Contains(c.Stages, StageRegression) && slices.Contains(c.Capabilities, "emulator") {
		return fmt.Errorf("config: a runner with the emulator capability takes mobile regression runs, so regression.mobile_repo is required")
	}
	if g.TimeoutText != "" {
		d, err := time.ParseDuration(g.TimeoutText)
		if err != nil || d <= 0 || d >= pipeline.RegressionMaxRun {
			return fmt.Errorf("config: regression.timeout must be a positive duration under %s, not %q", pipeline.RegressionMaxRun, g.TimeoutText)
		}
		g.timeout = d
	}
	return nil
}

// Repo finds a repo by its config key or by its slug (loopd keeps the PR's owner/name on the
// item once a PR exists), by one of its aliases, or by the name part of its slug (an item added
// with a bare --repo jarvis-server) when no other repo has that name.
func (c *Config) Repo(name string) (string, RepoConfig, bool) {
	if r, ok := c.Repos[name]; ok {
		return name, r, true
	}
	for k, r := range c.Repos {
		if r.Slug == name {
			return k, r, true
		}
	}
	if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
		for k, r := range c.Repos {
			for _, a := range r.Aliases {
				if strings.ToLower(strings.TrimSpace(a)) == name {
					return k, r, true
				}
			}
		}
		found := ""
		for k, r := range c.Repos {
			if _, short, ok := strings.Cut(r.Slug, "/"); ok && strings.ToLower(short) == name {
				if found != "" {
					return "", RepoConfig{}, false // two owners' repos share the name: map order would pick one
				}
				found = k
			}
		}
		if found != "" {
			return found, c.Repos[found], true
		}
	}
	return "", RepoConfig{}, false
}

// Path is the config file's own path.
func (c *Config) Path() string { return c.path }

// Protected lists what no command may name and no tool may write: the user's checkouts, the
// credential stores, loopd's files and this config.
func (c *Config) Protected() []string {
	h := Home()
	out := []string{filepath.Join(h, ".config", "jarvis-harness"), filepath.Join(h, ".config", "jarvis-loop"),
		filepath.Join(h, ".ssh"), filepath.Join(h, ".claude", ".credentials.json")}
	for _, r := range c.Repos {
		if r.Local != "" {
			out = append(out, r.Local)
		}
	}
	out = append(out, c.ProtectedPaths...)
	return uniq(append(out, c.ownSecrets()...))
}

// Secrets lists what neither the file tools nor sandboxed commands may read.
func (c *Config) Secrets() []string {
	h := Home()
	out := []string{filepath.Join(h, ".config", "jarvis-harness"), filepath.Join(h, ".config", "jarvis-loop"),
		filepath.Join(h, ".ssh"), filepath.Join(h, ".claude", ".credentials.json")}
	out = append(out, c.SecretPaths...)
	return uniq(append(out, c.ownSecrets()...))
}

func (c *Config) ownSecrets() []string {
	var out []string
	creds := ""
	if c.Qwen != nil {
		creds = c.Qwen.CredentialsFile
	}
	for _, p := range []string{c.path, c.LoopdTokens, c.LoopdData, creds} {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// LoopdAddrs are the host:port spellings of loopd's address an agent could use.
func (c *Config) LoopdAddrs() []string {
	u, err := url.Parse(c.LoopdURL)
	if err != nil {
		return nil
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	hosts := []string{host}
	if ip := net.ParseIP(host); (ip != nil && ip.IsLoopback()) || host == "localhost" {
		hosts = []string{"127.0.0.1", "localhost", "[::1]", "0.0.0.0"}
	}
	var out []string
	for _, h := range hosts {
		out = append(out, net.JoinHostPort(strings.Trim(h, "[]"), port))
	}
	return out
}

func uniq(in []string) []string {
	var out []string
	for _, s := range in {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}
