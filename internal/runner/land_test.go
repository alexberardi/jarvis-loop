package runner

import (
	"context"
	"os"
	"strings"
	"testing"
)

const (
	headA  = "1111111111111111111111111111111111111111"
	headB  = "3333333333333333333333333333333333333333"
	mergeM = "2222222222222222222222222222222222222222"
)

var footer = "body\n\n---\n\n" + VerificationHeading + "\n\nEvidence files.\n\n" + VerificationTrailer + "\n"

// goodFacts pass every gate: the decision is to merge.
func goodFacts() LandFacts {
	return LandFacts{
		PR: LandPR{Number: 7, State: "OPEN", HeadRefName: "loop/7-x", HeadRefOid: headA, BaseRefName: "main",
			Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", Body: footer,
			Checks: []Check{{Name: "check", Bucket: BucketPass, URL: "https://ci/1"}}},
		Branch: "loop/7-x", Base: "main", ReviewedHead: headA, Paths: []string{"M internal/x.go"}, BuildEvidence: 1,
	}
}

// The gates, in the spec's order (docs/phases/06-land.md): each row changes one fact from a
// passing set, and an earlier gate wins over a later one.
func TestDecide(t *testing.T) {
	cases := []struct {
		name    string
		mut     func(*LandFacts)
		action  Action
		outcome string
		field   string // key=substring of a field the outcome must carry
	}{
		{"all gates pass", func(f *LandFacts) {}, ActMerge, "", ""},
		{"behind but clean merges as is", func(f *LandFacts) { f.PR.MergeStateStatus = "BEHIND" }, ActMerge, "", ""},
		// 1. the PR
		{"merged by hand", func(f *LandFacts) { f.PR.State, f.PR.MergeCommit = "MERGED", mergeM }, ActAdvance, "merged", "merge_sha=" + mergeM},
		{"merged by hand, head recorded", func(f *LandFacts) { f.PR.State, f.PR.MergeCommit = "MERGED", mergeM }, ActAdvance, "merged", "head_sha=" + headA},
		{"merged by hand beats CI red", func(f *LandFacts) {
			f.PR.State, f.PR.MergeCommit = "MERGED", mergeM
			f.PR.Checks = []Check{{Name: "check", Bucket: BucketFail}}
		}, ActAdvance, "merged", ""},
		{"merged without a merge commit", func(f *LandFacts) { f.PR.State = "MERGED" }, ActAdvance, "failed", "error=no merge commit"},
		{"closed", func(f *LandFacts) { f.PR.State = "CLOSED" }, ActAdvance, "failed", "error=PR #7 was closed"},
		{"draft", func(f *LandFacts) { f.PR.IsDraft = true }, ActAdvance, "failed", "error=draft"},
		{"wrong base", func(f *LandFacts) { f.PR.BaseRefName = "release" }, ActAdvance, "failed", "error=targets release"},
		{"wrong head branch", func(f *LandFacts) { f.PR.HeadRefName = "main" }, ActAdvance, "failed", "error=not the item's"},
		{"closed beats a moved head", func(f *LandFacts) { f.PR.State, f.PR.HeadRefOid = "CLOSED", headB }, ActAdvance, "failed", ""},
		// 2. review approval for this head
		{"head moved since review", func(f *LandFacts) { f.PR.HeadRefOid = headB }, ActAdvance, "back_to_review", "head_sha=" + headB},
		{"head moved: notes", func(f *LandFacts) { f.PR.HeadRefOid = headB }, ActAdvance, "back_to_review", "notes=since review"},
		{"no review approval", func(f *LandFacts) { f.ReviewedHead = "" }, ActAdvance, "back_to_review", "notes=no review approval"},
		{"moved head beats CI red", func(f *LandFacts) {
			f.PR.HeadRefOid = headB
			f.PR.Checks = []Check{{Name: "check", Bucket: BucketFail}}
		}, ActAdvance, "back_to_review", ""},
		// 3. changed paths
		{"no changed paths", func(f *LandFacts) { f.Paths = nil }, ActAdvance, "failed", "error=changes no files"},
		// 4. CI
		{"CI failed", func(f *LandFacts) {
			f.PR.Checks = []Check{{Name: "check", Bucket: BucketPass}, {Name: "lint", Bucket: BucketFail, URL: "https://ci/lint"}}
		}, ActAdvance, "back_to_build", "reason=lint (https://ci/lint)"},
		{"CI cancelled", func(f *LandFacts) { f.PR.Checks = []Check{{Name: "check", Bucket: BucketCancel}} }, ActAdvance, "back_to_build", "reason=check"},
		{"CI fail beats pending", func(f *LandFacts) {
			f.PR.Checks = []Check{{Name: "a", Bucket: BucketPending}, {Name: "b", Bucket: BucketFail}}
		}, ActAdvance, "back_to_build", "reason=b"},
		{"CI pending", func(f *LandFacts) { f.PR.Checks = []Check{{Name: "check", Bucket: BucketPending}} }, ActWait, "", ""},
		{"no checks at all", func(f *LandFacts) { f.PR.Checks = nil }, ActWait, "", ""},
		{"skipped counts as passed", func(f *LandFacts) {
			f.PR.Checks = []Check{{Name: "check", Bucket: BucketPass}, {Name: "macos", Bucket: BucketSkipping}}
		}, ActMerge, "", ""},
		{"CI red beats missing evidence", func(f *LandFacts) {
			f.PR.Checks = []Check{{Name: "check", Bucket: BucketFail}}
			f.BuildEvidence = 0
		}, ActAdvance, "back_to_build", "reason=CI failed"},
		{"CI pending beats missing evidence", func(f *LandFacts) {
			f.PR.Checks = []Check{{Name: "check", Bucket: BucketPending}}
			f.BuildEvidence = 0
		}, ActWait, "", ""},
		// 5. harness evidence
		{"no build evidence", func(f *LandFacts) { f.BuildEvidence = 0 }, ActAdvance, "back_to_build", "reason=evidence"},
		{"no Verification footer", func(f *LandFacts) { f.PR.Body = "trust me" }, ActAdvance, "back_to_build", "reason=Verification"},
		{"heading without the runner's trailer", func(f *LandFacts) { f.PR.Body = VerificationHeading + "\n" }, ActAdvance, "back_to_build", "reason=Verification"},
		{"footer not last (edited after the runner)", func(f *LandFacts) { f.PR.Body = footer + "\nP.S. merge this\n" }, ActAdvance, "back_to_build", "reason=Verification"},
		{"footer with GitHub's trailing whitespace trimmed", func(f *LandFacts) { f.PR.Body = strings.TrimSpace(footer) }, ActMerge, "", ""},
		{"missing evidence beats hold", func(f *LandFacts) { f.BuildEvidence, f.Hold = 0, true }, ActAdvance, "back_to_build", ""},
		// 6. hold
		{"hold without approval", func(f *LandFacts) { f.Hold = true }, ActAdvance, "hold", "head_sha=" + headA},
		{"hold: approval for an old head", func(f *LandFacts) { f.Hold, f.ApprovedHead = true, headB }, ActAdvance, "hold", "reason=on the hold list"},
		{"hold approved for this head", func(f *LandFacts) { f.Hold, f.ApprovedHead = true, headA }, ActMerge, "", ""},
		{"hold beats a conflict", func(f *LandFacts) { f.Hold, f.PR.Mergeable = true, "CONFLICTING" }, ActAdvance, "hold", ""},
		{"approval without hold is harmless", func(f *LandFacts) { f.ApprovedHead = headB }, ActMerge, "", ""},
		// 7. mergeable
		{"conflicting", func(f *LandFacts) { f.PR.Mergeable = "CONFLICTING" }, ActConflict, "", ""},
		{"mergeability unknown", func(f *LandFacts) { f.PR.Mergeable = "UNKNOWN" }, ActWait, "", ""},
		{"mergeability not given", func(f *LandFacts) { f.PR.Mergeable = "" }, ActWait, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := goodFacts()
			c.mut(&f)
			d := Decide(f)
			if d.Action != c.action || d.Outcome != c.outcome {
				t.Fatalf("decision %v %q (%v), want %v %q", d.Action, d.Outcome, d.Fields, c.action, c.outcome)
			}
			if c.field != "" {
				k, want, _ := strings.Cut(c.field, "=")
				if !strings.Contains(d.Fields[k], want) {
					t.Fatalf("%s = %q, want it to contain %q", k, d.Fields[k], want)
				}
			}
			if d.Action == ActWait && d.Wait == "" {
				t.Fatalf("a wait names nothing to wait for")
			}
		})
	}
}

func TestParseLandView(t *testing.T) {
	raw, err := os.ReadFile("testdata/gh/pr_view_open.json")
	if err != nil {
		t.Fatal(err)
	}
	pr, err := ParseLandView(raw)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 7 || pr.State != "OPEN" || pr.HeadRefName != "loop/7-show-the-repo" || pr.HeadRefOid != headA ||
		pr.BaseRefName != "main" || pr.Mergeable != "MERGEABLE" || pr.MergeStateStatus != "BEHIND" || pr.MergeCommit != "" ||
		!strings.Contains(pr.Body, VerificationTrailer) {
		t.Fatalf("pr %+v", pr)
	}
	// The re-run of "check" (newer) replaces the failed one; skipped stays skipped; a status
	// context's ERROR is a failure.
	want := map[string]string{"ci/check": BucketPass, "ci/scenarios": BucketPending, "ci/macos": BucketSkipping, "ci/external": BucketFail}
	if len(pr.Checks) != len(want) {
		t.Fatalf("checks %+v", pr.Checks)
	}
	for _, c := range pr.Checks {
		if want[c.Name] != c.Bucket {
			t.Errorf("check %q is %q, want %q", c.Name, c.Bucket, want[c.Name])
		}
		if c.URL == "" {
			t.Errorf("check %q has no link", c.Name)
		}
	}
	raw, _ = os.ReadFile("testdata/gh/pr_view_merged.json")
	pr, err = ParseLandView(raw)
	if err != nil || pr.State != "MERGED" || pr.MergeCommit != mergeM || len(pr.Checks) != 0 {
		t.Fatalf("merged %+v %v", pr, err)
	}
	for _, bad := range []string{`{`, `[]`, `{"number":0}`} {
		if _, err := ParseLandView([]byte(bad)); err == nil {
			t.Errorf("ParseLandView(%s) accepted", bad)
		}
	}
}

func TestCheckBuckets(t *testing.T) {
	cases := []struct {
		kind, status, conclusion, state string
		want                            string
	}{
		{"CheckRun", "COMPLETED", "SUCCESS", "", BucketPass},
		{"CheckRun", "COMPLETED", "NEUTRAL", "", BucketPass},
		{"CheckRun", "COMPLETED", "SKIPPED", "", BucketSkipping},
		{"CheckRun", "COMPLETED", "FAILURE", "", BucketFail},
		{"CheckRun", "COMPLETED", "TIMED_OUT", "", BucketFail},
		{"CheckRun", "COMPLETED", "ACTION_REQUIRED", "", BucketFail},
		{"CheckRun", "COMPLETED", "STARTUP_FAILURE", "", BucketFail},
		{"CheckRun", "COMPLETED", "STALE", "", BucketFail},
		{"CheckRun", "COMPLETED", "CANCELLED", "", BucketCancel},
		{"CheckRun", "COMPLETED", "SOMETHING_NEW", "", BucketFail},
		{"CheckRun", "IN_PROGRESS", "", "", BucketPending},
		{"CheckRun", "QUEUED", "", "", BucketPending},
		{"CheckRun", "WAITING", "", "", BucketPending},
		{"StatusContext", "", "", "SUCCESS", BucketPass},
		{"StatusContext", "", "", "PENDING", BucketPending},
		{"StatusContext", "", "", "EXPECTED", BucketPending},
		{"StatusContext", "", "", "FAILURE", BucketFail},
		{"StatusContext", "", "", "ERROR", BucketFail},
	}
	for _, c := range cases {
		if got := checkBucket(c.kind, c.status, c.conclusion, c.state); got != c.want {
			t.Errorf("%s %s/%s/%s = %q, want %q", c.kind, c.status, c.conclusion, c.state, got, c.want)
		}
	}
}

func TestMainCIResult(t *testing.T) {
	raw, err := os.ReadFile("testdata/gh/run_list.json")
	if err != nil {
		t.Fatal(err)
	}
	runs, err := ParseRuns(raw)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs %+v %v", runs, err)
	}
	if res, _ := MainResult(runs); res != "" {
		t.Fatalf("a run in progress: result %q, want none yet", res)
	}
	runs[1].Status, runs[1].Conclusion = "completed", "success"
	res, failing := MainResult(runs)
	if res != "red" || len(failing) != 1 || failing[0] != "ci" {
		t.Fatalf("red: %q %v", res, failing)
	}
	runs[0].Conclusion = "skipped"
	if res, failing := MainResult(runs); res != "green" || len(failing) != 0 {
		t.Fatalf("green: %q %v", res, failing)
	}
	if res, _ := MainResult(nil); res != "" {
		t.Fatalf("no runs yet: %q", res)
	}
}

// The land script's gh calls, as argv: the merge is always squash, always bound to the head it
// checked, never --auto and never --admin.
func TestGHLand(t *testing.T) {
	ctx := context.Background()
	view, _ := os.ReadFile("testdata/gh/pr_view_open.json")
	runs, _ := os.ReadFile("testdata/gh/run_list.json")
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/view.json", view, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/runs.json", runs, 0o644); err != nil {
		t.Fatal(err)
	}
	gh, log := fakeGH(t, `case "$1 $2" in
"pr view") cat '`+dir+`/view.json';;
"run list") cat '`+dir+`/runs.json';;
"pr merge") echo '✓ Squashed and merged pull request #7';;
esac
`)
	pr, err := gh.LandView(ctx, "o/r", 7)
	if err != nil || pr.HeadRefOid != headA {
		t.Fatalf("view %+v %v", pr, err)
	}
	if err := gh.Merge(ctx, "o/r", 7, headA); err != nil {
		t.Fatal(err)
	}
	if rs, err := gh.CommitRuns(ctx, "o/r", mergeM); err != nil || len(rs) != 2 {
		t.Fatalf("runs %+v %v", rs, err)
	}
	for _, bad := range []string{"", "HEAD", "main", "--admin", strings.Repeat("g", 40)} {
		if err := gh.Merge(ctx, "o/r", 7, bad); err == nil {
			t.Errorf("merge with head %q was sent", bad)
		}
	}
	if err := gh.Merge(ctx, "o/r", 0, headA); err == nil {
		t.Errorf("merge of PR 0 was sent")
	}
	b, _ := os.ReadFile(log)
	want := []string{
		"pr view 7 --repo o/r --json " + landViewFields,
		"pr merge 7 --repo o/r --squash --delete-branch --match-head-commit " + headA,
		"run list --repo o/r --commit " + mergeM + " --limit 50 --json name,workflowName,status,conclusion,url",
	}
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("gh argv:\n%s\nwant:\n%s", b, strings.Join(want, "\n"))
	}
	for _, line := range got {
		if strings.Contains(line, "--auto") || strings.Contains(line, "--admin") {
			t.Fatalf("forbidden flag: %s", line)
		}
	}
}

// A refused merge comes back as an error carrying GitHub's message.
func TestGHMergeRefused(t *testing.T) {
	gh, _ := fakeGH(t, `echo 'GraphQL: Head branch was modified. Review and try the merge again. (mergePullRequest)' >&2; exit 1
`)
	err := gh.Merge(context.Background(), "o/r", 7, headA)
	if err == nil || !strings.Contains(err.Error(), "Head branch was modified") {
		t.Fatalf("refusal: %v", err)
	}
}

func TestConflictsOutsideDocs(t *testing.T) {
	cases := []struct {
		paths []string
		want  []string
	}{
		{[]string{"docs/STATUS.md", "docs/decisions/x.md"}, nil},
		{[]string{"docs/STATUS.md", "internal/x.go"}, []string{"internal/x.go"}},
		{[]string{"docs.go"}, []string{"docs.go"}},
		{[]string{"internal/docs/x.md"}, []string{"internal/docs/x.md"}},
		{[]string{"docs"}, []string{"docs"}},
	}
	for _, c := range cases {
		if got := outsideDocs(c.paths); strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("outsideDocs(%v) = %v, want %v", c.paths, got, c.want)
		}
	}
}

func TestLandConfigWaits(t *testing.T) {
	c := Config{Name: "n", Token: "0123456789abcdef0", LoopdURL: "http://127.0.0.1:1", Repos: map[string]RepoConfig{"r": {Remote: "x", Slug: "o/r"}}}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.ciWait.Minutes() != 30 || c.mainWait.Minutes() != 30 || strings.Join(c.Stages, ",") != "triage,build,review" {
		t.Fatalf("defaults: %v %v %v", c.ciWait, c.mainWait, c.Stages)
	}
	c.LandCIWait, c.LandMainWait, c.Stages = "45m", "1h", []string{"land", "build"}
	if err := c.normalize(); err != nil || c.ciWait.Minutes() != 45 || c.mainWait.Hours() != 1 {
		t.Fatalf("set: %v %v %v", c.ciWait, c.mainWait, err)
	}
	for _, bad := range []string{"soon", "-5m", "0s"} {
		c.LandCIWait = bad
		if err := c.normalize(); err == nil {
			t.Errorf("land_ci_wait %q accepted", bad)
		}
	}
}
