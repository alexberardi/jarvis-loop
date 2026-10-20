package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// carryRepo is a repo with main and a PR branch, origin/main standing in for the mirror's base.
type carryRepo struct {
	t   *testing.T
	dir string
}

func newCarryRepo(t *testing.T) carryRepo {
	t.Helper()
	r := carryRepo{t: t, dir: t.TempDir()}
	gitT(t, r.dir, "init", "-q", "-b", "main", ".")
	r.write("a.txt", "1\n2\n3\n4\n5\n6\n7\n8\n9\n")
	r.commit("init")
	gitT(t, r.dir, "update-ref", "refs/remotes/origin/main", "main")
	gitT(t, r.dir, "switch", "-q", "-c", "loop/7-x")
	return r
}

func (r carryRepo) write(file, content string) {
	os.WriteFile(filepath.Join(r.dir, file), []byte(content), 0o644)
}

func (r carryRepo) commit(msg string) string {
	gitT(r.t, r.dir, "add", "-A")
	gitT(r.t, r.dir, "commit", "-q", "-m", msg)
	return gitT(r.t, r.dir, "rev-parse", "HEAD")
}

// onMain makes a commit on main and moves origin/main to it.
func (r carryRepo) onMain(file, content string) {
	gitT(r.t, r.dir, "switch", "-q", "main")
	r.write(file, content)
	r.commit("main: " + file)
	gitT(r.t, r.dir, "update-ref", "refs/remotes/origin/main", "main")
	gitT(r.t, r.dir, "switch", "-q", "loop/7-x")
}

// merge merges main into the branch; with a conflict, resolve writes the files and commits.
func (r carryRepo) merge(resolve map[string]string) string {
	cmd := []string{"-c", "user.name=t", "-c", "user.email=t@example.com", "merge", "-q", "--no-ff", "--no-edit", "main"}
	if resolve == nil {
		gitT(r.t, r.dir, cmd...)
		return gitT(r.t, r.dir, "rev-parse", "HEAD")
	}
	_, _ = Git{}.run(context.Background(), r.dir, cmd...)
	for f, c := range resolve {
		r.write(f, c)
	}
	gitT(r.t, r.dir, "add", "-A")
	gitT(r.t, r.dir, "-c", "core.editor=true", "commit", "-q", "--no-edit")
	return gitT(r.t, r.dir, "rev-parse", "HEAD")
}

func (r carryRepo) delta(from, to string) ApprovalDelta {
	r.t.Helper()
	return Git{}.ApprovalDelta(context.Background(), r.dir, "main", from, to)
}

func TestApprovalDelta(t *testing.T) {
	t.Run("a clean merge of main: nothing of the PR's changed", func(t *testing.T) {
		r := newCarryRepo(t)
		r.write("b.txt", "pr\n")
		approved := r.commit("pr")
		r.onMain("c.txt", "main\n")
		head := r.merge(nil)
		d := r.delta(approved, head)
		if d.Problem != "" || len(d.Changed) != 0 || len(d.Conflicts) != 0 || !slices.Equal(d.Merges, []string{head}) {
			t.Fatalf("delta %+v", d)
		}
		if !d.Carryable() {
			t.Fatal("not carryable")
		}
	})
	t.Run("a conflict resolved inside its hunk", func(t *testing.T) {
		r := newCarryRepo(t)
		r.write("a.txt", "1\n2\n3\n4 pr\n5\n6\n7\n8\n9\n")
		approved := r.commit("pr")
		r.onMain("a.txt", "1\n2\n3\n4 main\n5\n6\n7\n8\n9\n")
		head := r.merge(map[string]string{"a.txt": "1\n2\n3\n4 pr and main\n5\n6\n7\n8\n9\n"})
		d := r.delta(approved, head)
		if d.Problem != "" || len(d.Changed) != 0 || !slices.Equal(d.Conflicts, []string{"a.txt"}) {
			t.Fatalf("delta %+v", d)
		}
		if !strings.Contains(d.Diff, "4 pr and main") {
			t.Fatalf("the delta's diff does not show the resolution: %q", d.Diff)
		}
	})
	t.Run("the resolution changes the PR's lines outside the conflict", func(t *testing.T) {
		r := newCarryRepo(t)
		r.write("a.txt", "1\n2\n3\n4 pr\n5\n6\n7\n8\n9\n")
		approved := r.commit("pr")
		r.onMain("a.txt", "1\n2\n3\n4 main\n5\n6\n7\n8\n9\n")
		head := r.merge(map[string]string{"a.txt": "1\n2\n3\n4 pr and main\n5\n6\n7\n8\n9 sneaked in\n"})
		d := r.delta(approved, head)
		if !slices.Equal(d.Changed, []string{"a.txt"}) || d.Carryable() {
			t.Fatalf("delta %+v", d)
		}
	})
	t.Run("the merge changes a file that had no conflict", func(t *testing.T) {
		r := newCarryRepo(t)
		r.write("b.txt", "pr\n")
		approved := r.commit("pr")
		r.onMain("c.txt", "main\n")
		gitT(t, r.dir, "merge", "-q", "--no-ff", "--no-commit", "main")
		r.write("b.txt", "pr, changed in the merge\n")
		head := r.commit("merge main")
		d := r.delta(approved, head)
		if !slices.Equal(d.Changed, []string{"b.txt"}) || d.Carryable() {
			t.Fatalf("delta %+v", d)
		}
	})
	t.Run("a new commit on the branch", func(t *testing.T) {
		r := newCarryRepo(t)
		r.write("b.txt", "pr\n")
		approved := r.commit("pr")
		r.onMain("c.txt", "main\n")
		r.merge(nil)
		r.write("b.txt", "pr v2\n")
		head := r.commit("more")
		d := r.delta(approved, head)
		if !strings.Contains(d.Problem, "not a merge of origin/main") || d.Carryable() {
			t.Fatalf("delta %+v", d)
		}
	})
	t.Run("a merge of another branch", func(t *testing.T) {
		r := newCarryRepo(t)
		r.write("b.txt", "pr\n")
		approved := r.commit("pr")
		gitT(t, r.dir, "switch", "-q", "-c", "other", "main")
		r.write("o.txt", "other\n")
		r.commit("other")
		gitT(t, r.dir, "switch", "-q", "loop/7-x")
		gitT(t, r.dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "merge", "-q", "--no-ff", "--no-edit", "other")
		head := gitT(t, r.dir, "rev-parse", "HEAD")
		d := r.delta(approved, head)
		if !strings.Contains(d.Problem, "not a merge of origin/main") || d.Carryable() {
			t.Fatalf("delta %+v", d)
		}
	})
	t.Run("a merge of another branch that holds only a merge of main", func(t *testing.T) {
		// The branch brings no commit of its own, so only the check that each merge's second
		// parent is on origin/main refuses it.
		r := newCarryRepo(t)
		r.write("b.txt", "pr\n")
		approved := r.commit("pr")
		gitT(t, r.dir, "switch", "-q", "-c", "other", "main")
		r.onMain("c.txt", "main\n")
		gitT(t, r.dir, "switch", "-q", "other")
		gitT(t, r.dir, "merge", "-q", "--no-ff", "--no-edit", "main")
		gitT(t, r.dir, "switch", "-q", "loop/7-x")
		gitT(t, r.dir, "merge", "-q", "--no-ff", "--no-edit", "other")
		head := gitT(t, r.dir, "rev-parse", "HEAD")
		d := r.delta(approved, head)
		if !strings.Contains(d.Problem, "which is not on origin/main") || d.Carryable() {
			t.Fatalf("delta %+v", d)
		}
	})
	t.Run("a rebase", func(t *testing.T) {
		r := newCarryRepo(t)
		r.write("b.txt", "pr\n")
		approved := r.commit("pr")
		r.onMain("c.txt", "main\n")
		gitT(t, r.dir, "rebase", "-q", "main")
		head := gitT(t, r.dir, "rev-parse", "HEAD")
		d := r.delta(approved, head)
		if !strings.Contains(d.Problem, "not in the new head's history") || d.Carryable() {
			t.Fatalf("delta %+v", d)
		}
	})
	t.Run("two merges of main", func(t *testing.T) {
		r := newCarryRepo(t)
		r.write("b.txt", "pr\n")
		approved := r.commit("pr")
		r.onMain("c.txt", "main\n")
		m1 := r.merge(nil)
		r.onMain("d.txt", "main 2\n")
		m2 := r.merge(nil)
		d := r.delta(approved, m2)
		if !d.Carryable() || !slices.Equal(d.Merges, []string{m1, m2}) {
			t.Fatalf("delta %+v", d)
		}
	})
}

// outsideConflicts compares a resolved file with git's merge of it (conflict markers in).
func TestKeptOutsideConflicts(t *testing.T) {
	merged := "a\nb\n<<<<<<< ours\nx\n=======\ny\n>>>>>>> theirs\nc\nd\n"
	for _, c := range []struct {
		resolved string
		ok       bool
	}{
		{"a\nb\nx\nc\nd\n", true},
		{"a\nb\nx\ny\nmore\nc\nd\n", true},
		{"a\nb\nc\nd\n", true},
		{"a\nB\nx\nc\nd\n", false},
		{"a\nb\nx\nc\nd\ne\n", false},
		{"z\na\nb\nx\nc\nd\n", false},
		{"a\nb\nx\nc\n", false},
	} {
		if got := keptOutsideConflicts(merged, c.resolved); got != c.ok {
			t.Errorf("%q: got %v", c.resolved, got)
		}
	}
	if keptOutsideConflicts("no markers\n", "changed\n") || !keptOutsideConflicts("same\n", "same\n") {
		t.Error("a file without markers must be unchanged")
	}
}
