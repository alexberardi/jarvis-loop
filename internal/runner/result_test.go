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
	return GitFacts{HeadSHA: headSHA, RemoteHead: headSHA, ChangedPaths: []string{"M\tinternal/x.go", "A\tinternal/x_test.go"}}
}

func goodResult() string {
	return `{"outcome":"pr_opened","repo":"alexberardi/jarvis-server","pr":161,"branch":"loop/12-fix-x",
 "head_sha":"` + headSHA + `","changed_paths":["A internal/x_test.go","M internal/x.go"],
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
	want := Expect{Stage: "build", Slug: "alexberardi/jarvis-server", Branch: "loop/12-fix-x"}
	cases := []struct {
		name  string
		raw   string
		facts func(*GitFacts)
		files []string
		err   string // "" = valid
	}{
		{"good", goodResult(), nil, []string{"after.txt"}, ""},
		{"question", `{"outcome":"question","question":"Which screen?"}`, nil, nil, ""},
		{"question without text", `{"outcome":"question","question":"  "}`, nil, nil, "question is empty"},
		{"not json", `{"outcome":`, nil, nil, "not valid JSON"},
		{"unknown field", strings.Replace(goodResult(), `"summary"`, `"approve":true,"summary"`, 1), nil, []string{"after.txt"}, "unknown field"},
		{"outcome the stage does not allow", `{"outcome":"approved","summary":"lgtm"}`, nil, nil, "not an outcome of build"},
		{"missing pr", strings.Replace(goodResult(), `"pr":161,`, ``, 1), nil, []string{"after.txt"}, "pr"},
		{"missing summary", strings.Replace(goodResult(), `"summary":"one paragraph",`, ``, 1), nil, []string{"after.txt"}, "summary"},
		{"other repo", strings.Replace(goodResult(), "alexberardi/jarvis-server", "evil/fork", 1), nil, []string{"after.txt"}, "repo"},
		{"other branch", strings.Replace(goodResult(), `"loop/12-fix-x"`, `"main"`, 1), nil, []string{"after.txt"}, "branch"},
		{"agent sha disagrees with git", strings.Replace(goodResult(), headSHA, otherSHA, 1), nil, []string{"after.txt"}, "head_sha"},
		{"pushed head is not HEAD", goodResult(), func(f *GitFacts) { f.RemoteHead = otherSHA }, []string{"after.txt"}, "pushed"},
		{"not pushed", goodResult(), func(f *GitFacts) { f.RemoteHead = "" }, []string{"after.txt"}, "pushed"},
		{"agent paths disagree with git", strings.Replace(goodResult(), `"M internal/x.go"`, `"M README.md"`, 1), nil, []string{"after.txt"}, "changed_paths"},
		{"agent leaves a path out", strings.Replace(goodResult(), `"A internal/x_test.go",`, ``, 1), nil, []string{"after.txt"}, "changed_paths"},
		{"no change at all", goodResult(), func(f *GitFacts) { f.ChangedPaths = nil }, []string{"after.txt"}, "no changes"},
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
				if res.Outcome == "pr_opened" && (res.HeadSHA != headSHA || len(res.Paths) != 2) {
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
