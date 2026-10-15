// Command loop-runner claims build items from loopd and runs one headless `claude -p` per item
// inside the four boundary layers of docs/phases/04-runner-build.md. See internal/runner.
//
//	loop-runner [--config ~/.config/jarvis-loop/runner.json] [--once] [--dry-run]
//
// --once claims and works at most one item, then exits. --dry-run skips the GitHub check of the
// agent's PR (for runs with a fake claude and a local remote); nothing else changes.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
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
	dry := fs.Bool("dry-run", false, "skip the GitHub PR check (fake claude, local remote)")
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
	if *cfg.VerifyPR && !*dry {
		r.VerifyPR = runner.GHVerify
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("loop-runner", "name", cfg.Name, "mode", cfg.Mode, "slots", cfg.Slots, "work_root", cfg.WorkRoot, "dry_run", *dry)
	if *once {
		claimed, err := r.ClaimAndRun(ctx)
		if err == nil && !claimed {
			log.Info("nothing to claim")
		}
		return err
	}
	return r.Run(ctx)
}
