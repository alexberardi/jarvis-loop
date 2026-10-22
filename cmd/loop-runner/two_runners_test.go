package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A second runner never takes the live runner's pid file (#474): two runners on one work root,
// the second (plain, then the mistyped `loop-runner version` of the mbp case) leaves the first's pid
// file alone, and `loop-runner drain` still reaches the first.
func TestTwoRunnersDrainTheFirst(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the loop-runner binary")
	}
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("no ps: isLoopRunner needs it")
	}
	dir := t.TempDir()
	// The name must be loop-runner: isLoopRunner checks the process's command name.
	bin := filepath.Join(dir, "loop-runner")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	work := filepath.Join(dir, "work")
	cfg := filepath.Join(dir, "runner.json")
	// loopd is unreachable: the runner keeps polling until it is drained.
	conf := `{"name": "t474", "loopd_url": "http://127.0.0.1:1", "token": "` + strings.Repeat("x", 40) + `",
		"work_root": "` + work + `", "poll_seconds": 1,
		"repos": {"demo": {"remote": "/nonexistent.git", "slug": "alexberardi/demo"}}}`
	if err := os.WriteFile(cfg, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(work, pidName)

	var aLog bytes.Buffer
	a := exec.Command(bin, "--config", cfg)
	a.Stdout, a.Stderr = &aLog, &aLog
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	aDone := make(chan error, 1)
	go func() { aDone <- a.Wait() }()
	t.Cleanup(func() {
		a.Process.Kill()
		<-aDone
	})
	holdsA := func() bool {
		b, err := os.ReadFile(pidFile)
		return err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(a.Process.Pid)
	}
	for deadline := time.Now().Add(20 * time.Second); !holdsA(); {
		if time.Now().After(deadline) {
			t.Fatalf("runner A never wrote its pid file; its log:\n%s", aLog.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// runAgain runs the binary to its end (or 20 s) and returns its exit status and output.
	runAgain := func(args ...string) (int, string) {
		t.Helper()
		c := exec.Command(bin, args...)
		var out bytes.Buffer
		c.Stdout, c.Stderr = &out, &out
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- c.Wait() }()
		select {
		case err := <-done:
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode(), out.String()
			}
			if err != nil {
				t.Fatal(err)
			}
			return 0, out.String()
		case <-time.After(20 * time.Second):
			c.Process.Kill()
			<-done
			t.Fatalf("loop-runner %v was still running after 20 s (a second runner?):\n%s", args, out.String())
			return 0, ""
		}
	}

	// Runner B refuses to start while A is live, and leaves A's pid file.
	code, out := runAgain("--config", cfg)
	if code != 1 || !strings.Contains(out, "already running") {
		t.Fatalf("runner B: exit %d, %q", code, out)
	}
	if !holdsA() {
		t.Fatal("runner B took or removed A's pid file")
	}
	// `loop-runner version` prints the revision and starts no runner.
	code, out = runAgain("--config", cfg, "version")
	if code != 0 || !strings.HasPrefix(out, "loop-runner ") || strings.Contains(out, "msg=") {
		t.Fatalf("version: exit %d, %q", code, out)
	}
	if code, out = runAgain("version"); code != 0 || !strings.HasPrefix(out, "loop-runner ") {
		t.Fatalf("version: exit %d, %q", code, out)
	}
	// An unknown subcommand is an error, not a runner.
	if code, out = runAgain("--config", cfg, "bogus"); code != 1 || !strings.Contains(out, `unknown subcommand "bogus"`) {
		t.Fatalf("bogus: exit %d, %q", code, out)
	}
	if !holdsA() {
		t.Fatal("version or bogus took or removed A's pid file")
	}

	// Draining reaches A: it exits drained and removes its own pid file.
	if code, out = runAgain("drain", "--config", cfg); code != 0 || !strings.Contains(out, "pid "+strconv.Itoa(a.Process.Pid)) {
		t.Fatalf("drain: exit %d, %q", code, out)
	}
	select {
	case err := <-aDone:
		aDone <- err // for the cleanup
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != exitDrained {
			t.Fatalf("runner A after drain: %v; its log:\n%s", err, aLog.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("runner A did not exit after drain; its log:\n%s", aLog.String())
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("A's pid file stayed after it exited: %v", err)
	}
}
