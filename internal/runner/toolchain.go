package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sync"
	"time"
)

// The runner's Go toolchain check (#591, docs/decisions/2026-10-23-proof-env-hardening.md): a
// runner whose Go cannot run `go test` (the mbp's mise go@1.27 without $GOTOOLDIR/vet) runs no
// command check that runs go, and claims no work in a Go repo, so a healthy runner gets both.
// envFailure already keeps such a failure from striking; this keeps the broken runner from
// overwriting the check's evidence with "could not run" each pass and from erroring Go verifies.

// GoToolchainEvery is how often the runner checks its Go again.
const GoToolchainEvery = time.Hour

// GoProbeTimeout bounds one check.
const GoProbeTimeout = time.Minute

// goHealth is the last check of the runner's Go.
type goHealth struct {
	mu      sync.Mutex
	checked time.Time
	done    bool
	problem string
}

// goBroken returns why the runner's Go cannot run go test ("" when it can), checking it at most
// every GoToolchainEvery (GoProbe, default probeGo).
func (r *Runner) goBroken(ctx context.Context) string {
	r.goh.mu.Lock()
	defer r.goh.mu.Unlock()
	now := r.Clock.Now()
	if r.goh.done && now.Sub(r.goh.checked) < GoToolchainEvery {
		return r.goh.problem
	}
	probe := r.GoProbe
	if probe == nil {
		probe = r.probeGo
	}
	p := probe(ctx)
	if ctx.Err() != nil {
		return r.goh.problem
	}
	switch {
	case p != "" && p != r.goh.problem:
		r.Log.Warn("go toolchain broken: go proofs and Go repos' work are left to other runners", "problem", p)
	case p == "" && r.goh.problem != "":
		r.Log.Info("go toolchain works again")
	}
	r.goh.checked, r.goh.done, r.goh.problem = now, true, p
	return p
}

// probeGo runs `go tool vet -h` (go test runs vet) as proofs run commands, fenced in the agent's
// environment: a toolchain failure (envFailure: go missing, no such tool, no GOROOT, mise) is the
// problem. Anything else, the fence missing included, counts as working: the command checks have
// their own handling of that.
func (r *Runner) probeGo(ctx context.Context) string {
	dir := r.Config().WorkRoot
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	out, err := r.shell(ctx, dir, "go tool vet -h", GoProbeTimeout)
	var ee *exec.ExitError
	if errors.As(err, &ee) && envFailure(ee.ExitCode(), out, GoProbeTimeout) {
		return clip(lastLine(tail(out, 5)), 300)
	}
	return ""
}

// goCommand matches a shell line that runs go (`go test`, `mise exec go@1.27 -- go vet`).
var goCommand = regexp.MustCompile("(^|[\\s;&|(`])go\\s+\\w")

// runsGo tells whether a command runs go.
func runsGo(command string) bool { return goCommand.MatchString(command) }

// goRepo tells whether a repo is a Go repo: its base has a go.mod in the runner's mirror, or its
// prepare, setup or verify commands run go.
func (r *Runner) goRepo(ctx context.Context, key string, repo RepoConfig) bool {
	if mirror := r.mirrorPath(key); dirExists(mirror) {
		if _, err := r.Git.run(ctx, mirror, "cat-file", "-e", "refs/remotes/origin/"+repo.Base+":go.mod"); err == nil {
			return true
		}
	}
	cmds := slices.Concat(repo.Prepare, repo.Setup)
	for _, v := range repo.Verify {
		cmds = append(cmds, v.Run)
	}
	return slices.ContainsFunc(cmds, runsGo)
}

// claimRepos is what a claim in stages asks for: the repos to name (cfg.RepoNames()) and the
// stages left. While the runner's Go is broken, a claim outside land names no Go repo; with
// none left, it takes only plan and orchestrate turns (no stages: claim nothing). Go is probed
// only when the runner has a Go repo.
func (r *Runner) claimRepos(ctx context.Context, cfg *Config, stages []string) ([]string, []string) {
	names := cfg.RepoNames()
	if len(names) == 0 || slices.Equal(stages, []string{StageLand}) {
		return names, stages
	}
	kept := slices.DeleteFunc(slices.Clone(names), func(n string) bool {
		key, repo, _ := cfg.Repo(n)
		return r.goRepo(ctx, key, repo)
	})
	if len(kept) == len(names) || r.goBroken(ctx) == "" {
		return names, stages
	}
	if len(kept) > 0 {
		return kept, stages
	}
	turns := slices.DeleteFunc(slices.Clone(stages), func(s string) bool {
		return s != StagePlan && s != StageOrchestrate
	})
	return nil, turns
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
