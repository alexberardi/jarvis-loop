package runner

import (
	"fmt"
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
		if strings.HasSuffix(f, "/") { // a directory, with a file in it
			os.MkdirAll(filepath.Join(dir, "evidence", f), 0o755)
			os.WriteFile(filepath.Join(dir, "evidence", f, "state.json"), []byte("{}"), 0o644)
			continue
		}
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
		{"a proof", withField(`"proof":[{"kind":"command","run":"scripts/check"},{"kind":"manual","look":"the board"}]`), nil, []string{"after.txt"}, ""},
		{"a bad proof", withField(`"proof":[{"kind":"command"}]`), nil, []string{"after.txt"}, "proof: check 1 (command): run is required"},
		{"a proof is not a list", withField(`"proof":"it works"`), nil, []string{"after.txt"}, "proof: must be a JSON list"},
		{"a question carries no proof", `{"outcome":"question","question":"x","proof":[]}`, nil, nil, `field "proof" is not part of outcome question`},
		{"needs qa is not a build field", withField(`"needs_qa":true`), nil, []string{"after.txt"}, `field "needs_qa" is not part of outcome ready_for_pr`},
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
		{"a tool's state dir and a dotfile are skipped", goodResult(), nil, []string{"after.txt", ".claude/", ".DS_Store"}, ""},
		{"skipped entries are not evidence", goodResult(), nil, []string{".claude/", ".DS_Store"}, "at least one evidence file"},
		{"a named directory is refused", strings.Replace(goodResult(), "evidence/after.txt", "evidence/shots", 1), nil, []string{"after.txt", "shots/"}, "directory or a dotfile"},
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
	outside := filepath.Join(t.TempDir(), "id_ed25519")
	os.WriteFile(outside, []byte("secret"), 0o600)
	ev := func(dir string) string { return filepath.Join(dir, "evidence") }
	cases := []struct {
		name    string
		files   []string
		setup   func(dir string)
		want    []string // the files kept
		skipped []string
		err     string // "" = no error
	}{
		{"regular files", []string{"a.png", "b.txt"}, nil, []string{"a.png", "b.txt"}, nil, ""},
		{"no evidence folder", nil, func(dir string) { os.Remove(ev(dir)) }, nil, nil, ""},
		// Seen on #26: a tool left its own state in evidence/; that is not the agent's evidence.
		{"a .claude dir is skipped", []string{".claude/", "repro.log"}, nil, []string{"repro.log"}, []string{".claude"}, ""},
		{"a plain dir is skipped", []string{"a.png", "sub/"}, nil, []string{"a.png"}, []string{"sub"}, ""},
		{"a dotfile is skipped", []string{".DS_Store", "a.png"}, nil, []string{"a.png"}, []string{".DS_Store"}, ""},
		{"an oversize dotfile is skipped, not refused", []string{"a.png"}, func(dir string) {
			os.Truncate(touch(ev(dir), ".big"), MaxEvidenceBytes+1)
		}, []string{"a.png"}, []string{".big"}, ""},
		{"skipped entries take no slot", append(names(MaxEvidenceFiles), ".claude/", ".x"), nil, names(MaxEvidenceFiles), []string{".claude", ".x"}, ""},
		// A symlink to a file outside is refused, never followed and uploaded.
		{"a symlinked file is refused", []string{"a.png"}, func(dir string) { os.Symlink(outside, filepath.Join(ev(dir), "key.txt")) }, nil, nil, "not a regular file"},
		{"a symlinked dotfile is refused", []string{"a.png"}, func(dir string) { os.Symlink(outside, filepath.Join(ev(dir), ".key")) }, nil, nil, "not a regular file"},
		{"a symlinked dir is refused", []string{"a.png"}, func(dir string) { os.Symlink(filepath.Dir(outside), filepath.Join(ev(dir), ".claude")) }, nil, nil, "not a regular file"},
		{"an oversize file is refused", []string{"a.png"}, func(dir string) {
			os.Truncate(touch(ev(dir), "big.log"), MaxEvidenceBytes+1)
		}, nil, nil, "at most 20 MB"},
		{"too many files", names(MaxEvidenceFiles + 1), nil, nil, nil, "at most 20"},
		{"the evidence folder as a symlink is refused", nil, func(dir string) {
			os.Remove(ev(dir))
			os.Symlink(filepath.Dir(outside), ev(dir))
		}, nil, nil, "not a directory"},
		{"the evidence folder as a file is refused", nil, func(dir string) {
			os.Remove(ev(dir))
			os.WriteFile(ev(dir), []byte("x"), 0o644)
		}, nil, nil, "not a directory"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := evidenceDir(t, c.files...)
			if c.setup != nil {
				c.setup(dir)
			}
			files, skipped, err := EvidenceFiles(dir)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want it to mention %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, f := range files {
				got = append(got, f.Name)
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") || strings.Join(skipped, ",") != strings.Join(c.skipped, ",") {
				t.Fatalf("files %v skipped %v, want %v and %v", got, skipped, c.want, c.skipped)
			}
		})
	}
}

// names returns n evidence file names, in the order ReadDir lists them.
func names(n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("f%02d.txt", i))
	}
	return out
}

func touch(dir, name string) string {
	p := filepath.Join(dir, name)
	os.WriteFile(p, []byte("x"), 0o644)
	return p
}

func resolver(name string) (string, bool) {
	switch name {
	case "jarvis-loop", "alexberardi/jarvis-loop":
		return "alexberardi/jarvis-loop", true
	case "demo", "alexberardi/demo":
		return "alexberardi/demo", true
	}
	return "", false
}

func TestValidateTriage(t *testing.T) {
	repro := func(extra string) string {
		return `{"outcome":"reproduced","area":"slack","size":"S","risk":"normal","resources":[],` + extra +
			`"summary":"The thread header has no repo.","notes":"Change bot.go; test in bot_test.go.","evidence":[{"file":"evidence/repro.txt","label":"repro"}]}`
	}
	cases := []struct {
		name, raw, itemRepo string
		wantRepo            string
		err                 string
	}{
		{"no repo: picks one by key", repro(`"repo":"jarvis-loop",`), "", "alexberardi/jarvis-loop", ""},
		{"no repo: picks one by slug", repro(`"repo":"alexberardi/jarvis-loop",`), "", "alexberardi/jarvis-loop", ""},
		{"no repo: must pick", repro(``), "", "", "must name one of the configured repos"},
		{"routed by key", `{"outcome":"routed","repo":"jarvis-loop","why":"the ask is about the digest"}`, "", "alexberardi/jarvis-loop", ""},
		{"routed by slug", `{"outcome":"routed","repo":"alexberardi/jarvis-loop","why":"x"}`, "", "alexberardi/jarvis-loop", ""},
		{"routed, unknown repo", `{"outcome":"routed","repo":"nobody/nothing","why":"x"}`, "", "", "must name one of the configured repos"},
		{"routed, no repo", `{"outcome":"routed","why":"x"}`, "", "", "must name one of the configured repos"},
		{"routed, no why", `{"outcome":"routed","repo":"jarvis-loop"}`, "", "", "why must be one line"},
		{"routed, why on two lines", `{"outcome":"routed","repo":"jarvis-loop","why":"a\nb"}`, "", "", "why must be one line"},
		{"routed when the item has a repo", `{"outcome":"routed","repo":"jarvis-loop","why":"x"}`, "jarvis-loop", "", "only for an item with none"},
		{"routed with notes", `{"outcome":"routed","repo":"jarvis-loop","why":"x","notes":"n"}`, "", "", "not part of outcome routed"},
		{"no repo: not configured", repro(`"repo":"alexberardi/other",`), "", "", "not a configured repo"},
		{"has a repo: none given", repro(``), "demo", "", ""},
		{"has a repo: the same, dropped", repro(`"repo":"alexberardi/demo",`), "demo", "", ""},
		{"has a repo: cannot change it", repro(`"repo":"jarvis-loop",`), "demo", "", "cannot change it"},
		{"needs qa", repro(`"needs_qa":true,`), "demo", "", ""},
		{"gpu", strings.Replace(repro(``), `"resources":[]`, `"resources":["gpu","voice"]`, 1), "demo", "", ""},
		{"unknown resource", strings.Replace(repro(``), `"resources":[]`, `"resources":["tpu"]`, 1), "demo", "", "unknown resource"},
		{"bad size", strings.Replace(repro(``), `"size":"S"`, `"size":"XL"`, 1), "demo", "", "size must be"},
		{"bad risk", strings.Replace(repro(``), `"risk":"normal"`, `"risk":"low"`, 1), "demo", "", "risk must be"},
		{"no area", strings.Replace(repro(``), `"area":"slack",`, ``, 1), "demo", "", "area"},
		{"no notes", strings.Replace(repro(``), `"notes":"Change bot.go; test in bot_test.go.",`, ``, 1), "demo", "", "notes are required"},
		{"no summary", strings.Replace(repro(``), `"summary":"The thread header has no repo.",`, ``, 1), "demo", "", "summary is required"},
		{"duplicate", `{"outcome":"not_reproducible","summary":"duplicate of #1 / PR #101","notes":"Same title, PR open."}`, "", "", ""},
		{"not reproducible without notes", `{"outcome":"not_reproducible","summary":"works on main"}`, "", "", "notes are required"},
		{"a repo on not_reproducible", `{"outcome":"not_reproducible","repo":"jarvis-loop","summary":"x","notes":"y"}`, "", "", `field "repo" is not part of outcome not_reproducible`},
		{"question", `{"outcome":"question","question":"Which screen?"}`, "", "", ""},
		{"a build outcome", `{"outcome":"ready_for_pr","pr_title":"x"}`, "", "", "not an outcome of triage"},
		{"a build field", strings.Replace(repro(``), `"area"`, `"pr_title":"x","area"`, 1), "demo", "", `field "pr_title" is not part of outcome reproduced`},
		{"evidence outside", strings.Replace(repro(``), `evidence/repro.txt`, `../x`, 1), "demo", "", "directly in evidence/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := ValidateTriage([]byte(c.raw), TriageExpect{ItemRepo: c.itemRepo, Resolve: resolver})
			if c.err == "" {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				if r.Repo != c.wantRepo {
					t.Fatalf("repo %q, want %q", r.Repo, c.wantRepo)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("err %v, want %q", err, c.err)
			}
		})
	}
}

func TestValidateReview(t *testing.T) {
	cases := []struct {
		name, raw, err string
		review         string
	}{
		{"approved", `{"outcome":"approved","summary":"Tests fail without the fix.","review":"**Approved.** Ran the check."}`, "", "**Approved.** Ran the check."},
		{"approved, needs qa", `{"outcome":"approved","summary":"s","review":"r","needs_qa":true}`, "", "r"},
		{"needs qa on a bounce", `{"outcome":"bounce","notes":"n","needs_qa":true}`, `field "needs_qa"`, ""},
		{"approved for the head", `{"outcome":"approved","summary":"s","review":"r","head_sha":"` + headSHA + `"}`, "", "r"},
		{"approved for another head", `{"outcome":"approved","summary":"s","review":"r","head_sha":"` + otherSHA + `"}`, "not the reviewed commit", ""},
		{"approved without a review", `{"outcome":"approved","summary":"s"}`, "review (the PR comment) is required", ""},
		{"approved without a summary", `{"outcome":"approved","review":"r"}`, "summary is required", ""},
		{"bounce", `{"outcome":"bounce","notes":"x.go:3: test the empty case.","review":"Changes needed."}`, "", "Changes needed."},
		{"bounce: the notes are the review", `{"outcome":"bounce","notes":"x.go:3: test the empty case."}`, "", "x.go:3: test the empty case."},
		{"bounce without notes", `{"outcome":"bounce","review":"no"}`, "notes are required", ""},
		{"a GitHub approval is not a field", `{"outcome":"approved","summary":"s","review":"r","approve":true}`, "unknown field", ""},
		{"a question is not a review outcome", `{"outcome":"question","question":"?"}`, "not an outcome of review", ""},
		{"a build field", `{"outcome":"approved","summary":"s","review":"r","pr_body":"x"}`, `field "pr_body"`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := ValidateReview([]byte(c.raw), headSHA)
			if c.err == "" {
				if err != nil || r.Review != c.review || r.HeadSHA != headSHA {
					t.Fatalf("%+v %v", r, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("err %v, want %q", err, c.err)
			}
		})
	}
}
