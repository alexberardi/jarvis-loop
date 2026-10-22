// Command loop-runner claims triage, build and review items (config `stages`) from loopd and runs
// one headless `claude -p` per item inside the four boundary layers of
// docs/phases/04-runner-build.md (stages: docs/phases/05-triage-review.md). With "land" in its
// stages it also lands approved items itself, with no agent (docs/phases/06-land.md). See
// internal/runner.
//
//	loop-runner [--config ~/.config/jarvis-loop/runner.json] [--once] [--dry-run]
//	loop-runner reload|drain [--config ...]
//	loop-runner version
//
// SIGHUP (loop-runner reload) reloads the config for the next claim; live runs keep theirs, and an
// invalid file is refused with a log line. SIGUSR1 (loop-runner drain) stops claiming, waits for
// the live runs to end and exits 0, for the service manager to start the new binary
// (docs/decisions/2026-10-18-runner-reload-drain.md). SIGINT and SIGTERM end the runs, as before.
//
// Only one runner runs on a work root: a second refuses to start while the pid file names a live
// one. Any other argument is an error, never a runner.
//
// --once claims and works at most one item (and, after a merge, watches main's CI), then exits.
// --dry-run opens no PR and skips the GitHub check (for runs with a fake claude and a local
// remote); the runner still pushes the item's branch to the configured remote. A dry run cannot
// land.
package main

import (
	"context"
	"errors"
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
	err := run(os.Args[1:])
	if err != nil && !errors.Is(err, errDrained) {
		fmt.Fprintln(os.Stderr, "loop-runner:", err)
	}
	os.Exit(exitCode(err))
}

// errDrained is run's end after a drain: the live runs ended and the runner wants restarting.
var errDrained = errors.New("drained")

// exitDrained is the exit status after a drain (EX_TEMPFAIL): not 0, so a service that restarts
// only on failure (systemd Restart=on-failure, as the dev box's) starts the new binary too (#284).
const exitDrained = 75

func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errDrained):
		return exitDrained
	default:
		return 1
	}
}

// version prints the commit this binary was built from, or why it is unknown.
func version() error {
	rev, why := runner.BuildRevision()
	if rev == "" {
		rev = "unknown: " + why
	}
	fmt.Println("loop-runner", rev)
	return nil
}

const defaultConfig = "~/.config/jarvis-loop/runner.json"

func run(args []string) error {
	if len(args) > 0 && (args[0] == "reload" || args[0] == "drain") {
		return control(args[0], args[1:])
	}
	if len(args) > 0 && args[0] == "version" {
		return version()
	}
	fs := flag.NewFlagSet("loop-runner", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfig, "runner config (0600)")
	once := fs.Bool("once", false, "work at most one item, then exit")
	dry := fs.Bool("dry-run", false, "open no PR and skip the GitHub PR check (fake claude, local remote)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// flag stops at the first non-flag: a leftover word is a subcommand (`loop-runner --config c
	// version`) or a typo, and never starts a runner (#474).
	switch {
	case fs.NArg() == 1 && fs.Arg(0) == "version":
		return version()
	case fs.NArg() > 0:
		return fmt.Errorf("unknown subcommand %q (want reload, drain or version)", fs.Arg(0))
	}
	cfg, err := runner.LoadConfig(runner.Expand(*cfgPath))
	if err != nil {
		return err
	}
	if !*once {
		// Before anything else, so a second runner does nothing (#474).
		pid, err := writePID(cfg.WorkRoot)
		if err != nil {
			return err
		}
		defer removePID(pid)
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
	// SIGHUP reloads the config and SIGUSR1 drains; neither ends a run (control.go).
	ctl := make(chan os.Signal, 4)
	signal.Notify(ctl, syscall.SIGHUP, syscall.SIGUSR1)
	defer signal.Stop(ctl)
	go onSignals(ctx, r, ctl, log)
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
	err = r.Run(ctx)
	if err == nil && r.Draining() && ctx.Err() == nil {
		log.Info("drained: the live runs ended; exiting for the service manager to restart the runner", "status", exitDrained)
		return errDrained
	}
	return err
}
