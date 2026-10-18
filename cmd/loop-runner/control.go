package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/alexberardi/jarvis-loop/internal/runner"
)

// pidName is the running loop-runner's pid file, in its work root: `loop-runner reload` and
// `loop-runner drain` find it there.
const pidName = "loop-runner.pid"

// onSignals reloads the config on SIGHUP and drains on SIGUSR1 until ctx ends.
func onSignals(ctx context.Context, r *runner.Runner, sigs <-chan os.Signal, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-sigs:
			switch s {
			case syscall.SIGHUP:
				if err := r.Reload(); err != nil {
					log.Error("config reload refused: the config stays as it was", "err", err)
				}
			case syscall.SIGUSR1:
				if err := r.Drain(ctx); err != nil {
					log.Warn("draining, but loopd was not told (the board will not show it)", "err", err)
				}
			}
		}
	}
}

// writePID writes this process's pid to the work root's pid file.
func writePID(workRoot string) (string, error) {
	p := filepath.Join(workRoot, pidName)
	if err := os.MkdirAll(workRoot, 0o700); err != nil {
		return "", err
	}
	return p, os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

// removePID removes the pid file if it is still this process's (a second runner may have
// replaced it).
func removePID(p string) {
	if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(os.Getpid()) {
		os.Remove(p)
	}
}

// isLoopRunner says whether pid is a loop-runner process, so a stale pid file never signals
// another program.
var isLoopRunner = func(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	return err == nil && filepath.Base(strings.TrimSpace(string(out))) == "loop-runner"
}

// control is `loop-runner reload` and `loop-runner drain`: it signals the runner whose pid file is
// in the config's work root.
func control(cmd string, args []string) error {
	fs := flag.NewFlagSet("loop-runner "+cmd, flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfig, "runner config (0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := runner.LoadConfig(runner.Expand(*cfgPath))
	if err != nil {
		return err
	}
	sig, name, what := syscall.SIGHUP, "SIGHUP", "it reloads its config for the next claim (its log says whether it took the file)"
	if cmd == "drain" {
		sig, name, what = syscall.SIGUSR1, "SIGUSR1", "it claims nothing new and exits once its live runs end"
	}
	pid, err := signalRunner(cfg.WorkRoot, sig)
	if err != nil {
		return err
	}
	fmt.Printf("sent %s to loop-runner (pid %d): %s\n", name, pid, what)
	return nil
}

// signalRunner sends sig to the loop-runner whose pid file is in workRoot.
func signalRunner(workRoot string, sig syscall.Signal) (int, error) {
	p := filepath.Join(workRoot, pidName)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("no %s: is loop-runner running (and not with --once)?", p)
	}
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("%s does not hold a pid", p)
	}
	if !isLoopRunner(pid) {
		return 0, fmt.Errorf("pid %d (from %s) is not a running loop-runner: a stale pid file", pid, p)
	}
	return pid, syscall.Kill(pid, sig)
}
