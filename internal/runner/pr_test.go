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
		RunStats{Turns: 41, CacheHit: 0.973, Seconds: 754, CostUSD: 3.214, HasResult: true})
	want := "## What\n\nFixed.\n\n---\n\n### Verification\n\nEvidence files (on loop item #12):\n\n- `after.png`: after, phone\n- `test.log`\n\n" +
		"Run: 41 turns, prompt-cache hit 97%, 12m 34s, about $3.21 (estimate).\n\n*Built by a jarvis-loop agent; pushed and opened by the runner.*\n"
	if b != want {
		t.Fatalf("got:\n%s", b)
	}
	if b := PRBody("x", 1, nil, nil, RunStats{Seconds: 9}); !strings.Contains(b, "No evidence files.") || !strings.Contains(b, "Run: 9s (no usage numbers).") {
		t.Fatalf("got:\n%s", b)
	}
}
