package main

import (
	"bytes"
	"fmt"
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
		"--field", "risk=normal", "--field", "resources=", "--evidence", ev)
	if !strings.Contains(out, "evidence 1: repro.log") || !strings.Contains(out, "#1 → build") {
		t.Fatalf("advance: %q", out)
	}
	c.ok(runnerTok, "claim", "--agent", "builder")
	// The diff, as `git diff --name-status main...HEAD` prints it: deleting a test puts it on the hold list.
	diff := filepath.Join(t.TempDir(), "diff.txt")
	os.WriteFile(diff, []byte("M\tinternal/y.go\nD\tinternal/x_test.go\n"), 0o600)
	if code, _, errs := c.run(runnerTok, "advance", "1", "pr_opened", "--field", "repo=jarvis-server", "--field", "pr=142", "--field", "branch=fix/x",
		"--field", "head_sha="+sha, "--evidence", ev); code != 2 || !strings.Contains(errs, "changed paths") {
		t.Fatalf("pr_opened without changed paths: %d %q", code, errs)
	}
	if n := len(evidenceOf(t, c, 1)); n != 1 {
		t.Fatalf("a refused advance uploaded its evidence anyway: %d files", n)
	}
	c.ok(runnerTok, "advance", "1", "pr_opened", "--field", "repo=jarvis-server", "--field", "pr=142", "--field", "branch=fix/x", "--field", "head_sha="+sha,
		"--paths-file", diff, "--evidence", ev)
	if out := c.ok(runnerTok, "claim", "--agent", "builder"); !strings.Contains(out, "nothing to do") {
		t.Fatalf("builder got its own review: %q", out)
	}
	c.ok(runnerTok, "claim", "--agent", "reviewer")
	c.ok(runnerTok, "advance", "1", "approved", "--field", "summary=ok", "--field", "head_sha="+sha)
	c.ok(runnerTok, "claim", "--agent", "lander")
	if code, _, errs := c.run(runnerTok, "advance", "1", "merged", "--field", "merge_sha="+sha, "--field", "head_sha="+sha, "--path", "M internal/y.go"); code != 1 || !strings.Contains(errs, "approval_required") {
		t.Fatalf("merging a hold-risk item without approval: %d %q", code, errs)
	}
	c.ok(runnerTok, "advance", "1", "hold", "--field", "reason=on the hold list", "--field", "head_sha="+sha, "--paths-file", diff)
	if code, _, errs := c.run(userTok, "approve", "1", "--sha", "deadbeef"); code != 1 || !strings.Contains(errs, "sha_mismatch") {
		t.Fatalf("wrong-sha approve: %d %q", code, errs)
	}
	c.ok(userTok, "approve", "1", "--sha", sha, "--words", "fine")
	c.ok(runnerTok, "claim", "--agent", "lander")
	if out := c.ok(runnerTok, "advance", "1", "merged", "--field", "merge_sha="+sha, "--field", "head_sha="+sha, "--paths-file", diff); !strings.Contains(out, "#1 → done") {
		t.Fatalf("merged: %q", out)
	}

	out = c.ok(userTok, "show", "1")
	for _, want := range []string{"#1 wake word misses", "stage:    done", "repo: jarvis-server\n", "approval: user:alex", "triage:   area voice, size S, resources []", "risk:     hold (deleted-test: D internal/x_test.go; land: on the hold list)", "advanced", "pr_opened", "evidence", "repro.log"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}

	// The throttle.
	c.ok(userTok, "add", "another")
	if out := c.ok(userTok, "show", "2"); !strings.Contains(out, "repo: not set, triage will pick\n") {
		t.Errorf("show without a repo:\n%s", out)
	}
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

func evidenceOf(t *testing.T, c cli, id int) []string {
	t.Helper()
	out := c.ok(userTok, "show", fmt.Sprint(id))
	_, ev, _ := strings.Cut(out, "\nevidence:\n")
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(ev), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestCLIDrop(t *testing.T) {
	st, err := store.Open(t.TempDir(), clock.NewFake(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	toks, _ := api.ParseTokens(strings.NewReader(userTok + "\tuser\talex\n" + runnerTok + "\trunner\tdevbox\n"))
	srv := httptest.NewServer(api.New(st, toks, slog.New(slog.DiscardHandler)))
	defer srv.Close()
	c := cli{t: t, url: srv.URL}

	c.ok(userTok, "add", "device-kasa", "--repo", "jarvis-server")
	if code, _, errs := c.run(runnerTok, "drop", "1"); code != 1 || !strings.Contains(errs, "forbidden") {
		t.Fatalf("drop with a runner token: %d %q", code, errs)
	}
	if out := c.ok(userTok, "drop", "1", "--words", "no hardware"); !strings.Contains(out, "#1 dropped") {
		t.Fatalf("drop: %q", out)
	}
	if out := c.ok(userTok, "show", "1"); !strings.Contains(out, "stage:    closed") || !strings.Contains(out, "no hardware") {
		t.Fatalf("show after drop: %q", out)
	}
	if code, _, errs := c.run(userTok, "drop", "1"); code != 1 || !strings.Contains(errs, "wrong_stage") {
		t.Fatalf("second drop: %d %q", code, errs)
	}
}

func TestCLIQA(t *testing.T) {
	st, err := store.Open(t.TempDir(), clock.NewFake(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	toks, _ := api.ParseTokens(strings.NewReader(userTok + "\tuser\talex\n" + runnerTok + "\trunner\tdevbox\n"))
	srv := httptest.NewServer(api.New(st, toks, slog.New(slog.DiscardHandler)))
	defer srv.Close()
	c := cli{t: t, url: srv.URL}

	c.ok(userTok, "add", "bigger pairing code", "--repo", "jarvis-node-mobile")
	if code, _, errs := c.run(runnerTok, "qa", "1"); code != 1 || !strings.Contains(errs, "forbidden") {
		t.Fatalf("qa with a runner token: %d %q", code, errs)
	}
	if out := c.ok(userTok, "qa", "1"); !strings.Contains(out, "#1 waits in qa") {
		t.Fatalf("qa: %q", out)
	}
	if out := c.ok(userTok, "show", "1"); !strings.Contains(out, "qa:       waits for your check") {
		t.Fatalf("show after qa: %q", out)
	}
	// Verified and reopen only from qa.
	if code, _, errs := c.run(userTok, "verified", "1"); code != 1 || !strings.Contains(errs, "wrong_stage") {
		t.Fatalf("verified in triage: %d %q", code, errs)
	}
	if code, _, errs := c.run(userTok, "reopen", "1", "broken"); code != 1 || !strings.Contains(errs, "wrong_stage") {
		t.Fatalf("reopen in triage: %d %q", code, errs)
	}
}
