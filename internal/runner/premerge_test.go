package runner

import (
	"fmt"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeIn writes files (path: content) in dir and commits them.
func writeIn(t *testing.T, dir, msg string, files map[string]string) {
	t.Helper()
	for f, s := range files {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755)
		os.WriteFile(filepath.Join(dir, f), []byte(s), 0o644)
	}
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", msg)
}

// preMergeRig is a branch loop/1-x off main with its own app.txt and .claude/settings.json edit,
// then main moving on with mainFiles.
func preMergeRig(t *testing.T, mainFiles map[string]string) (gitRig, string) {
	r := newGitRig(t)
	writeIn(t, r.seed, "app and settings", map[string]string{"app.txt": "one\n", ".claude/settings.json": "{}\n"})
	gitT(t, r.seed, "push", "-q", r.remote, "main")
	r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote, Base: "main"})
	wt := filepath.Join(t.TempDir(), "wt")
	if _, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{}); err != nil {
		t.Fatal(err)
	}
	writeIn(t, wt, "the branch's change", map[string]string{"app.txt": "branch\n", ".claude/settings.json": `{"branch": 1}` + "\n"})
	writeIn(t, r.seed, "main moves on", mainFiles)
	gitT(t, r.seed, "push", "-q", r.remote, "main")
	r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote, Base: "main"})
	return r, wt
}

func lines(s string) []string {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// #349: main adds a skill under .claude/skills, changes the settings the branch changed too, and
// conflicts with the branch in app.txt: the runner merges it, the protected paths take main's
// side, and the agent sees only app.txt's conflict (and can finish the merge with .claude/skills
// read-only, as the sandbox keeps it).
func TestPreMergeLeavesOnlyUnprotectedConflicts(t *testing.T) {
	r, wt := preMergeRig(t, map[string]string{
		".claude/skills/x/SKILL.md": "skill\n", ".claude/settings.json": `{"main": 1}` + "\n", "app.txt": "main\n"})
	main := gitT(t, r.mirror, "rev-parse", "refs/remotes/origin/main")
	p, err := r.g.PreMerge(r.ctx, wt, "main", "loop/1-x")
	if err != nil {
		t.Fatal(err)
	}
	if p.SHA != main || p.Committed || !slices.Equal(p.Conflicts, []string{"app.txt"}) || !slices.Equal(p.Taken, []string{".claude/settings.json"}) {
		t.Fatalf("pre-merge %+v, want main %s, in progress, app.txt left, settings taken", p, main)
	}
	if got := lines(gitT(t, wt, "diff", "--name-only", "--diff-filter=U")); !slices.Equal(got, []string{"app.txt"}) {
		t.Fatalf("the agent sees conflicts in %q, want only app.txt", got)
	}
	if gitT(t, wt, "rev-parse", "-q", "--verify", "MERGE_HEAD") != main {
		t.Fatal("the merge is not in progress")
	}
	if d := gitT(t, wt, "diff", "--cached", "refs/remotes/origin/main", "--", ".claude/"); d != "" {
		t.Fatalf(".claude/ differs from main:\n%s", d)
	}
	if want := "runner merged origin/main at " + short7(main) + "; protected paths taken from main (.claude/settings.json); left in progress for the agent: app.txt"; p.Evidence("main") != want {
		t.Fatalf("evidence %q, want %q", p.Evidence("main"), want)
	}
	// The agent resolves app.txt and commits, with .claude/skills read-only.
	skills := filepath.Join(wt, ".claude", "skills")
	os.Chmod(skills, 0o555)
	defer os.Chmod(skills, 0o755)
	os.WriteFile(filepath.Join(wt, "app.txt"), []byte("both\n"), 0o644)
	gitT(t, wt, "add", "app.txt")
	gitT(t, wt, "commit", "-q", "--no-edit")
	if gitT(t, wt, "rev-parse", "HEAD^2") != main {
		t.Fatal("the agent's commit is not the merge of main")
	}
}

// Only protected conflicts: the runner takes main's side and commits the merge.
func TestPreMergeCommitsWhenOnlyProtectedConflict(t *testing.T) {
	r, wt := preMergeRig(t, map[string]string{".claude/settings.json": `{"main": 1}` + "\n", "other.txt": "x\n"})
	p, err := r.g.PreMerge(r.ctx, wt, "main", "loop/1-x")
	if err != nil {
		t.Fatal(err)
	}
	main := gitT(t, r.mirror, "rev-parse", "refs/remotes/origin/main")
	if !p.Committed || len(p.Conflicts) != 0 || gitT(t, wt, "rev-parse", "HEAD^2") != main {
		t.Fatalf("pre-merge %+v, want a merge commit of main", p)
	}
	if got := gitT(t, wt, "show", "HEAD:app.txt"); got != "branch" {
		t.Fatalf("app.txt is %q, want the branch's", got)
	}
	if st := gitT(t, wt, "status", "--porcelain"); st != "" {
		t.Fatalf("the worktree is not clean: %s", st)
	}
}

// A clean merge of a protected change is committed.
func TestPreMergeClean(t *testing.T) {
	r, wt := preMergeRig(t, map[string]string{".claude/skills/x/SKILL.md": "skill\n"})
	p, err := r.g.PreMerge(r.ctx, wt, "main", "loop/1-x")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Committed || len(p.Conflicts)+len(p.Taken) != 0 || gitT(t, wt, "rev-parse", "HEAD^2") != p.SHA {
		t.Fatalf("pre-merge %+v, want a clean merge commit", p)
	}
}

// Main's change touches nothing protected, or the branch is not behind: no merge.
func TestPreMergeNotNeeded(t *testing.T) {
	r, wt := preMergeRig(t, map[string]string{"app.txt": "main\n"})
	head := gitT(t, wt, "rev-parse", "HEAD")
	if p, err := r.g.PreMerge(r.ctx, wt, "main", "loop/1-x"); err != nil || p.SHA != "" {
		t.Fatalf("pre-merge %+v %v, want none", p, err)
	}
	if gitT(t, wt, "rev-parse", "HEAD") != head || r.g.hasRef(r.ctx, wt, "MERGE_HEAD") {
		t.Fatal("the worktree changed")
	}
	gitT(t, wt, "merge", "-q", "-X", "theirs", "--no-edit", "origin/main")
	if p, err := r.g.PreMerge(r.ctx, wt, "main", "loop/1-x"); err != nil || p.SHA != "" {
		t.Fatalf("up to date: pre-merge %+v %v, want none", p, err)
	}
}

// main deleted a protected file the branch changed: the merge takes the deletion.
func TestPreMergeTakesProtectedDeletion(t *testing.T) {
	r := newGitRig(t)
	writeIn(t, r.seed, "settings", map[string]string{".claude/settings.json": "{}\n"})
	gitT(t, r.seed, "push", "-q", r.remote, "main")
	r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote, Base: "main"})
	wt := filepath.Join(t.TempDir(), "wt")
	if _, err := r.g.AddWorktree(r.ctx, r.mirror, wt, "loop/1-x", "main", Recorded{}); err != nil {
		t.Fatal(err)
	}
	writeIn(t, wt, "branch", map[string]string{".claude/settings.json": "{\"b\": 1}\n"})
	gitT(t, r.seed, "rm", "-q", ".claude/settings.json")
	gitT(t, r.seed, "commit", "-q", "-m", "drop settings")
	gitT(t, r.seed, "push", "-q", r.remote, "main")
	r.g.EnsureMirror(r.ctx, r.mirror, RepoConfig{Remote: r.remote, Base: "main"})
	p, err := r.g.PreMerge(r.ctx, wt, "main", "loop/1-x")
	if err != nil || !p.Committed || !slices.Equal(p.Taken, []string{".claude/settings.json"}) {
		t.Fatalf("pre-merge %+v %v", p, err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatal("the deleted settings are still there")
	}
}

// #349: the brief says what the runner's merge did: a merge commit not to repeat, or one in
// progress with only the agent's conflicts to resolve.
func TestBuildBriefPreMerge(t *testing.T) {
	info := RunInfo{Slug: "a/b", Base: "main", Branch: "loop/12-x", Worktree: "/w", Scratch: "/s", Sandbox: "loop-12"}
	sha := "0123456789abcdef0123456789abcdef01234567"
	info.PreMerge = &PreMerge{SHA: sha, Taken: []string{".claude/settings.json"}, Conflicts: []string{"app.txt", "go.mod"}}
	b := BuildBrief(Item{ID: 12, Title: "x"}, info)
	for _, want := range []string{"A merge of `origin/main` (at `0123456`) is in progress", "took `main`'s side of the conflicts under `.claude/` (`.claude/settings.json`)",
		"Resolve only these conflicts: `app.txt`, `go.mod`; `git add` them and `git commit --no-edit`", "Do not abort the merge"} {
		if !strings.Contains(b, want) {
			t.Errorf("in-progress brief lacks %q", want)
		}
	}
	info.PreMerge = &PreMerge{SHA: sha, Committed: true}
	b = BuildBrief(Item{ID: 12, Title: "x"}, info)
	if !strings.Contains(b, "**The runner merged `origin/main`** (at `0123456`) into this branch before you started") || !strings.Contains(b, "Do not merge `main` again.") ||
		strings.Contains(b, "took `main`'s side") {
		t.Errorf("committed brief:\n%s", b[strings.Index(b, "## The item"):])
	}
}

// #349 end to end: a build of a branch from before main changed .claude/ starts on the runner's
// merge of main, says so in its brief and run fields, and pushes it.
func TestBuildStartsOnThePreMerge(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toBuild("Premerge")
	branch := fmt.Sprintf("loop/%d-premerge", id)
	clone := filepath.Join(t.TempDir(), "clone")
	gitT(t, g.remote, "clone", "-q", g.remote, clone)
	gitT(t, clone, "checkout", "-q", "-b", branch)
	writeIn(t, clone, "the branch's own work", map[string]string{"app.txt": "branch\n"})
	gitT(t, clone, "push", "-q", "origin", branch)
	gitT(t, clone, "checkout", "-q", "main")
	writeIn(t, clone, "vendor a skill", map[string]string{".claude/skills/x/SKILL.md": "skill\n"})
	gitT(t, clone, "push", "-q", "origin", "main")
	main := gitT(t, clone, "rev-parse", "HEAD")
	g.runOnce()
	if d := g.item(id); d.Item.Stage != pipeline.Review {
		t.Fatalf("stage %s %q; log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	if p := g.read(filepath.Join(g.lastRun(id, "build"), "brief.md")); !strings.Contains(p, "**The runner merged `origin/main`** (at `"+short7(main)+"`)") {
		t.Fatalf("brief:\n%s", p)
	}
	head := g.remoteHead(branch)
	if !strings.Contains(gitT(t, g.remote, "rev-list", "--parents", head), main) {
		t.Fatalf("the pushed branch does not have main's %s merged", main)
	}
	if !strings.Contains(g.logs.String(), "runner merged origin/main at "+short7(main)) {
		t.Fatalf("log:\n%s", g.logs.String())
	}
}

func TestPRBodyPreMerge(t *testing.T) {
	b := PRBody("body", 12, nil, nil, RunStats{PreMerge: "runner merged origin/main at 0123456; protected paths taken from main"}, nil, nil)
	if !strings.Contains(b, "Before the build, the runner merged origin/main at 0123456; protected paths taken from main.") {
		t.Fatalf("footer:\n%s", b)
	}
	if strings.Contains(PRBody("body", 12, nil, nil, RunStats{}, nil, nil), "Before the build") {
		t.Fatal("a footer without a pre-merge says one")
	}
}
