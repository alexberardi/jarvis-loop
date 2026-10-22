package main

import (
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// `loop-runner drain` signals the pid in the work root's pid file, and only a loop-runner: a stale
// file naming another program is refused.
func TestSignalRunner(t *testing.T) {
	root := t.TempDir()
	if _, err := signalRunner(root, syscall.SIGUSR1); err == nil || !strings.Contains(err.Error(), "is loop-runner running") {
		t.Fatalf("no pid file: %v", err)
	}
	p, err := writePID(root)
	if err != nil {
		t.Fatal(err)
	}
	// The test binary is not a loop-runner.
	if _, err := signalRunner(root, syscall.SIGUSR1); err == nil || !strings.Contains(err.Error(), "stale pid file") {
		t.Fatalf("another program's pid: %v", err)
	}

	orig := isLoopRunner
	isLoopRunner = func(pid int) bool { return pid == os.Getpid() }
	t.Cleanup(func() { isLoopRunner = orig })
	got := make(chan os.Signal, 1)
	signal.Notify(got, syscall.SIGUSR1)
	defer signal.Stop(got)
	pid, err := signalRunner(root, syscall.SIGUSR1)
	if err != nil || pid != os.Getpid() {
		t.Fatalf("pid %d, %v", pid, err)
	}
	select {
	case s := <-got:
		if s != syscall.SIGUSR1 {
			t.Fatalf("got %v", s)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no SIGUSR1")
	}

	removePID(p)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("the pid file stayed: %v", err)
	}
	// Another runner's pid file is left alone.
	os.WriteFile(filepath.Join(root, pidName), []byte(strconv.Itoa(os.Getpid()+1)), 0o600)
	removePID(p)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("another runner's pid file was removed: %v", err)
	}
}

// A runner refuses to start while the pid file names a live loop-runner, and takes over a stale
// one (a dead pid, another program, or garbage) (#474).
func TestWritePIDRefusesALiveRunner(t *testing.T) {
	orig := isLoopRunner
	t.Cleanup(func() { isLoopRunner = orig })
	live := os.Getpid() + 1
	isLoopRunner = func(pid int) bool { return pid == live }
	root := t.TempDir()
	p := filepath.Join(root, pidName)

	os.WriteFile(p, []byte(strconv.Itoa(live)+"\n"), 0o600)
	if _, err := writePID(root); err == nil || !strings.Contains(err.Error(), "already running (pid "+strconv.Itoa(live)) {
		t.Fatalf("live runner: %v", err)
	}
	if b, _ := os.ReadFile(p); strings.TrimSpace(string(b)) != strconv.Itoa(live) {
		t.Fatalf("the live runner's pid file was changed: %q", b)
	}

	for _, stale := range []string{strconv.Itoa(live + 1), "not a pid", ""} {
		os.WriteFile(p, []byte(stale), 0o600)
		if _, err := writePID(root); err != nil {
			t.Fatalf("stale %q: %v", stale, err)
		}
		if b, _ := os.ReadFile(p); strings.TrimSpace(string(b)) != strconv.Itoa(os.Getpid()) {
			t.Fatalf("stale %q: the pid file holds %q", stale, b)
		}
	}
}
