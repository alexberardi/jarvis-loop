package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fake gh on PATH logs its argv and answers like gh does.
func fakeGH(t *testing.T, script string) (GH, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\n" + script
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return GH{}, log
}

func TestGH(t *testing.T) {
	ctx := context.Background()
	gh, log := fakeGH(t, `case "$1 $2" in
"pr list") echo '[{"number":12,"headRefName":"loop/1-xy"},{"number":5,"headRefName":"loop/1-x"}]';;
"pr create") echo 'https://github.com/o/r/pull/42';;
"pr edit") echo 'https://github.com/o/r/pull/5';;
esac
`)
	if n, err := gh.Find(ctx, "o/r", "loop/1-x"); err != nil || n != 5 {
		t.Fatalf("find %d %v", n, err)
	}
	if n, err := gh.Find(ctx, "o/r", "loop/2-y"); err != nil || n != 0 {
		t.Fatalf("find none %d %v", n, err)
	}
	spec := PRSpec{Slug: "o/r", Head: "loop/1-x", Base: "main", Title: "Fix it; `rm -rf /`", BodyFile: "/run/body.md"}
	if n, err := gh.Create(ctx, spec); err != nil || n != 42 {
		t.Fatalf("create %d %v", n, err)
	}
	if err := gh.Edit(ctx, 5, spec); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	want := []string{
		"pr list --repo o/r --head loop/1-x --state open --json number,headRefName",
		"pr list --repo o/r --head loop/2-y --state open --json number,headRefName",
		"pr create --repo o/r --head loop/1-x --base main --title Fix it; `rm -rf /` --body-file /run/body.md",
		"pr edit 5 --repo o/r --title Fix it; `rm -rf /` --body-file /run/body.md",
	}
	if got := strings.Split(strings.TrimSpace(string(b)), "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("gh argv:\n%s", b)
	}
}

func TestGHFailures(t *testing.T) {
	ctx := context.Background()
	gh, _ := fakeGH(t, `case "$1 $2" in
"pr create") echo 'Warning: 2 uncommitted changes';;
*) echo 'HTTP 401: Bad credentials' >&2; exit 1;;
esac
`)
	if _, err := gh.Create(ctx, PRSpec{Slug: "o/r", Head: "loop/1-x", Base: "main", Title: "t", BodyFile: "b"}); err == nil || !strings.Contains(err.Error(), "no PR URL") {
		t.Fatalf("create without a URL: %v", err)
	}
	if _, err := gh.Find(ctx, "o/r", "loop/1-x"); err == nil || !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("find: %v", err)
	}
}

func TestPRBody(t *testing.T) {
	ev := []EvidenceFile{{Name: "after.png"}, {Name: "test.log"}}
	b := PRBody("## What\n\nFixed.\n\n", 12, ev, map[string]string{"after.png": "after,\n phone"},
		RunStats{Turns: 41, CacheHit: 0.973, Seconds: 754, CostUSD: 3.214, HasResult: true}, nil)
	want := "## What\n\nFixed.\n\n---\n\n### Verification\n\nEvidence files (on loop item #12):\n\n- `after.png`: after, phone\n- `test.log`\n\n" +
		"Run: 41 turns, prompt-cache hit 97%, 12m 34s, about $3.21 (estimate).\n\n*Built by a jarvis-loop agent; pushed and opened by the runner.*\n"
	if b != want {
		t.Fatalf("got:\n%s", b)
	}
	if b := PRBody("x", 1, nil, nil, RunStats{Seconds: 9}, nil); !strings.Contains(b, "No evidence files.") || !strings.Contains(b, "Run: 9s (no usage numbers).") {
		t.Fatalf("got:\n%s", b)
	}
}

// The runner's verify steps are listed apart from the agent's evidence, before it, with each
// step's result and files; the trailer stays last (land checks it).
func TestPRBodyVerify(t *testing.T) {
	vs := []VerifyResult{
		{Name: "unit tests", Seconds: 62, LogFile: EvidenceFile{Name: "verify-1-unit-tests.log"}},
		{Name: "admin screenshots", Skipped: "no changed path matches web/**"},
		{Name: "mobile shots", Seconds: 300, LogFile: EvidenceFile{Name: "verify-3-mobile-shots.log"},
			Evidence: []EvidenceFile{{Name: "verify-3-mobile-shots-home-light.png"}, {Name: "verify-3-mobile-shots-index.json"}}},
	}
	b := PRBody("body", 4, []EvidenceFile{{Name: "after.txt"}}, nil, RunStats{Seconds: 5}, vs)
	want := "**Runner verify** (run by the runner after the agent, outside its sandbox, from the runner's config):\n\n" +
		"- unit tests: passed (1m 02s); `verify-1-unit-tests.log`\n" +
		"- admin screenshots: skipped (no changed path matches web/**)\n" +
		"- mobile shots: passed (5m 00s); `verify-3-mobile-shots.log`, `verify-3-mobile-shots-home-light.png`, `verify-3-mobile-shots-index.json`\n\n" +
		"Evidence files (on loop item #4):\n\n- `after.txt`\n"
	if !strings.Contains(b, want) || !strings.HasSuffix(strings.TrimSpace(b), VerificationTrailer) {
		t.Fatalf("got:\n%s", b)
	}
}

// The reads triage and a bounced build need, and the review's comment: a comment, never an
// approval.
func TestGHReadsAndComment(t *testing.T) {
	ctx := context.Background()
	gh, log := fakeGH(t, `case "$*" in
"pr view 7 --repo a/b --json comments,reviews") echo '{"comments":[{"author":{"login":"alex"},"body":"## Loop review: changes requested","createdAt":"2026-10-15T12:01:00Z"}],
 "reviews":[{"author":{"login":"alex"},"body":"","state":"COMMENTED","submittedAt":"2026-10-15T12:00:00Z"},
            {"author":{"login":"bob"},"body":"","state":"CHANGES_REQUESTED","submittedAt":"2026-10-15T12:03:00Z"}]}';;
"api --method GET repos/a/b/pulls/7/comments?per_page=100") echo '[{"user":{"login":"alex"},"body":"name it","path":"x.go","line":4,"created_at":"2026-10-15T12:02:00Z"}]';;
"pr list --repo a/b --state merged --limit 30 --json number,title,mergedAt,files") echo '[{"number":3,"title":"Add x","mergedAt":"2026-10-14T10:00:00Z","files":[{"path":"a.go"},{"path":"b.go"}]}]';;
"pr view 7 --repo a/b --json number,title,body,headRefName,headRefOid,baseRefName,state") echo '{"number":7,"title":"T","body":"B","headRefName":"loop/1-x","headRefOid":"abc","baseRefName":"main","state":"OPEN"}';;
"pr comment 7 --repo a/b --body-file /run/review.md") ;;
*) echo "unexpected: $*" >&2; exit 1;;
esac
`)
	cs, err := gh.Comments(ctx, "a/b", 7)
	if err != nil {
		t.Fatal(err)
	}
	// The empty COMMENTED review is left out; oldest first.
	if len(cs) != 3 || cs[0].Body != "## Loop review: changes requested" || cs[1].Path != "x.go" || cs[1].Line != 4 || cs[2].State != "CHANGES_REQUESTED" {
		t.Fatalf("comments %+v", cs)
	}
	m, err := gh.MergedPRs(ctx, "a/b", 30)
	if err != nil || len(m) != 1 || m[0].Number != 3 || strings.Join(m[0].Files, ",") != "a.go,b.go" {
		t.Fatalf("merged %+v %v", m, err)
	}
	v, err := gh.View(ctx, "a/b", 7)
	if err != nil || v.HeadRefOid != "abc" || v.Body != "B" {
		t.Fatalf("view %+v %v", v, err)
	}
	if err := gh.Comment(ctx, "a/b", 7, "/run/review.md"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "pr comment 7 --repo a/b --body-file /run/review.md") ||
		strings.Contains(string(b), "pr review") || strings.Contains(string(b), "approve") {
		t.Fatalf("gh calls:\n%s", b)
	}
}
