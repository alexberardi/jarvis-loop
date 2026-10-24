package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// brokenGo makes the rig's Go probe say what the mbp's did (#591); the returned pointer counts
// the probes, and setting the string it closes over mends it.
func (g *rig) brokenGo() (problem *string, probes *int) {
	p, n := `go: no such tool "vet"`, 0
	g.r.GoProbe = func(context.Context) string { n++; return p }
	return &p, &n
}

// A runner whose Go is broken runs no command check that runs go: not run, not reported, so the
// check stays pending for a runner whose Go works, with no "could not run" evidence. Its other
// checks run as before. The probe runs once an hour, and a mended Go runs the go check.
func TestGoBrokenRunnerLeavesGoProofs(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	problem, probes := g.brokenGo()
	id := g.toQAWithProof(t, `[{"kind":"command","run":"go version"},{"kind":"command","run":"grep -q demo README.md"}]`)
	if err := g.r.CheckProofs(g.ctx); err != nil {
		t.Fatal(err)
	}
	d := g.item(id)
	if c := d.Item.Proof.Checks[0]; c.State != pipeline.CheckPending || c.Evidence != "" || c.Fails != 0 {
		t.Fatalf("go check %+v; log:\n%s", c, g.logs.String())
	}
	if c := d.Item.Proof.Checks[1]; c.State != pipeline.CheckPassed {
		t.Fatalf("other check %+v; log:\n%s", c, g.logs.String())
	}
	if n := strings.Count(g.logs.String(), "proof command ended"); n != 1 {
		t.Fatalf("%d commands ran, want only the non-go one; log:\n%s", n, g.logs.String())
	}
	if !strings.Contains(g.logs.String(), `level=WARN msg="go toolchain broken`) {
		t.Fatalf("no warning; log:\n%s", g.logs.String())
	}
	g.loopdClk.Advance(pipeline.ProofRecheck)
	if err := g.r.CheckProofs(g.ctx); err != nil {
		t.Fatal(err)
	}
	if *probes != 1 || g.item(id).Item.Proof.Checks[0].State != pipeline.CheckPending {
		t.Fatalf("probes %d, check %+v", *probes, g.item(id).Item.Proof.Checks[0])
	}
	*problem = ""
	g.clk.Advance(GoToolchainEvery)
	g.loopdClk.Advance(pipeline.ProofRecheck)
	if err := g.r.CheckProofs(g.ctx); err != nil {
		t.Fatal(err)
	}
	if d := g.item(id); *probes != 2 || d.Item.Stage != pipeline.Done || d.Item.Proof.Checks[0].State != pipeline.CheckPassed {
		t.Fatalf("probes %d, stage %s, check %+v; log:\n%s", *probes, d.Item.Stage, d.Item.Proof.Checks[0], g.logs.String())
	}
}

// A runner whose Go is broken names no Go repo in a claim outside land: one whose base has a
// go.mod in the runner's mirror, or whose prepare, setup or verify commands run go. With none
// left it claims only plan and orchestrate turns. A working Go changes nothing.
func TestGoBrokenRunnerClaimsNoGoRepoWork(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	cfg := g.r.Cfg
	all := cfg.RepoNames()
	problem, _ := g.brokenGo()
	*problem = ""
	if repos, stages := g.r.claimRepos(g.ctx, cfg, []string{StageBuild}); !slices.Equal(repos, all) || !slices.Equal(stages, []string{StageBuild}) {
		t.Fatalf("working go: %v %v", repos, stages)
	}

	g = newRig(t, fakeclaude.Good)
	cfg = g.r.Cfg
	g.brokenGo()
	if repos, _ := g.r.claimRepos(g.ctx, cfg, []string{StageBuild}); !slices.Equal(repos, all) {
		t.Fatalf("no Go repo: %v, want %v", repos, all)
	}
	g.pushMain("go.mod", "module demo\n")
	if err := g.r.Git.EnsureMirror(g.ctx, g.r.mirrorPath("demo"), cfg.Repos["demo"]); err != nil {
		t.Fatal(err)
	}
	if repos, _ := g.r.claimRepos(g.ctx, cfg, []string{StageBuild}); slices.Contains(repos, "demo") || slices.Contains(repos, "alex/demo") || !slices.Contains(repos, "alex/other") {
		t.Fatalf("demo has a go.mod: %v", repos)
	}
	if repos, _ := g.r.claimRepos(g.ctx, cfg, []string{StageLand}); !slices.Equal(repos, all) {
		t.Fatalf("land: %v, want %v", repos, all)
	}
	rc := cfg.Repos["other"]
	rc.Verify = []VerifyStep{{Name: "tests", Run: "CGO_ENABLED=1 go test -race ./..."}}
	cfg.Repos["other"] = rc
	repos, stages := g.r.claimRepos(g.ctx, cfg, []string{StageBuild, StagePlan, StageOrchestrate})
	if repos != nil || !slices.Equal(stages, []string{StagePlan, StageOrchestrate}) {
		t.Fatalf("only Go repos: %v %v", repos, stages)
	}
	id := g.toBuild("Go change")
	if g.runOnce() {
		t.Fatalf("claimed a Go repo's build; log:\n%s", g.logs.String())
	}
	if d := g.item(id); d.Item.Stage != pipeline.Build || d.Lease != nil {
		t.Fatalf("stage %s, lease %+v", d.Item.Stage, d.Lease)
	}
}

func TestRunsGo(t *testing.T) {
	for cmd, want := range map[string]bool{
		"go test ./internal/modules/tts/":            true,
		"cd x && go vet ./...":                       true,
		"CGO_ENABLED=1 go test -race ./...":          true,
		"mise exec go@1.27 -- go test -c ./x":        true,
		"(go build ./cmd/x)":                         true,
		"scripts/check":                              false,
		"grep -q golang README.md":                   false,
		"npx jest src/go.test.ts":                    false,
		"python -m pytest tests/test_go_to_sleep.py": false,
	} {
		if got := runsGo(cmd); got != want {
			t.Errorf("runsGo(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// The default probe runs `go tool vet -h` fenced in the agent's environment: go's own "no such
// tool" (or go missing) is a broken toolchain; vet's usage, whatever its exit code, is a working one.
func TestProbeGo(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	if err := g.r.ProbeFence(); err != nil {
		t.Fatalf("bubblewrap cannot run here (the runner needs it): %v", err)
	}
	// Under the work root, which the fence binds in: it puts a private tmpfs over /tmp, where
	// t.TempDir() is on CI.
	bin := filepath.Join(g.r.Config().WorkRoot, "probe-go")
	env := g.r.Env
	for _, c := range []struct{ name, script, want string }{
		{"no vet", "echo 'go: no such tool \"vet\"' >&2; exit 2", `go: no such tool "vet"`},
		{"vet usage", "echo 'usage: vet [flags]'; exit 1", ""},
		{"no go", "", "bash: line 1: go: command not found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(bin, c.name)
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			sh, err := exec.LookPath("bash") // the fence runs the command with bash
			if err != nil {
				t.Fatal(err)
			}
			os.Symlink(sh, filepath.Join(path, "bash"))
			if c.script != "" {
				os.WriteFile(filepath.Join(path, "go"), []byte("#!/bin/sh\n"+c.script+"\n"), 0o755)
			}
			g.r.Env = append(slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return strings.HasPrefix(kv, "PATH=") }), "PATH="+path)
			got := g.r.probeGo(g.ctx)
			if got != c.want {
				t.Fatalf("probeGo = %q, want %q", got, c.want)
			}
		})
	}
}
