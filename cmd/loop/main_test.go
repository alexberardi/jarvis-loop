package main

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/api"
	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

const (
	userTok   = "user-token-0123456789abcdef"
	runnerTok = "runner-token-0123456789abcdef"
	sha       = "0123456789abcdef0123456789abcdef01234567"
)

type cli struct {
	t   *testing.T
	url string
}

func (c cli) run(tok string, args ...string) (int, string, string) {
	c.t.Helper()
	env := map[string]string{"LOOP_URL": c.url, "LOOP_TOKEN": tok}
	var out, errb bytes.Buffer
	code := run(args, func(k string) string { return env[k] }, &out, &errb)
	return code, out.String(), errb.String()
}

func (c cli) ok(tok string, args ...string) string {
	c.t.Helper()
	code, out, errs := c.run(tok, args...)
	if code != 0 {
		c.t.Fatalf("loop %s: exit %d: %s%s", strings.Join(args, " "), code, out, errs)
	}
	return out
}

func TestCLISmoke(t *testing.T) {
	st, err := store.Open(t.TempDir(), clock.NewFake(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	toks, _ := api.ParseTokens(strings.NewReader(userTok + "\tuser\talex\n" + runnerTok + "\trunner\tdevbox\n"))
	srv := httptest.NewServer(api.New(st, toks, slog.New(slog.DiscardHandler)))
	defer srv.Close()
	c := cli{t: t, url: srv.URL}

	out := c.ok(userTok, "add", "wake word misses", "--ask", "it misses after resume", "--repo", "jarvis-server", "--priority", "P2")
	if !strings.Contains(out, "added #1") {
		t.Fatalf("add: %q", out)
	}
	if out := c.ok(userTok, "status"); !strings.Contains(out, "#1") || !strings.Contains(out, "P2") || !strings.Contains(out, "intake") {
		t.Fatalf("status: %q", out)
	}
	c.ok(userTok, "priority", "1", "P1")

	out = c.ok(runnerTok, "claim", "--runner", "devbox", "--agent", "a1", "--cap", "gpu,voice")
	if !regexp.MustCompile(`claimed #1 at triage, lease [0-9a-f]{32}`).MatchString(out) {
		t.Fatalf("claim: %q", out)
	}
	lease := regexp.MustCompile(`[0-9a-f]{32}`).FindString(out)
	if out := c.ok(runnerTok, "heartbeat", lease, "--note", "reproducing"); !strings.Contains(out, "keep going") {
		t.Fatalf("heartbeat: %q", out)
	}

	ev := filepath.Join(t.TempDir(), "repro.log")
	os.WriteFile(ev, []byte("missed 3/10\n"), 0o600)
	out = c.ok(runnerTok, "advance", "1", "reproduced", "--field", "area=voice", "--field", "size=S",
		"--field", "risk=hold", "--field", "resources=", "--evidence", ev)
	if !strings.Contains(out, "evidence 1: repro.log") || !strings.Contains(out, "#1 → build") {
		t.Fatalf("advance: %q", out)
	}
	c.ok(runnerTok, "claim", "--agent", "builder")
	c.ok(runnerTok, "advance", "1", "pr_opened", "--field", "repo=jarvis-server", "--field", "pr=142", "--field", "branch=fix/x", "--field", "head_sha="+sha, "--evidence", ev)
	if out := c.ok(runnerTok, "claim", "--agent", "builder"); !strings.Contains(out, "nothing to do") {
		t.Fatalf("builder got its own review: %q", out)
	}
	c.ok(runnerTok, "claim", "--agent", "reviewer")
	c.ok(runnerTok, "advance", "1", "approved", "--field", "summary=ok", "--field", "head_sha="+sha)
	c.ok(runnerTok, "claim", "--agent", "lander")
	if code, _, errs := c.run(runnerTok, "advance", "1", "merged", "--field", "merge_sha="+sha); code != 1 || !strings.Contains(errs, "approval_required") {
		t.Fatalf("merging a hold-risk item without approval: %d %q", code, errs)
	}
	c.ok(runnerTok, "advance", "1", "hold", "--field", "reason=risk is hold")
	if code, _, errs := c.run(userTok, "approve", "1", "--sha", "deadbeef"); code != 1 || !strings.Contains(errs, "sha_mismatch") {
		t.Fatalf("wrong-sha approve: %d %q", code, errs)
	}
	c.ok(userTok, "approve", "1", "--sha", sha, "--words", "fine")
	c.ok(runnerTok, "claim", "--agent", "lander")
	if out := c.ok(runnerTok, "advance", "1", "merged", "--field", "merge_sha="+sha); !strings.Contains(out, "#1 → done") {
		t.Fatalf("merged: %q", out)
	}

	out = c.ok(userTok, "show", "1")
	for _, want := range []string{"#1 wake word misses", "stage:    done", "approval: user:alex", "advanced", "pr_opened", "evidence", "repro.log"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}

	// The throttle.
	c.ok(userTok, "add", "another")
	if out := c.ok(userTok, "agents", "0"); !strings.Contains(out, "active agents: 0") {
		t.Fatalf("agents 0: %q", out)
	}
	if out := c.ok(runnerTok, "claim", "--agent", "a"); !strings.Contains(out, "nothing to do") {
		t.Fatalf("claim at 0: %q", out)
	}
	c.ok(userTok, "agents", "2")
	c.ok(userTok, "stop")
	if out := c.ok(runnerTok, "claim", "--agent", "a"); !strings.Contains(out, "nothing to do") {
		t.Fatalf("claim while stopped: %q", out)
	}
	if out := c.ok(userTok, "resume"); !strings.Contains(out, "running") {
		t.Fatalf("resume: %q", out)
	}
	c.ok(runnerTok, "claim", "--agent", "a")
	if out := c.ok(runnerTok, "advance", "2", "question", "--field", "question=which repo?"); !strings.Contains(out, "needs_you") {
		t.Fatalf("question: %q", out)
	}
	if out := c.ok(userTok, "answer", "2", "jarvis-server"); !strings.Contains(out, "#2 → triage") {
		t.Fatalf("answer: %q", out)
	}

	// Errors.
	if code, _, errs := c.run("", "status"); code != 2 || !strings.Contains(errs, "LOOP_TOKEN") {
		t.Fatalf("no token: %d %q", code, errs)
	}
	if code, _, errs := c.run("wrong-token-0123456789", "status"); code != 1 || !strings.Contains(errs, "unauthorized") {
		t.Fatalf("bad token: %d %q", code, errs)
	}
	if code, _, _ := c.run(userTok, "frobnicate"); code != 2 {
		t.Fatalf("unknown command: %d", code)
	}
	if code, _, _ := c.run(userTok, "add", "x", "--bogus", "1"); code != 2 {
		t.Fatalf("unknown flag: %d", code)
	}
	if code, _, _ := c.run(runnerTok, "claim", "--runner", "mac", "--agent", "a"); code != 1 {
		t.Fatalf("claiming as another runner: %d", code)
	}
}
