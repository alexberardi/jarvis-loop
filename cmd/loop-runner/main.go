// Command loop-runner claims triage, build and review items (config `stages`) from loopd and runs
// one headless `claude -p` per item inside the four boundary layers of
// docs/phases/04-runner-build.md (stages: docs/phases/05-triage-review.md). With "land" in its
// stages it also lands approved items itself, with no agent (docs/phases/06-land.md). See
// internal/runner.
//
//	loop-runner [--config ~/.config/jarvis-loop/runner.json] [--once] [--dry-run]
//
// --once claims and works at most one item (and, after a merge, watches main's CI), then exits.
// --dry-run opens no PR and skips the GitHub check (for runs with a fake claude and a local
// remote); the runner still pushes the item's branch to the configured remote. A dry run cannot
// land.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/alexberardi/jarvis-loop/internal/runner"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "loop-runner:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("loop-runner", flag.ContinueOnError)
	cfgPath := fs.String("config", "~/.config/jarvis-loop/runner.json", "runner config (0600)")
	once := fs.Bool("once", false, "work at most one item, then exit")
	dry := fs.Bool("dry-run", false, "open no PR and skip the GitHub PR check (fake claude, local remote)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := runner.LoadConfig(runner.Expand(*cfgPath))
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	r := &runner.Runner{
		Cfg: cfg, Client: &runner.Client{Base: cfg.LoopdURL, Token: cfg.Token}, Clock: runner.RealClock{}, Log: log,
	}
	// The runner, not the agent, pushes the item's branch and opens its PR, with the user's own
	// git and gh credentials (docs/decisions/2026-10-15-runner-pushes.md). gh runs in a directory
	// of the runner's own that is no checkout, so it never acts on a local repository.
	ghDir := filepath.Join(cfg.WorkRoot, "gh")
	if err := os.MkdirAll(ghDir, 0o700); err != nil {
		return err
	}
	gh := runner.GH{Dir: ghDir}
	r.PRs = gh
	if *dry {
		r.PRs = runner.DryRunPRs{}
		if slices.Contains(cfg.Stages, runner.StageLand) {
			return fmt.Errorf("a --dry-run runner cannot land: take \"land\" out of the config's stages")
		}
	} else {
		r.Land = gh  // only with land in stages does the runner ever claim a land item
		r.Close = gh // closes the PRs of the items the user dropped
	}
	if *cfg.VerifyPR && !*dry {
		r.VerifyPR = runner.GHVerify
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Harness sessions end with their runs; this ends any left (keepers, then down).
	defer r.CloseHarness()
	// Probe the fence once: without it, setup, prepare, cleanup and verify commands refuse to run
	// (their items go to the user); nothing ever runs them unfenced.
	if err := r.ProbeFence(); err != nil {
		log.Error("fence unavailable: items with setup or verify steps will go to the user", "err", err)
	}
	// The commit this build contains: the qa items of a qa_after_deploy repo (the loop itself) whose
	// merge it contains are reported deployed (docs/decisions/2026-10-16-qa-after-deploy.md).
	rev, why := runner.BuildRevision()
	r.Revision = rev
	if rev == "" {
		log.Warn("build revision unknown: no deployed reports for qa_after_deploy repos (use /loop verified)", "why", why)
	}
	log.Info("loop-runner", "revision", rev, "name", cfg.Name, "mode", cfg.Mode, "slots", cfg.Slots, "stages", cfg.Stages, "work_root", cfg.WorkRoot, "dry_run", *dry)
	if *once {
		claimed, err := r.ClaimAndRun(ctx)
		if err == nil && !claimed {
			log.Info("nothing to claim")
		}
		r.Wait()
		return err
	}
	return r.Run(ctx)
}
