package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// configFile writes the rig's config to a 0600 runner.json and makes the runner's config the one
// loaded from it (as loop-runner starts); edit changes the written copy.
func (g *rig) configFile(edit func(*Config)) string {
	g.t.Helper()
	p := filepath.Join(filepath.Dir(g.r.Cfg.WorkRoot), "runner.json")
	g.writeConfig(p, edit)
	c, err := LoadConfig(p)
	if err != nil {
		g.t.Fatal(err)
	}
	g.r.Cfg = c
	return p
}

func (g *rig) writeConfig(p string, edit func(*Config)) {
	g.t.Helper()
	c := *g.r.Config()
	c.Repos = map[string]RepoConfig{}
	for k, v := range g.r.Config().Repos {
		c.Repos[k] = v
	}
	if edit != nil {
		edit(&c)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		g.t.Fatal(err)
	}
}

// A reload applies to the next claim: a new repo, its verify steps, more slots. A run made before
// it keeps the config it started with.
func TestReloadAppliesToTheNextClaim(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	p := g.configFile(nil)
	claim := func(id int64, repo string) *Claim {
		return &Claim{Stage: StageBuild, Item: Item{ID: id, Title: "x", Repo: repo, Stage: StageBuild}}
	}
	live, err := g.r.newRun(claim(1, "alex/demo"), "devbox-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.r.newRun(claim(2, "alex/extra"), "devbox-2"); err == nil {
		t.Fatal("a repo the config does not have was run")
	}

	g.writeConfig(p, func(c *Config) {
		c.Slots = 2
		c.Repos["extra"] = RepoConfig{Remote: g.remote, Slug: "alex/extra", Base: "main"}
		demo := c.Repos["demo"]
		demo.Verify = []VerifyStep{{Name: "unit", Run: "true"}}
		c.Repos["demo"] = demo
	})
	if err := g.r.Reload(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g.logs.String(), "config reloaded") || !strings.Contains(g.logs.String(), "repos_added=[extra]") {
		t.Fatalf("no reload log line:\n%s", g.logs.String())
	}
	if g.r.Config().Slots != 2 {
		t.Fatalf("slots %d after the reload", g.r.Config().Slots)
	}
	next, err := g.r.newRun(claim(2, "alex/extra"), "devbox-2")
	if err != nil {
		t.Fatalf("the new repo after the reload: %v", err)
	}
	if next.repoKey != "extra" {
		t.Fatalf("repo key %q", next.repoKey)
	}
	again, err := g.r.newRun(claim(3, "alex/demo"), "devbox-3")
	if err != nil || len(again.repo.Verify) != 1 {
		t.Fatalf("a new demo run has verify %v (%v), want the reloaded step", again.repo.Verify, err)
	}
	// The live run keeps the config it started with: no verify step, one slot.
	if len(live.repo.Verify) != 0 || len(g.r.cfgOf(live).Repos["demo"].Verify) != 0 || g.r.cfgOf(live).Slots != 1 {
		t.Fatalf("the live run's config changed: %+v", g.r.cfgOf(live).Repos["demo"])
	}
}

// An invalid file, or one that changes what only a restart can, is refused; the config stays.
func TestReloadRefusesABadFile(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	p := g.configFile(nil)
	before := g.r.Config()

	os.WriteFile(p, []byte(`{"slots": 2,`), 0o600)
	if err := g.r.Reload(); err == nil {
		t.Fatal("a truncated file was taken")
	}
	g.writeConfig(p, func(c *Config) { c.Slots = 3 })
	os.Chmod(p, 0o644)
	if err := g.r.Reload(); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("a world-readable file: %v", err)
	}
	os.Chmod(p, 0o600)
	for field, edit := range map[string]func(*Config){
		"token":       func(c *Config) { c.Token += "-another" },
		"name":        func(c *Config) { c.Name = "laptop" },
		"work_root":   func(c *Config) { c.WorkRoot = c.WorkRoot + "2" },
		"loopd_url":   func(c *Config) { c.LoopdURL = "http://127.0.0.1:1" },
		"verify_mask": func(c *Config) { c.VerifyMask = append(c.VerifyMask, "/etc/more") },
	} {
		g.writeConfig(p, func(c *Config) { c.Slots = 3; edit(c) })
		err := g.r.Reload()
		if err == nil || !strings.Contains(err.Error(), field+" changed") {
			t.Errorf("%s changed: %v", field, err)
		}
	}
	if g.r.Config() != before {
		t.Fatalf("a refused reload changed the config")
	}
	g.writeConfig(p, func(c *Config) { c.Slots = 3 })
	if err := g.r.Reload(); err != nil || g.r.Config().Slots != 3 {
		t.Fatalf("a good file after bad ones: %v, slots %d", err, g.r.Config().Slots)
	}
}

// A claim after a reload tells loopd the reloaded stages and poll interval (its slot holds read
// them), not the ones the runner started with.
func TestReloadedStagesGoWithTheNextClaim(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	p := g.configFile(nil)
	seen := func() pipeline.RunnerSeen {
		t.Helper()
		if _, err := g.r.claimAndRunIn(g.ctx, g.ctx, []string{StageBuild}); err != nil {
			t.Fatal(err)
		}
		panel, err := g.st.Runners(g.ctx)
		if err != nil || len(panel.Runners) != 1 {
			t.Fatalf("runners %+v (%v)", panel.Runners, err)
		}
		return panel.Runners[0]
	}
	if r := seen(); r.PollSeconds != 30 || !slices.Equal(r.Stages, []pipeline.Stage{pipeline.Stage(StageBuild)}) {
		t.Fatalf("before the reload: poll %d, stages %v", r.PollSeconds, r.Stages)
	}
	g.writeConfig(p, func(c *Config) { c.PollSeconds = 10; c.Stages = []string{StageTriage, StageBuild} })
	if err := g.r.Reload(); err != nil {
		t.Fatal(err)
	}
	if r := seen(); r.PollSeconds != 10 || !slices.Equal(r.Stages, []pipeline.Stage{pipeline.Stage(StageBuild), pipeline.Stage(StageTriage)}) {
		t.Fatalf("after the reload: poll %d, stages %v; want 10, [build triage]", r.PollSeconds, r.Stages)
	}
}

// Run starts the slots a reload adds: with one slot held by a hanging build, the second build
// waits; a reload to two slots claims it, and the first run goes on.
func TestReloadAddsASlot(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	p := g.configFile(nil)
	first := g.toBuild("Slow build")
	g.script(first, StageBuild, fakeclaude.Hang, "")
	second := g.toBuild("Waits for a slot")
	g.script(second, StageBuild, fakeclaude.Hang, "")

	stop := g.runInBackground()
	defer stop()
	waitFile(t, filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-build", first), "wip.txt"))
	if d := g.item(second); d.Lease != nil {
		t.Fatalf("the second build was claimed with one slot")
	}
	g.writeConfig(p, func(c *Config) { c.Slots = 2 })
	if err := g.r.Reload(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-build", second), "wip.txt"))
	if d := g.item(first); d.Lease == nil {
		t.Fatalf("the first build lost its lease over the reload")
	}
}

// Drain stops the claims, lets the live run finish and report, tells loopd, and ends Run.
func TestDrainLetsTheLiveRunFinish(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	live := g.toBuild("Half way through")
	g.script(live, StageBuild, fakeclaude.PlanWait, "")
	queued := g.toBuild("Waits for the restart")

	ctx, cancel := context.WithCancel(g.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.r.Run(ctx) }()
	waitFile(t, filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-build", live), "wip.txt"))

	if err := g.r.Drain(g.ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := g.st.RunnerStates(g.ctx); err != nil || len(got) != 1 || got[0].State != store.RunnerDraining {
		t.Fatalf("loopd's runner states %+v %v", got, err)
	}
	select {
	case <-done:
		t.Fatal("Run ended with a run live")
	case <-time.After(200 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(g.scripts, fmt.Sprintf("%d.release", live)), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("Run did not end after the drain; log:\n%s", g.logs.String())
	}
	if ev := g.advanced(live); ev.Outcome != "pr_opened" {
		t.Fatalf("the live run's advance %+v; log:\n%s", ev, g.logs.String())
	}
	if d := g.item(queued); d.Item.Stage != pipeline.Build || d.Lease != nil || len(g.events(queued, "claimed")) != 1 {
		t.Fatalf("the queued item was claimed while draining: %s %+v", d.Item.Stage, d.Lease)
	}
	if g.runOnce() {
		t.Fatal("a drained runner claimed")
	}
}
