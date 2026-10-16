package runner

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

func TestExtractCIFailureGo(t *testing.T) {
	raw, err := os.ReadFile("testdata/gh/log_failed_go.txt")
	if err != nil {
		t.Fatal(err)
	}
	f := ExtractCIFailure(string(raw))
	if want := []string{"TestWatchMain", "TestScenarios", "TestScenarios/main-red"}; !slices.Equal(f.Tests, want) {
		t.Fatalf("tests %q, want %q", f.Tests, want)
	}
	for _, want := range []string{
		"==== job: check ====\n",
		"WARNING: DATA RACE\n", // the race report before the failure is kept
		"--- FAIL: TestWatchMain (0.21s)\n",
		"land_test.go:410: main_ci event missing; GH_TOKEN=[redacted] Authorization: Bearer [redacted]",
		"--- FAIL: TestScenarios/main-red (1.20s)",
		"commit 0123456789abcdef0123456789abcdef01234567", // a sha stays
		"##[error]Process completed with exit code 1.",
		"\n...\n",
	} {
		if !strings.Contains(f.Text, want) {
			t.Errorf("excerpt lacks %q", want)
		}
	}
	for _, bad := range []string{"\x1b", "ghp_", "abc.def-ghi_jkl123", "\tcheck\t", "2026-10-16T12:", "internal/pkg0\t", "internal/pkg55\t"} {
		if strings.Contains(f.Text, bad) {
			t.Errorf("excerpt has %q", bad)
		}
	}
	if n := strings.Count(f.Text, "\n"); n > 2*(ciBefore+ciAfter)+10 {
		t.Errorf("excerpt is %d lines", n)
	}
}

func TestExtractCIFailureScenario(t *testing.T) {
	raw, err := os.ReadFile("testdata/gh/log_failed_scenario.txt")
	if err != nil {
		t.Fatal(err)
	}
	f := ExtractCIFailure(string(raw))
	if want := []string{"main-red", "ci-red-unchanged"}; !slices.Equal(f.Tests, want) {
		t.Fatalf("tests %q, want %q", f.Tests, want)
	}
	// The lint job names no test: its tail is kept.
	for _, want := range []string{"==== job: check ====\n", "no message matching", "==== job: lint ====\nnot gofmt'd:\ninternal/runner/cilog.go\n"} {
		if !strings.Contains(f.Text, want) {
			t.Errorf("excerpt lacks %q:\n%s", want, f.Text)
		}
	}
}

// No failure recognized: the tail, said so; a huge log is capped.
func TestExtractCIFailureTail(t *testing.T) {
	var b strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&b, "build\tmake\t2026-10-16T12:00:00.0000000Z \x1b[33mline %d\x1b[0m\n", i)
	}
	f := ExtractCIFailure(b.String())
	if len(f.Tests) != 0 || !strings.Contains(f.Text, "the last 200 of 1000 lines") || strings.Contains(f.Text, "line 799\n") ||
		!strings.Contains(f.Text, "line 800\nline 801") || !strings.HasSuffix(f.Text, "line 999\n") {
		t.Fatalf("tail:\n%s", clip(f.Text, 400))
	}
	big := strings.Repeat("--- FAIL: TestBig\n"+strings.Repeat("x", 2000)+"\n", 2000)
	if f := ExtractCIFailure(big); len(f.Text) > ciMaxBytes+100 || !strings.HasSuffix(f.Text, "KB)\n") {
		t.Fatalf("not capped: %d bytes", len(f.Text))
	}
}

func TestRedactTokens(t *testing.T) {
	for in, want := range map[string]string{
		"token ghp_0123456789abcdefABCDEF here":                 "token [redacted] here",
		"github_pat_11ABCDEFG0123456789_abcdefghijklmnop":       "[redacted]",
		"SLACK xoxb-1234-5678-abcdefgh":                         "SLACK [redacted]",
		"Authorization: bearer eyJhbGciOiJIUzI1NiJ9.x.y":        "Authorization: bearer [redacted]",
		"LOOP_TOKEN=s3cr3t-value":                               "LOOP_TOKEN=[redacted]",
		"(api_key: abc123, next)":                               "(api_key: [redacted], next)",
		`password: "hunter2"`:                                   "password: [redacted]",
		"blob aGVsbG8gV29ybGQgdGhpcyBpcyBhIHNlY3JldCBrZXkx":     "blob [redacted]",
		"sha 0123456789abcdef0123456789abcdef01234567 ok":       "sha 0123456789abcdef0123456789abcdef01234567 ok",
		"--- FAIL: TestScenarios/ci-red-unchanged (0.90s)":      "--- FAIL: TestScenarios/ci-red-unchanged (0.90s)",
		"github.com/alexberardi/jarvis-loop/internal/runner ok": "github.com/alexberardi/jarvis-loop/internal/runner ok",
	} {
		if got := RedactTokens(in); got != want {
			t.Errorf("RedactTokens(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCIRuns(t *testing.T) {
	if id := RunIDFromURL("https://github.com/o/r/actions/runs/123456/job/789"); id != 123456 {
		t.Fatalf("run id %d", id)
	}
	if id := RunIDFromURL("https://ci/1"); id != 0 {
		t.Fatalf("run id %d", id)
	}
	runs := ciRunsFromChecks([]Check{
		{Name: "test", Bucket: BucketFail, URL: "https://github.com/o/r/actions/runs/7/job/1"},
		{Name: "lint", Bucket: BucketPass, URL: "https://github.com/o/r/actions/runs/7/job/2"},
		{Name: "vet", Bucket: BucketCancel, URL: "https://github.com/o/r/actions/runs/7/job/3"},
		{Name: "e2e", Bucket: BucketFail, URL: "https://github.com/o/r/actions/runs/8/job/4"},
		{Name: "external", Bucket: BucketFail, URL: "https://ci.example/1"},
	})
	if want := []CIRun{{7, "test, vet"}, {8, "e2e"}}; !slices.Equal(runs, want) {
		t.Fatalf("runs %+v, want %+v", runs, want)
	}
	got := failedRuns([]WorkflowRun{{ID: 1, Name: "ci", Conclusion: "failure"}, {ID: 2, Name: "docs", Conclusion: "success"},
		{ID: 3, Workflow: "release", Conclusion: "timed_out"}, {ID: 4, Name: "x", Conclusion: "cancelled"}, {Name: "noid", Conclusion: "failure"}})
	if want := []CIRun{{1, "ci"}, {3, "release"}}; !slices.Equal(got, want) {
		t.Fatalf("failed runs %+v, want %+v", got, want)
	}
}

// The runner reads the merge commit back only from a P1 it filed itself for the item's repo.
func TestMainRedCommit(t *testing.T) {
	ask := MainRedAsk("o/r", 25, 24, mergeM, []string{"https://github.com/o/r/actions/runs/1"})
	p1 := Item{Title: "main red after #25: ci", Ask: ask, Source: "finding"}
	if got := MainRedCommit(p1, "o/r"); got != mergeM {
		t.Fatalf("commit %q", got)
	}
	for name, it := range map[string]Item{
		"from a user":    {Title: p1.Title, Ask: ask, Source: "cli"},
		"another title":  {Title: "Fix the digest", Ask: ask, Source: "finding"},
		"a quoted ask":   {Title: p1.Title, Ask: "> " + ask, Source: "finding"},
		"no ask":         {Title: p1.Title, Source: "finding"},
		"short sha only": {Title: p1.Title, Ask: strings.ReplaceAll(ask, mergeM, short7(mergeM)), Source: "finding"},
	} {
		if got := MainRedCommit(it, "o/r"); got != "" {
			t.Errorf("%s: commit %q", name, got)
		}
	}
	if got := MainRedCommit(p1, "o/other"); got != "" {
		t.Errorf("another repo: commit %q", got)
	}
}

// Red CI on the PR: land attaches the failing jobs' log (ci-failure.log) under its own lease,
// before it sends the item back to build; the build's brief points at it.
func TestLandAttachesTheCILog(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id, gh := g.toLand("Add a feature")
	raw, _ := os.ReadFile("testdata/gh/log_failed_go.txt")
	gh.mu.Lock()
	gh.pr.Checks = []Check{
		{Name: "check", Bucket: BucketFail, URL: "https://github.com/alex/demo/actions/runs/77/job/1"},
		{Name: "lint", Bucket: BucketPass, URL: "https://github.com/alex/demo/actions/runs/77/job/2"},
	}
	gh.logs = map[int64]string{77: string(raw)}
	head := gh.pr.HeadRefOid
	gh.mu.Unlock()
	g.stages(StageLand)
	g.runOnce()

	d := g.item(id)
	evs := g.events(id, "advanced")
	back := evs[len(evs)-1]
	if d.Item.Stage != pipeline.Build || back.Outcome != "back_to_build" {
		t.Fatalf("stage %s, last advance %+v; log:\n%s", d.Item.Stage, back, g.logs.String())
	}
	if !slices.Equal(gh.logCalls, []int64{77}) {
		t.Fatalf("logs asked for: %v", gh.logCalls)
	}
	ev := d.Evidence[len(d.Evidence)-1]
	if ev.Name != CIFailureLog || ev.Label != "CI failure on "+short7(head)+": check" || ev.LeaseID != back.Data["lease"] {
		t.Fatalf("evidence %+v (back_to_build under lease %v); log:\n%s", ev, back.Data["lease"], g.logs.String())
	}
	_, f, err := g.st.OpenEvidence(g.ctx, id, ev.N)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(f)
	f.Close()
	for _, want := range []string{"CI failure on " + short7(head) + ": check\nCommit: " + head, "Failing tests: TestWatchMain, TestScenarios, TestScenarios/main-red\n",
		"######## check (run 77)\n", "--- FAIL: TestWatchMain"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("ci-failure.log lacks %q", want)
		}
	}
	if strings.Contains(string(body), "ghp_") {
		t.Errorf("ci-failure.log has a token")
	}

	g.stages(StageBuild)
	g.runOnce()
	brief := g.read(filepath.Join(g.lastRun(id, StageBuild), "brief.md"))
	want := fmt.Sprintf("- **CI failed; the runner fetched its log**: `.loop/triage/%d-ci-failure.log` (CI failure on %s: check)", ev.N, short7(head))
	if !strings.Contains(brief, want) || !strings.Contains(brief, "usually a race") {
		t.Fatalf("build brief lacks %q:\n%s", want, brief[strings.Index(brief, ItemHeading):])
	}
}

// A log that cannot be fetched never stops land: the item still goes back to build.
func TestLandCILogFailureDoesNotBlock(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id, gh := g.toLand("Add a feature")
	gh.mu.Lock()
	gh.pr.Checks = []Check{{Name: "check", Bucket: BucketFail, URL: "https://github.com/alex/demo/actions/runs/78"}}
	gh.mu.Unlock()
	g.stages(StageLand)
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Build || d.Evidence[len(d.Evidence)-1].Name == CIFailureLog || !strings.Contains(g.logs.String(), "run 78 not found") {
		t.Fatalf("stage %s, evidence %+v; log:\n%s", d.Item.Stage, d.Evidence, g.logs.String())
	}
}

// Main red: triage of the P1 the runner filed fetches main's failed runs on the merge commit,
// attaches their log and gives it to the agent; a second triage reuses the attached one.
func TestTriageOfMainRedAttachesTheCILog(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	g.stages(StageTriage)
	raw, _ := os.ReadFile("testdata/gh/log_failed_scenario.txt")
	gh := &fakeLand{runs: []WorkflowRun{
		{ID: 300, Name: "ci", Status: "completed", Conclusion: "failure"},
		{ID: 301, Name: "docs", Status: "completed", Conclusion: "success"},
	}, logs: map[int64]string{300: string(raw)}}
	g.r.Land = gh
	it, err := g.st.AddItem(g.ctx, "runner:devbox", store.NewItem{Title: "main red after #25: ci", Repo: "alex/demo", Source: "finding",
		Ask: MainRedAsk("alex/demo", 25, 24, mergeM, []string{"https://github.com/alex/demo/actions/runs/300"})})
	if err != nil {
		t.Fatal(err)
	}
	g.script(it.ID, StageTriage, fakeclaude.TriageQuestion, "")
	if !g.runOnce() {
		t.Fatal("nothing claimed")
	}
	d := g.item(it.ID)
	if len(d.Evidence) != 1 || d.Evidence[0].Name != CIFailureLog || d.Evidence[0].Label != "CI failure on 2222222: ci" {
		t.Fatalf("evidence %+v; log:\n%s", d.Evidence, g.logs.String())
	}
	brief := g.read(filepath.Join(g.lastRun(it.ID, StageTriage), "brief.md"))
	if want := "- **CI failed; the runner fetched its log**: `.loop/context/ci-failure.log` (CI failure on 2222222: ci)"; !strings.Contains(brief, want) {
		t.Fatalf("triage brief lacks %q:\n%s", want, brief[strings.Index(brief, ItemHeading):])
	}
	if !slices.Equal(gh.logCalls, []int64{300}) {
		t.Fatalf("logs asked for: %v", gh.logCalls)
	}

	// Answered, triaged again: the attached log is reused, nothing is fetched or attached again.
	if _, err := g.st.Answer(g.ctx, "slack:U1", it.ID, "go on"); err != nil {
		t.Fatal(err)
	}
	g.script(it.ID, StageTriage, fakeclaude.TriageQuestion, "")
	if !g.runOnce() {
		t.Fatal("nothing claimed")
	}
	brief = g.read(filepath.Join(g.lastRun(it.ID, StageTriage), "brief.md"))
	if d := g.item(it.ID); len(d.Evidence) != 1 || len(gh.logCalls) != 1 || !strings.Contains(brief, "`.loop/context/ci-failure.log` (CI failure on 2222222: ci)") {
		t.Fatalf("second triage: evidence %d, log calls %v; log:\n%s", len(d.Evidence), gh.logCalls, g.logs.String())
	}
}
