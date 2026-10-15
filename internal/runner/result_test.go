package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	headSHA  = "0123456789abcdef0123456789abcdef01234567"
	otherSHA = "fedcba9876543210fedcba9876543210fedcba98"
)

func facts() GitFacts {
	return GitFacts{HeadSHA: headSHA, ChangedPaths: []string{"M\tinternal/x.go", "A\tinternal/x_test.go"}}
}

func goodResult() string {
	return `{"outcome":"ready_for_pr","pr_title":"Fix the timer","pr_body":"## What\nThe timer.\n\nEvidence: after.txt",
 "summary":"one paragraph","not_verified":["the phone"],"evidence":[{"file":"evidence/after.txt","label":"after"}]}`
}

func evidenceDir(t *testing.T, files ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".loop")
	os.MkdirAll(filepath.Join(dir, "evidence"), 0o755)
	for _, f := range files {
		os.WriteFile(filepath.Join(dir, "evidence", f), []byte("x"), 0o644)
	}
	return dir
}

func TestValidateResult(t *testing.T) {
	want := Expect{Stage: "build"}
	withField := func(field string) string { return strings.Replace(goodResult(), `"summary"`, field+`,"summary"`, 1) }
	without := func(field string) string { return strings.Replace(goodResult(), field, ``, 1) }
	cases := []struct {
		name  string
		raw   string
		facts func(*GitFacts)
		files []string
		err   string // "" = valid
	}{
		{"good", goodResult(), nil, []string{"after.txt"}, ""},
		{"good with the head it means", withField(`"head_sha":"` + headSHA + `"`), nil, []string{"after.txt"}, ""},
		{"needs review", withField(`"needs_review":true`), nil, []string{"after.txt"}, ""},
		{"question", `{"outcome":"question","question":"Which screen?"}`, nil, nil, ""},
		{"question without text", `{"outcome":"question","question":"  "}`, nil, nil, "question is empty"},
		{"question with a dirty worktree", `{"outcome":"question","question":"Which screen?"}`, func(f *GitFacts) { f.Dirty = []string{"?? x.go"} }, nil, ""},
		{"not json", `{"outcome":`, nil, nil, "not valid JSON"},
		{"unknown field", withField(`"approve":true`), nil, []string{"after.txt"}, "unknown field"},
		{"the old contract", withField(`"pr":161`), nil, []string{"after.txt"}, "unknown field"},
		{"pr_opened is the runner's to report", strings.Replace(goodResult(), "ready_for_pr", "pr_opened", 1), nil, []string{"after.txt"}, "not an outcome of build"},
		{"outcome the stage does not allow", `{"outcome":"approved","summary":"lgtm"}`, nil, nil, "not an outcome of build"},
		{"missing title", without(`"pr_title":"Fix the timer",`), nil, []string{"after.txt"}, "pr_title"},
		{"two-line title", strings.Replace(goodResult(), "Fix the timer", `Fix\nthe timer`, 1), nil, []string{"after.txt"}, "pr_title"},
		{"long title", strings.Replace(goodResult(), "Fix the timer", strings.Repeat("x", 257), 1), nil, []string{"after.txt"}, "pr_title"},
		{"missing body", strings.Replace(goodResult(), `"pr_body":"## What\nThe timer.\n\nEvidence: after.txt",`, ``, 1), nil, []string{"after.txt"}, "pr_body"},
		{"body too long", strings.Replace(goodResult(), "The timer.", strings.Repeat("é", MaxPRBody), 1), nil, []string{"after.txt"}, "pr_body"},
		{"missing summary", without(`"summary":"one paragraph",`), nil, []string{"after.txt"}, "summary"},
		{"agent sha disagrees with git", withField(`"head_sha":"` + otherSHA + `"`), nil, []string{"after.txt"}, "head_sha"},
		{"uncommitted changes", goodResult(), func(f *GitFacts) { f.Dirty = []string{" M internal/x.go", "?? notes.md"} }, []string{"after.txt"}, "uncommitted"},
		{"no change at all", goodResult(), func(f *GitFacts) { f.ChangedPaths = nil }, []string{"after.txt"}, "no commits"},
		{"brief committed", goodResult(), func(f *GitFacts) { f.ChangedPaths = append(f.ChangedPaths, "A\t.loop/brief.md") }, []string{"after.txt"}, ".loop"},
		{"no evidence", goodResult(), nil, nil, "evidence"},
		{"evidence entry outside the folder", strings.Replace(goodResult(), "evidence/after.txt", "../../etc/passwd", 1), nil, []string{"after.txt"}, "evidence"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := facts()
			if c.facts != nil {
				c.facts(&f)
			}
			res, err := ValidateResult([]byte(c.raw), want, f, evidenceDir(t, c.files...))
			if c.err == "" {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				if res.Outcome == OutcomeReadyForPR && (res.HeadSHA != headSHA || len(res.Paths) != 2) {
					t.Fatalf("the runner reports git's values: %+v", res)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("err = %v, want it to mention %q", err, c.err)
			}
		})
	}
}

func TestEvidenceFiles(t *testing.T) {
	dir := evidenceDir(t, "a.png", "b.txt")
	outside := filepath.Join(t.TempDir(), "id_ed25519")
	os.WriteFile(outside, []byte("secret"), 0o600)
	files, err := EvidenceFiles(dir)
	if err != nil || len(files) != 2 {
		t.Fatalf("%v %v", files, err)
	}
	// A symlink to a file outside is refused, never followed and uploaded.
	os.Symlink(outside, filepath.Join(dir, "evidence", "key.txt"))
	if _, err := EvidenceFiles(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink: %v", err)
	}
	os.Remove(filepath.Join(dir, "evidence", "key.txt"))
	os.Mkdir(filepath.Join(dir, "evidence", "sub"), 0o755)
	if _, err := EvidenceFiles(dir); err == nil {
		t.Fatal("a directory is not evidence")
	}
	os.Remove(filepath.Join(dir, "evidence", "sub"))
	for i := 0; i < 20; i++ {
		os.WriteFile(filepath.Join(dir, "evidence", strings.Repeat("x", i+1)), []byte("x"), 0o644)
	}
	if _, err := EvidenceFiles(dir); err == nil || !strings.Contains(err.Error(), "20") {
		t.Fatalf("too many: %v", err)
	}
	// The evidence folder itself being a symlink is refused too.
	dir2 := filepath.Join(t.TempDir(), ".loop")
	os.MkdirAll(dir2, 0o755)
	os.Symlink(filepath.Dir(outside), filepath.Join(dir2, "evidence"))
	if _, err := EvidenceFiles(dir2); err == nil {
		t.Fatal("a symlinked evidence folder must be refused")
	}
}
