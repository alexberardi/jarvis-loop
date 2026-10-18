package main

import (
	"bytes"
	"encoding/json"
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
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
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

func TestCLIAfter(t *testing.T) {
	st, err := store.Open(t.TempDir(), clock.NewFake(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	toks, _ := api.ParseTokens(strings.NewReader(userTok + "\tuser\talex\n" + runnerTok + "\trunner\tdevbox\n"))
	srv := httptest.NewServer(api.New(st, toks, slog.New(slog.DiscardHandler)))
	defer srv.Close()
	c := cli{t: t, url: srv.URL}

	c.ok(userTok, "add", "a")
	c.ok(userTok, "add", "b")
	if out := c.ok(userTok, "add", "c", "--after", "1, #2"); !strings.Contains(out, "added #3 c (P3), after #1, #2") {
		t.Fatalf("add --after: %q", out)
	}
	if out := c.ok(userTok, "show", "3"); !strings.Contains(out, "blocked:  waiting on #1, #2") {
		t.Fatalf("show: %q", out)
	}
	if out := c.ok(userTok, "show", "1"); strings.Contains(out, "waiting on") {
		t.Fatalf("show without dependencies: %q", out)
	}
	if code, _, errs := c.run(userTok, "add", "d", "--after", "9"); code != 1 || !strings.Contains(errs, "no item #9") {
		t.Fatalf("unknown dependency: %d %q", code, errs)
	}
	if code, _, errs := c.run(userTok, "add", "d", "--after", "x"); code != 2 || !strings.Contains(errs, "not an item id") {
		t.Fatalf("bad --after: %d %q", code, errs)
	}
}

func TestCLIBudgetAndUsage(t *testing.T) {
	st, err := store.Open(t.TempDir(), clock.NewFake(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	toks, _ := api.ParseTokens(strings.NewReader(userTok + "\tuser\talex\n" + runnerTok + "\trunner\tdevbox\n"))
	srv := httptest.NewServer(api.New(st, toks, slog.New(slog.DiscardHandler)))
	defer srv.Close()
	c := cli{t: t, url: srv.URL}

	if out := c.ok(userTok, "budget"); out != "claude budget: per 5h 100% of the last limit · per week 100% of the last limit · pacing from 85%\n" {
		t.Fatalf("budget: %q", out)
	}
	if out := c.ok(userTok, "budget", "claude", "--per-5h", "2M", "--per-week", "80%", "--pace-at", "90"); out != "claude budget: per 5h 2.0M · per week 80% of the last limit · pacing from 90%\n" {
		t.Fatalf("budget set: %q", out)
	}
	if out := c.ok(userTok, "budget", "--per-5h", "off"); !strings.Contains(out, "per 5h none · per week 80%") {
		t.Fatalf("budget off: %q", out)
	}
	if code, _, errs := c.run(userTok, "budget", "--per-5h", "lots"); code != 2 || !strings.Contains(errs, "tokens") {
		t.Fatalf("bad budget: %d %q", code, errs)
	}
	if code, _, errs := c.run(userTok, "budget", "--per-week", "150%"); code != 1 || !strings.Contains(errs, "0 to 100") {
		t.Fatalf("budget 150%%: %d %q", code, errs)
	}
	out := c.ok(userTok, "usage")
	for _, want := range []string{"claude 5h window: none open", "claude week: 0 (no limit known yet), rolling 7 days", "pace: normal", "budget: per 5h none"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage lacks %q:\n%s", want, out)
		}
	}
}

func TestUsageLines(t *testing.T) {
	now := time.Date(2026, 10, 18, 20, 0, 0, 0, time.UTC)
	fc := now.Add(40 * time.Minute)
	u := usageReport{
		Claude: pipeline.UsageState{Now: now, WindowStart: now.Add(-2 * time.Hour), WindowEnd: now.Add(3 * time.Hour), Burn5h: 8_700_000, Limit5h: 10_000_000,
			WeekStart: now.Add(-3 * 24 * time.Hour), WeekEnd: now.Add(4 * 24 * time.Hour), BurnWeek: 40_000_000},
		Pace:     pipeline.Pace{Level: pipeline.PaceSlow, Agents: 3, Window: "5h", Percent: 87},
		Budget:   pipeline.Budget{Per5hPct: 100, PerWeekPct: 100},
		Forecast: &fc,
		Hits:     []pipeline.UsageHit{{At: now.Add(-6 * time.Hour), Window: "5h", Burn: 10_000_000, ResetAt: now.Add(-5 * time.Hour)}},
	}
	u.Items = append(u.Items, struct {
		ItemID  int64   `json:"item_id"`
		Title   string  `json:"title"`
		Burn    int64   `json:"burn"`
		CostUSD float64 `json:"cost_usd"`
		Runs    int     `json:"runs"`
	}{12, "the big one", 3_100_000, 4.1, 5})
	got := strings.Join(usageLines(u, time.UTC), "\n")
	want := strings.Join([]string{
		"claude 5h window: 8.7M of 10.0M (87%), Sun 18:00–23:00 · at this rate it runs out at 20:40",
		"claude week: 40.0M (no limit known yet), resets Thu 20:00",
		"pace: pacing: the 5h window is at 87% of its budget; 3 at once",
		"budget: per 5h 100% of the last limit · per week 100% of the last limit · pacing from 85%",
		"costliest items (7 days):",
		"  #12 the big one  burn 3.1M  $4.10  5 runs",
		"limits hit:",
		"  Sun 14:00 5h limit at burn 10.0M, reset Sun 15:00",
	}, "\n")
	if got != want {
		t.Fatalf("usage lines:\n%s\nwant:\n%s", got, want)
	}
}

func testLoopd(t *testing.T) cli {
	t.Helper()
	st, err := store.Open(t.TempDir(), clock.NewFake(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	toks, _ := api.ParseTokens(strings.NewReader(userTok + "\tuser\talex\n" + runnerTok + "\trunner\tdevbox\n"))
	srv := httptest.NewServer(api.New(st, toks, slog.New(slog.DiscardHandler)))
	t.Cleanup(srv.Close)
	return cli{t: t, url: srv.URL}
}

// evidenceFile is a file to attach where an advance needs evidence.
func evidenceFile(t *testing.T) string {
	t.Helper()
	ev := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(ev, []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestCLIFlagsAnywhere(t *testing.T) {
	c := testLoopd(t)
	if out := c.ok(userTok, "add", "after the title", "--priority", "P2", "--ask", "x"); !strings.Contains(out, "added #1 after the title (P2)") {
		t.Fatalf("flags after the title: %q", out)
	}
	if out := c.ok(userTok, "add", "--repo", "jarvis-server", "quiet one", "-q"); out != "2\n" {
		t.Fatalf("add -q: %q", out)
	}
	if out := c.ok(userTok, "add", "--quiet", "quiet two"); out != "3\n" {
		t.Fatalf("add --quiet: %q", out)
	}
	out := c.ok(userTok, "add", "as json", "--json", "--priority", "P4")
	var it item
	if err := json.Unmarshal([]byte(out), &it); err != nil || it.ID != 4 || it.Priority != "P4" || !strings.HasSuffix(out, "}\n") {
		t.Fatalf("add --json: %q %v", out, err)
	}
	for _, x := range []struct {
		args []string
		want string
	}{
		{[]string{"add", "t", "-p", "P2"}, "unknown flag -p (flags are --long"},
		{[]string{"add", "title", "with", "spaces"}, `add takes 1 argument, got 3 ("title" "with" "spaces"): quote text with spaces`},
		{[]string{"add", "t", "--json=yes"}, "--json takes no value"},
		{[]string{"add", "t", "--json", "-q"}, "--json or --quiet, not both"},
		{[]string{"show", "1", "-q"}, "unknown flag -q"},
		{[]string{"rebuild", "1"}, "rebuild needs notes"},
		{[]string{"link", "1"}, "link needs --after"},
		{[]string{"edit", "1"}, "edit needs --title or --ask"},
	} {
		code, _, errs := c.run(userTok, x.args...)
		if code != 2 || !strings.Contains(errs, x.want) {
			t.Errorf("loop %s: %d %q, want %q", strings.Join(x.args, " "), code, errs, x.want)
		}
	}
	// Text that starts with a dash goes after --.
	if out := c.ok(userTok, "add", "-q", "--", "-v is broken"); out != "5\n" {
		t.Fatalf("add -- -v: %q", out)
	}
}

func TestCLIEditLinkResources(t *testing.T) {
	c := testLoopd(t)
	c.ok(userTok, "add", "wrong tittle", "--repo", "jarvis-server")
	c.ok(userTok, "add", "b")
	c.ok(userTok, "add", "c")

	// Edit, before build.
	if out := c.ok(userTok, "edit", "1", "--title", "right title", "--ask", "the real ask"); !strings.Contains(out, "#1 edited: right title") {
		t.Fatalf("edit: %q", out)
	}
	out := c.ok(userTok, "show", "1")
	if !strings.Contains(out, "#1 right title") || !strings.Contains(out, "ask:      the real ask") || !strings.Contains(out, "edited") || !strings.Contains(out, `"from":"wrong tittle"`) {
		t.Fatalf("show after edit: %q", out)
	}

	// Link and unlink.
	if out := c.ok(userTok, "link", "3", "--after", "1,#2"); !strings.Contains(out, "#3 waits on #1, #2") {
		t.Fatalf("link: %q", out)
	}
	if out := c.ok(userTok, "show", "3"); !strings.Contains(out, "blocked:  waiting on #1, #2") {
		t.Fatalf("show after link: %q", out)
	}
	for _, x := range []struct {
		args []string
		want string
	}{
		{[]string{"link", "3", "--after", "3"}, "cannot wait for itself"},
		{[]string{"link", "1", "--after", "3"}, "invalid_field"}, // a cycle
		{[]string{"link", "3", "--after", "9"}, "no item #9"},
		{[]string{"unlink", "3", "9"}, "#3 does not wait for #9"},
	} {
		code, _, errs := c.run(userTok, x.args...)
		if code != 1 || !strings.Contains(errs, x.want) {
			t.Errorf("loop %s: %d %q, want %q", strings.Join(x.args, " "), code, errs, x.want)
		}
	}
	if out := c.ok(userTok, "unlink", "3", "1"); !strings.Contains(out, "#3 waits on #2") {
		t.Fatalf("unlink: %q", out)
	}
	if out := c.ok(userTok, "unlink", "#3", "#2"); !strings.Contains(out, "#3 waits on nothing") {
		t.Fatalf("unlink the last: %q", out)
	}

	// The user's token only.
	for _, args := range [][]string{{"edit", "1", "--ask", "x"}, {"link", "3", "--after", "1"}, {"unlink", "3", "1"}, {"resources", "1", "voice"}} {
		if code, _, errs := c.run(runnerTok, args...); code != 1 || !strings.Contains(errs, "forbidden") {
			t.Errorf("loop %s with a runner token: %d %q", strings.Join(args, " "), code, errs)
		}
	}

	// Resources, after triage; the title and ask no longer change.
	if code, _, errs := c.run(userTok, "resources", "1", "voice"); code != 1 || !strings.Contains(errs, "wrong_stage") {
		t.Fatalf("resources in triage: %d %q", code, errs)
	}
	c.ok(runnerTok, "claim", "--agent", "triager")
	c.ok(runnerTok, "advance", "1", "reproduced", "--field", "area=a", "--field", "size=S", "--field", "risk=normal", "--field", "resources=macos", "--evidence", evidenceFile(t))
	if code, _, errs := c.run(userTok, "edit", "1", "--ask", "late"); code != 1 || !strings.Contains(errs, "before build starts") {
		t.Fatalf("edit in build: %d %q", code, errs)
	}
	if code, _, errs := c.run(userTok, "resources", "1", "macos,pi"); code != 1 || !strings.Contains(errs, `unknown resource "pi"`) {
		t.Fatalf("unknown resource: %d %q", code, errs)
	}
	if out := c.ok(userTok, "resources", "1", "voice,gpu"); !strings.Contains(out, "#1 needs gpu, voice") {
		t.Fatalf("resources: %q", out)
	}
	if out := c.ok(userTok, "show", "1"); !strings.Contains(out, "resources [gpu,voice]") || !strings.Contains(out, `from=["macos"]`) {
		t.Fatalf("show after resources: %q", out)
	}
	if out := c.ok(userTok, "resources", "1", "none"); !strings.Contains(out, "#1 needs no resources") {
		t.Fatalf("resources none: %q", out)
	}
}

func TestCLIRebuild(t *testing.T) {
	c := testLoopd(t)
	c.ok(userTok, "add", "x", "--repo", "jarvis-server")
	c.ok(runnerTok, "claim", "--agent", "triager")
	ev := evidenceFile(t)
	c.ok(runnerTok, "advance", "1", "reproduced", "--field", "area=a", "--field", "size=S", "--field", "risk=normal", "--field", "resources=", "--evidence", ev)

	// needs_you, on the build's question.
	c.ok(runnerTok, "claim", "--agent", "builder")
	c.ok(runnerTok, "advance", "1", "question", "--field", "question=which port?")
	if out := c.ok(userTok, "rebuild", "1", "--notes", "use 7781"); !strings.Contains(out, "#1 → build") {
		t.Fatalf("rebuild from needs_you: %q", out)
	}

	// held, with the notes as an argument.
	c.ok(runnerTok, "claim", "--agent", "builder")
	c.ok(runnerTok, "advance", "1", "pr_opened", "--field", "repo=jarvis-server", "--field", "pr=7", "--field", "branch=b", "--field", "head_sha="+sha, "--path", "M x.go", "--evidence", ev)
	c.ok(runnerTok, "claim", "--agent", "reviewer")
	c.ok(runnerTok, "advance", "1", "approved", "--field", "summary=ok", "--field", "head_sha="+sha)
	c.ok(runnerTok, "claim", "--agent", "lander")
	c.ok(runnerTok, "advance", "1", "hold", "--field", "reason=r", "--field", "head_sha="+sha, "--path", "M x.go")
	if out := c.ok(userTok, "rebuild", "1", "rebase it on main"); !strings.Contains(out, "#1 → build") {
		t.Fatalf("rebuild from held: %q", out)
	}
	if out := c.ok(userTok, "show", "1"); !strings.Contains(out, `notes="rebase it on main"`) || !strings.Contains(out, `notes="use 7781"`) {
		t.Fatalf("show after rebuilds: %q", out)
	}
	if code, _, errs := c.run(userTok, "rebuild", "1", "again"); code != 1 || !strings.Contains(errs, "wrong_stage") {
		t.Fatalf("rebuild in build: %d %q", code, errs)
	}
}
