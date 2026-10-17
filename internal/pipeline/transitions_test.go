package pipeline

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

const sha1 = "0123456789abcdef0123456789abcdef01234567"
const sha2 = "fedcba9876543210fedcba9876543210fedcba98"
const shaM = "cccccccccccccccccccccccccccccccccccccccc"

func item(stage Stage) Item {
	return Item{ID: 7, Title: "t", Priority: "P3", Stage: stage, Risk: RiskNormal, HeadSHA: sha1, BuilderAgent: "builder", CreatedAt: t0}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if code == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("want error code %q, got %v", code, err)
	}
	if pe.Code != code {
		t.Fatalf("want error code %q, got %q (%s)", code, pe.Code, pe.Message)
	}
}

// One row per transition in the spec's table, plus the refusals each row implies.
func TestApplyAdvance(t *testing.T) {
	reproduced := map[string]string{"area": "voice", "size": "M", "risk": "normal", "resources": "gpu,voice"}
	prOpened := map[string]string{"repo": "jarvis-server", "pr": "142", "branch": "fix/x", "head_sha": sha2}
	without := func(m map[string]string, k string) map[string]string {
		out := map[string]string{}
		for kk, v := range m {
			if kk != k {
				out[kk] = v
			}
		}
		return out
	}
	with := func(m map[string]string, k, v string) map[string]string {
		out := without(m, k)
		out[k] = v
		return out
	}

	cases := []struct {
		name     string
		from     Stage
		outcome  Outcome
		fields   map[string]string
		evidence int
		agent    string
		wantTo   Stage
		wantRet  Stage
		wantCode string
		noPaths  bool // send no changed_paths
	}{
		// triage → build
		{"reproduced", Triage, Reproduced, reproduced, 1, "a", Build, "", "", false},
		{"reproduced no resources", Triage, Reproduced, with(reproduced, "resources", ""), 1, "a", Build, "", "", false},
		{"reproduced missing area", Triage, Reproduced, without(reproduced, "area"), 1, "a", "", "", CodeMissingField, false},
		{"reproduced missing size", Triage, Reproduced, without(reproduced, "size"), 1, "a", "", "", CodeMissingField, false},
		{"reproduced bad size", Triage, Reproduced, with(reproduced, "size", "XL"), 1, "a", "", "", CodeInvalidField, false},
		{"reproduced bad risk", Triage, Reproduced, with(reproduced, "risk", "maybe"), 1, "a", "", "", CodeInvalidField, false},
		{"reproduced missing resources", Triage, Reproduced, without(reproduced, "resources"), 1, "a", "", "", CodeMissingField, false},
		{"reproduced unknown resource", Triage, Reproduced, with(reproduced, "resources", "gpu,tpu"), 1, "a", "", "", CodeInvalidField, false},
		{"reproduced no evidence", Triage, Reproduced, reproduced, 0, "a", "", "", CodeEvidenceRequired, false},
		{"reproduced from build", Build, Reproduced, reproduced, 1, "a", "", "", CodeWrongStage, false},
		// triage → closed
		{"not reproducible", Triage, NotReproducible, map[string]string{"summary": "works on main"}, 1, "a", Closed, "", "", false},
		{"not reproducible no evidence", Triage, NotReproducible, map[string]string{"summary": "s"}, 0, "a", "", "", CodeEvidenceRequired, false},
		{"not reproducible no summary", Triage, NotReproducible, nil, 1, "a", "", "", CodeMissingField, false},
		{"not reproducible from review", Review, NotReproducible, map[string]string{"summary": "s"}, 1, "a", "", "", CodeWrongStage, false},
		// question
		{"question from triage", Triage, Question, map[string]string{"question": "which repo?"}, 0, "a", NeedsYou, Triage, "", false},
		{"question from build", Build, Question, map[string]string{"question": "which repo?"}, 0, "a", NeedsYou, Build, "", false},
		{"question from review", Review, Question, map[string]string{"question": "q"}, 0, "a", "", "", CodeWrongStage, false},
		{"question from land", Land, Question, map[string]string{"question": "q"}, 0, "a", "", "", CodeWrongStage, false},
		{"question empty", Triage, Question, map[string]string{"question": "  "}, 0, "a", "", "", CodeMissingField, false},
		// build → review
		{"pr opened", Build, PROpened, prOpened, 1, "builder2", Review, "", "", false},
		{"pr opened bad pr", Build, PROpened, with(prOpened, "pr", "x"), 1, "a", "", "", CodeInvalidField, false},
		{"pr opened zero pr", Build, PROpened, with(prOpened, "pr", "0"), 1, "a", "", "", CodeInvalidField, false},
		{"pr opened bad sha", Build, PROpened, with(prOpened, "head_sha", "zzz"), 1, "a", "", "", CodeInvalidField, false},
		{"pr opened missing branch", Build, PROpened, without(prOpened, "branch"), 1, "a", "", "", CodeMissingField, false},
		{"pr opened missing repo", Build, PROpened, without(prOpened, "repo"), 1, "a", "", "", CodeMissingField, false},
		{"pr opened no evidence", Build, PROpened, prOpened, 0, "a", "", "", CodeEvidenceRequired, false},
		{"pr opened from triage", Triage, PROpened, prOpened, 1, "a", "", "", CodeWrongStage, false},
		{"pr opened no changed paths", Build, PROpened, prOpened, 1, "a", "", "", CodeMissingField, true},
		{"pr opened bad needs_review", Build, PROpened, with(prOpened, "needs_review", "maybe"), 1, "a", "", "", CodeInvalidField, false},
		// review → land
		{"approved", Review, Approved, map[string]string{"summary": "lgtm", "head_sha": sha1}, 0, "reviewer", Land, "", "", false},
		{"approved wrong sha", Review, Approved, map[string]string{"summary": "lgtm", "head_sha": sha2}, 0, "reviewer", "", "", CodeSHAMismatch, false},
		{"approved no summary", Review, Approved, map[string]string{"head_sha": sha1}, 0, "reviewer", "", "", CodeMissingField, false},
		{"approved by builder", Review, Approved, map[string]string{"summary": "lgtm", "head_sha": sha1}, 0, "builder", "", "", CodeSameAgent, false},
		{"approved from build", Build, Approved, map[string]string{"summary": "lgtm", "head_sha": sha1}, 0, "reviewer", "", "", CodeWrongStage, false},
		// review → build (first bounce)
		{"bounce", Review, Bounce, map[string]string{"notes": "no test for the error path"}, 0, "reviewer", Build, "", "", false},
		{"bounce no notes", Review, Bounce, nil, 0, "reviewer", "", "", CodeMissingField, false},
		{"bounce from land", Land, Bounce, map[string]string{"notes": "n"}, 0, "reviewer", "", "", CodeWrongStage, false},
		// land
		{"merged", Land, Merged, map[string]string{"merge_sha": sha2, "head_sha": sha1}, 0, "lander", Done, "", "", false},
		{"merged no merge sha", Land, Merged, map[string]string{"head_sha": sha1}, 0, "lander", "", "", CodeMissingField, false},
		{"merged no head sha", Land, Merged, map[string]string{"merge_sha": sha2}, 0, "lander", "", "", CodeMissingField, false},
		{"merged no changed paths", Land, Merged, map[string]string{"merge_sha": sha2, "head_sha": sha1}, 0, "lander", "", "", CodeMissingField, true},
		{"hold", Land, Hold, map[string]string{"reason": "touches auth", "head_sha": sha1}, 0, "lander", Held, "", "", false},
		{"hold no reason", Land, Hold, map[string]string{"head_sha": sha1}, 0, "lander", "", "", CodeMissingField, false},
		{"hold no head sha", Land, Hold, map[string]string{"reason": "r"}, 0, "lander", "", "", CodeMissingField, false},
		{"hold no changed paths", Land, Hold, map[string]string{"reason": "r", "head_sha": sha1}, 0, "lander", "", "", CodeMissingField, true},
		{"back to build", Land, BackToBuild, map[string]string{"reason": "conflict in internal/"}, 0, "lander", Build, "", "", false},
		{"back to build from review", Review, BackToBuild, map[string]string{"reason": "r"}, 0, "lander", "", "", CodeWrongStage, false},
		{"back to build no paths", Land, BackToBuild, map[string]string{"reason": "CI failed: test"}, 0, "lander", Build, "", "", true},
		{"back to review", Land, BackToReview, map[string]string{"notes": "the head moved", "head_sha": sha2}, 0, "lander", Review, "", "", false},
		{"back to review no notes", Land, BackToReview, map[string]string{"head_sha": sha2}, 0, "lander", "", "", CodeMissingField, false},
		{"back to review no head", Land, BackToReview, map[string]string{"notes": "n"}, 0, "lander", "", "", CodeMissingField, false},
		{"back to review bad head", Land, BackToReview, map[string]string{"notes": "n", "head_sha": "HEAD"}, 0, "lander", "", "", CodeInvalidField, false},
		{"back to review no paths", Land, BackToReview, map[string]string{"notes": "n", "head_sha": sha2}, 0, "lander", "", "", CodeMissingField, true},
		{"back to review from review", Review, BackToReview, map[string]string{"notes": "n", "head_sha": sha2}, 0, "lander", "", "", CodeWrongStage, false},
		// usage_limit, any work stage
		{"usage limit triage", Triage, UsageLimit, map[string]string{"error": "rate limited"}, 0, "a", Triage, "", "", false},
		{"usage limit build", Build, UsageLimit, map[string]string{"error": "rate limited", "reset": "at 17:00"}, 0, "a", Build, "", "", false},
		{"usage limit review", Review, UsageLimit, map[string]string{"error": "e"}, 0, "a", Review, "", "", false},
		{"usage limit land", Land, UsageLimit, map[string]string{"error": "e"}, 0, "a", Land, "", "", false},
		{"usage limit no error", Build, UsageLimit, nil, 0, "a", "", "", CodeMissingField, false},
		// failed, any work stage
		{"failed triage", Triage, Failed, map[string]string{"error": "boom"}, 0, "a", NeedsYou, Triage, "", false},
		{"failed land", Land, Failed, map[string]string{"error": "boom"}, 0, "a", NeedsYou, Land, "", false},
		{"failed no error", Build, Failed, nil, 0, "a", "", "", CodeMissingField, false},
		// not work stages
		{"advance needs_you", NeedsYou, Failed, map[string]string{"error": "e"}, 0, "a", "", "", CodeWrongStage, false},
		{"advance held", Held, Merged, map[string]string{"merge_sha": sha2, "head_sha": sha1}, 0, "a", "", "", CodeWrongStage, false},
		{"advance done", Done, Failed, map[string]string{"error": "e"}, 0, "a", "", "", CodeWrongStage, false},
		{"unknown outcome", Build, Outcome("shipped"), nil, 0, "a", "", "", CodeUnknownOutcome, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			it := item(tc.from)
			paths := []string{"M internal/modules/cc/turn.go"}
			if tc.noPaths {
				paths = nil
			}
			tr, err := ApplyAdvance(it, Advance{Outcome: tc.outcome, Fields: tc.fields, Evidence: tc.evidence, Agent: tc.agent,
				ChangedPaths: paths, HoldRules: DefaultHoldRules()}, t0)
			wantCode(t, err, tc.wantCode)
			if tc.wantCode != "" {
				return
			}
			if tr.From != tc.from || tr.To != tc.wantTo || tr.Item.Stage != tc.wantTo {
				t.Fatalf("from %s to %s (item %s), want %s → %s", tr.From, tr.To, tr.Item.Stage, tc.from, tc.wantTo)
			}
			if tr.Item.ReturnStage != tc.wantRet {
				t.Fatalf("return stage %q, want %q", tr.Item.ReturnStage, tc.wantRet)
			}
			if tr.Outcome != tc.outcome {
				t.Fatalf("outcome %q, want %q", tr.Outcome, tc.outcome)
			}
			if got := tr.ActiveAgentsZero; got != (tc.outcome == UsageLimit) {
				t.Fatalf("ActiveAgentsZero = %v", got)
			}
		})
	}
}

func TestReproducedSetsTriageResult(t *testing.T) {
	tr, err := ApplyAdvance(item(Triage), Advance{Outcome: Reproduced, Evidence: 1, Fields: map[string]string{
		"area": "voice", "size": "L", "risk": "hold", "resources": " voice , gpu,voice",
	}}, t0)
	wantCode(t, err, "")
	it := tr.Item
	if it.Area != "voice" || it.Size != "L" || it.Risk != RiskHold {
		t.Fatalf("triage result not recorded: %+v", it)
	}
	if len(it.Resources) != 2 || it.Resources[0] != "gpu" || it.Resources[1] != "voice" {
		t.Fatalf("resources = %v, want sorted, deduplicated [gpu voice]", it.Resources)
	}
}

// Triage may name the repo of an item that has none (items added from Slack), and only a repo
// loopd is configured with.
func TestReproducedRepo(t *testing.T) {
	repos := []string{"alexberardi/jarvis-loop", "alexberardi/jarvis-server"}
	base := map[string]string{"area": "loopd", "size": "S", "risk": "normal", "resources": ""}
	cases := []struct {
		name, itemRepo, repo string
		repos                []string
		want                 string // the item's repo after
		code                 string
	}{
		{"no repo, none given", "", "", repos, "", ""},
		{"no repo, a configured one", "", "alexberardi/jarvis-loop", repos, "alexberardi/jarvis-loop", ""},
		{"no repo, spaces trimmed", "", " alexberardi/jarvis-server ", repos, "alexberardi/jarvis-server", ""},
		{"no repo, not configured", "", "alexberardi/other", repos, "", CodeInvalidField},
		{"no repo, a short name is not a slug", "", "jarvis-loop", repos, "", CodeInvalidField},
		{"no repo, loopd has no repos configured", "", "alexberardi/jarvis-loop", nil, "", CodeInvalidField},
		{"already has one, none given", "demo", "", repos, "demo", ""},
		{"already has one, even the same", "alexberardi/jarvis-loop", "alexberardi/jarvis-loop", repos, "", CodeInvalidField},
		{"already has one, another", "demo", "alexberardi/jarvis-loop", repos, "", CodeInvalidField},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			it := item(Triage)
			it.Repo = c.itemRepo
			f := map[string]string{}
			for k, v := range base {
				f[k] = v
			}
			if c.repo != "" {
				f["repo"] = c.repo
			}
			tr, err := ApplyAdvance(it, Advance{Outcome: Reproduced, Evidence: 1, Fields: f, Repos: c.repos}, t0)
			wantCode(t, err, c.code)
			if c.code == "" && tr.Item.Repo != c.want {
				t.Fatalf("repo %q, want %q", tr.Item.Repo, c.want)
			}
		})
	}
}

var clean = []string{"M internal/modules/cc/turn.go"}

func prOpened(head string, paths []string, extra map[string]string) Advance {
	f := map[string]string{"repo": "jarvis-server", "pr": "142", "branch": "fix/x", "head_sha": head}
	for k, v := range extra {
		f[k] = v
	}
	return Advance{Outcome: PROpened, Evidence: 1, Agent: "builder-2", Fields: f, ChangedPaths: paths, HoldRules: DefaultHoldRules()}
}

func merged(head string, paths []string) Advance {
	return Advance{Outcome: Merged, Agent: "lander", Fields: map[string]string{"merge_sha": shaM, "head_sha": head},
		ChangedPaths: paths, HoldRules: DefaultHoldRules()}
}

func landHold(head string, paths []string) Advance {
	return Advance{Outcome: Hold, Agent: "lander", Fields: map[string]string{"reason": "merged main in", "head_sha": head},
		ChangedPaths: paths, HoldRules: DefaultHoldRules()}
}

func TestPROpenedRecordsBuilderAndVoidsApproval(t *testing.T) {
	it := item(Build)
	it.Approval = &Approval{By: "alex", HeadSHA: sha1, At: t0}
	tr, err := ApplyAdvance(it, prOpened(sha2, clean, nil), t0)
	wantCode(t, err, "")
	if tr.Item.BuilderAgent != "builder-2" || tr.Item.PR != 142 || tr.Item.Branch != "fix/x" || tr.Item.HeadSHA != sha2 || tr.Item.Repo != "jarvis-server" {
		t.Fatalf("pr_opened not recorded: %+v", tr.Item)
	}
	if tr.Item.Approval != nil {
		t.Fatalf("approval for the old head must be void after a new head sha")
	}
	if tr.Item.Risk != RiskNormal || tr.Item.HoldReason != "" {
		t.Fatalf("clean paths made it hold: %+v", tr.Item)
	}

	// Same head sha keeps it (nothing the user saw has changed).
	it.HeadSHA = sha2
	it.Approval = &Approval{By: "alex", HeadSHA: sha2, At: t0}
	tr, err = ApplyAdvance(it, prOpened(sha2, clean, nil), t0)
	wantCode(t, err, "")
	if tr.Item.Approval == nil {
		t.Fatalf("approval for the same head sha should stand")
	}
}

// Hold risk is computed by loopd from the PR's changed paths, not trusted from triage.
func TestPROpenedComputesHoldRisk(t *testing.T) {
	cases := []struct {
		name       string
		triageRisk string
		paths      []string
		extra      map[string]string
		wantRisk   string
		wantReason string // substring of hold_reason
	}{
		{"triage normal, auth path", RiskNormal, []string{"M internal/modules/cc/turn.go", "M internal/modules/auth/login.go"}, nil, RiskHold, "auth: M internal/modules/auth/login.go"},
		{"deleted test", RiskNormal, []string{"D internal/modules/cc/turn_test.go"}, nil, RiskHold, "deleted-test: D internal/modules/cc/turn_test.go"},
		{"CI workflow", RiskNormal, []string{"M .github/workflows/ci.yml"}, nil, RiskHold, "ci: M .github/workflows/ci.yml"},
		{"marked needs review", RiskNormal, clean, map[string]string{"needs_review": "true"}, RiskHold, "marked needs review"},
		{"needs_review false", RiskNormal, clean, map[string]string{"needs_review": "false"}, RiskNormal, ""},
		{"clean stays normal", RiskNormal, clean, nil, RiskNormal, ""},
		{"triage hold stays hold on clean paths", RiskHold, clean, nil, RiskHold, "triage"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			it := item(Build)
			it.Risk = c.triageRisk
			if c.triageRisk == RiskHold {
				it.HoldReason = "triage: risk hold"
			}
			tr, err := ApplyAdvance(it, prOpened(sha2, c.paths, c.extra), t0)
			wantCode(t, err, "")
			if tr.Item.Risk != c.wantRisk || !strings.Contains(tr.Item.HoldReason, c.wantReason) {
				t.Fatalf("risk %q reason %q, want %q containing %q", tr.Item.Risk, tr.Item.HoldReason, c.wantRisk, c.wantReason)
			}
			if c.wantRisk == RiskHold && c.wantReason != "triage" && c.wantReason != "marked needs review" && tr.Data["hold_matches"] == nil {
				t.Fatalf("the matched rule is not on the event: %+v", tr.Data)
			}
		})
	}
}

func TestReproducedHoldSetsReason(t *testing.T) {
	tr, err := ApplyAdvance(item(Triage), Advance{Outcome: Reproduced, Evidence: 1, Fields: map[string]string{
		"area": "a", "size": "S", "risk": "hold", "resources": ""}}, t0)
	wantCode(t, err, "")
	if tr.Item.HoldReason != "triage: risk hold" {
		t.Fatalf("hold reason %q", tr.Item.HoldReason)
	}
}

func TestRiskOnlyRatchetsUp(t *testing.T) {
	it := item(Build)
	tr, err := ApplyAdvance(it, prOpened(sha1, []string{"M internal/update/helper.go"}, nil), t0)
	wantCode(t, err, "")
	it = tr.Item
	it.Stage = Build // bounced back
	tr, err = ApplyAdvance(it, prOpened(sha2, clean, nil), t0)
	wantCode(t, err, "")
	if tr.Item.Risk != RiskHold || !strings.Contains(tr.Item.HoldReason, "update: M internal/update/helper.go") {
		t.Fatalf("risk went down: %q %q", tr.Item.Risk, tr.Item.HoldReason)
	}
}

func TestMergedHoldRiskNeedsApprovalForMergedHead(t *testing.T) {
	it := item(Land)
	it.Risk = RiskHold
	_, err := ApplyAdvance(it, merged(sha1, clean), t0)
	wantCode(t, err, CodeApprovalRequired)

	// Approval for the recorded head, but land merged another one.
	it.Approval = &Approval{By: "alex", HeadSHA: sha1, At: t0}
	_, err = ApplyAdvance(it, merged(sha2, clean), t0)
	wantCode(t, err, CodeApprovalRequired)

	tr, err := ApplyAdvance(it, merged(sha1, clean), t0)
	wantCode(t, err, "")
	if tr.To != Done || tr.Item.MergeSHA != shaM || tr.Item.HeadSHA != sha1 {
		t.Fatalf("merged: %+v", tr.Item)
	}
}

// A normal item becomes hold at merge time when what was merged hits a rule.
func TestMergedRecomputesRisk(t *testing.T) {
	_, err := ApplyAdvance(item(Land), merged(sha2, []string{"D contract/auth/login_test.go"}), t0)
	wantCode(t, err, CodeApprovalRequired)

	tr, err := ApplyAdvance(item(Land), merged(sha2, clean), t0)
	wantCode(t, err, "")
	if tr.Item.HeadSHA != sha2 || tr.To != Done {
		t.Fatalf("merged a normal item at a new head: %+v", tr.Item)
	}
}

// The spec's scenario: approval for A; land pushes B; merged with B refused; hold with B shows B and
// voids A; approve B; merged with B succeeds.
func TestLandHeadMovesAfterApproval(t *testing.T) {
	it := item(Held)
	it.Risk, it.HeadSHA = RiskHold, sha1
	tr, err := ApplyApprove(it, "alex", sha1, "ok", t0)
	wantCode(t, err, "")
	it = tr.Item // land, approved for A

	_, err = ApplyAdvance(it, merged(sha2, clean), t0)
	wantCode(t, err, CodeApprovalRequired)

	tr, err = ApplyAdvance(it, landHold(sha2, clean), t0)
	wantCode(t, err, "")
	it = tr.Item
	if it.Stage != Held || it.HeadSHA != sha2 || it.Approval != nil {
		t.Fatalf("hold with a new head: %+v", it)
	}

	_, err = ApplyApprove(it, "alex", sha1, "", t0)
	wantCode(t, err, CodeSHAMismatch)
	tr, err = ApplyApprove(it, "alex", sha2, "yes", t0)
	wantCode(t, err, "")
	tr, err = ApplyAdvance(tr.Item, merged(sha2, clean), t0)
	wantCode(t, err, "")
	if tr.To != Done || tr.Item.HeadSHA != sha2 {
		t.Fatalf("merged B: %+v", tr.Item)
	}
}

func TestLandHoldSameHeadKeepsApproval(t *testing.T) {
	it := item(Land)
	it.Approval = &Approval{By: "alex", HeadSHA: sha1, At: t0}
	tr, err := ApplyAdvance(it, landHold(sha1, clean), t0)
	wantCode(t, err, "")
	if tr.Item.Approval == nil || tr.Item.Risk != RiskHold || !strings.Contains(tr.Item.HoldReason, "land: merged main in") {
		t.Fatalf("hold at the same head: %+v", tr.Item)
	}
}

// Land's back_to_review: the PR's head moved since review (or land merged the base in). The new
// head goes to a fresh review; an approval for another head is void; risk is recomputed on the
// paths; the builder stays the builder, so review independence still holds.
func TestBackToReview(t *testing.T) {
	it := item(Land)
	it.Approval = &Approval{By: "alex", HeadSHA: sha1, At: t0}
	tr, err := ApplyAdvance(it, Advance{Outcome: BackToReview, Agent: "lander",
		Fields: map[string]string{"notes": "head moved", "head_sha": sha2}, ChangedPaths: []string{"M internal/modules/auth/x.go"},
		HoldRules: DefaultHoldRules()}, t0)
	wantCode(t, err, "")
	if tr.To != Review || tr.Item.HeadSHA != sha2 || tr.Item.Approval != nil || tr.Data["approval_voided"] != true {
		t.Fatalf("back to review: %+v %+v", tr.Item, tr.Data)
	}
	if tr.Item.Risk != RiskHold || !strings.Contains(tr.Item.HoldReason, "auth: M internal/modules/auth/x.go") {
		t.Fatalf("risk not recomputed: %q %q", tr.Item.Risk, tr.Item.HoldReason)
	}
	if tr.Item.BuilderAgent != "builder" {
		t.Fatalf("builder changed to %q", tr.Item.BuilderAgent)
	}
	// The same head keeps the approval (nothing to re-approve).
	tr, err = ApplyAdvance(it, Advance{Outcome: BackToReview, Fields: map[string]string{"notes": "n", "head_sha": sha1},
		ChangedPaths: clean, HoldRules: DefaultHoldRules()}, t0)
	wantCode(t, err, "")
	if tr.Item.Approval == nil {
		t.Fatalf("approval for the same head was voided")
	}
}

// back_to_build from land may carry the changed paths; when it does, risk is recomputed.
func TestBackToBuildRecomputesRisk(t *testing.T) {
	tr, err := ApplyAdvance(item(Land), Advance{Outcome: BackToBuild, Fields: map[string]string{"reason": "CI failed"},
		ChangedPaths: []string{"D x_test.go"}, HoldRules: DefaultHoldRules()}, t0)
	wantCode(t, err, "")
	if tr.Item.Risk != RiskHold {
		t.Fatalf("risk %q", tr.Item.Risk)
	}
	_, err = ApplyAdvance(item(Land), Advance{Outcome: BackToBuild, Fields: map[string]string{"reason": "r"},
		ChangedPaths: []string{"M /etc/passwd"}}, t0)
	wantCode(t, err, CodeInvalidField)
}

// The rules applied are the item's repo's: pr_opened uses the repo it reports (the item may not
// have had one), every other outcome the item's.
func TestHoldRulesFollowTheItemsRepo(t *testing.T) {
	loopPaths := []string{"M internal/pipeline/pipeline.go"}
	a := prOpened(sha2, loopPaths, map[string]string{"repo": LoopRepo})
	tr, err := ApplyAdvance(item(Build), a, t0)
	wantCode(t, err, "")
	if tr.Item.Risk != RiskHold || !strings.Contains(tr.Item.HoldReason, "approval: M internal/pipeline/pipeline.go") {
		t.Fatalf("jarvis-loop pr_opened: %q %q", tr.Item.Risk, tr.Item.HoldReason)
	}
	tr, err = ApplyAdvance(item(Build), prOpened(sha2, loopPaths, map[string]string{"repo": ServerRepo}), t0)
	wantCode(t, err, "")
	if tr.Item.Risk != RiskNormal {
		t.Fatalf("jarvis-server pr_opened held on a jarvis-loop rule: %q", tr.Item.HoldReason)
	}
	it := item(Land)
	it.Repo = LoopRepo
	_, err = ApplyAdvance(it, merged(sha1, loopPaths), t0)
	wantCode(t, err, CodeApprovalRequired)
	it.Repo = ServerRepo
	_, err = ApplyAdvance(it, merged(sha1, loopPaths), t0)
	wantCode(t, err, "")
}

func TestHumanTransitions(t *testing.T) {
	t.Run("answer returns to the stage that asked", func(t *testing.T) {
		it := item(NeedsYou)
		it.ReturnStage = Build
		it.Question = "which repo?"
		tr, err := ApplyAnswer(it, "jarvis-server")
		wantCode(t, err, "")
		if tr.To != Build || tr.Item.Stage != Build || tr.Item.Question != "" || tr.Item.ReturnStage != "" {
			t.Fatalf("answer: %+v", tr.Item)
		}
	})
	t.Run("answer needs text", func(t *testing.T) {
		it := item(NeedsYou)
		it.ReturnStage = Build
		_, err := ApplyAnswer(it, " ")
		wantCode(t, err, CodeMissingField)
	})
	t.Run("answer outside needs_you", func(t *testing.T) {
		_, err := ApplyAnswer(item(Build), "x")
		wantCode(t, err, CodeWrongStage)
	})
	t.Run("approve with the head sha", func(t *testing.T) {
		tr, err := ApplyApprove(item(Held), "alex", sha1, "yes, merge it", t0)
		wantCode(t, err, "")
		a := tr.Item.Approval
		if tr.To != Land || a == nil || a.By != "alex" || a.HeadSHA != sha1 || a.Words != "yes, merge it" || !a.At.Equal(t0) {
			t.Fatalf("approve: %+v %+v", tr, a)
		}
	})
	t.Run("approve with another sha is refused", func(t *testing.T) {
		_, err := ApplyApprove(item(Held), "alex", sha2, "", t0)
		wantCode(t, err, CodeSHAMismatch)
	})
	t.Run("approve outside held", func(t *testing.T) {
		_, err := ApplyApprove(item(Land), "alex", sha1, "", t0)
		wantCode(t, err, CodeWrongStage)
	})
	t.Run("hold stays held", func(t *testing.T) {
		tr, err := ApplyHold(item(Held), "not yet")
		wantCode(t, err, "")
		if tr.To != Held {
			t.Fatalf("hold: %+v", tr)
		}
	})
	t.Run("hold outside held", func(t *testing.T) {
		_, err := ApplyHold(item(Review), "")
		wantCode(t, err, CodeWrongStage)
	})
	t.Run("rebuild sends a held item to build with the notes, no bounce", func(t *testing.T) {
		it := item(Held)
		it.PR, it.Branch, it.Risk, it.HoldReason = 12, "loop/7-t", RiskHold, "land: on the hold list"
		it.Approval = &Approval{By: "alex", HeadSHA: sha1}
		tr, err := ApplyRebuild(it, " rebase and fix the typo ")
		wantCode(t, err, "")
		got := tr.Item
		if tr.From != Held || tr.To != Build || tr.Kind != "rebuild" || got.Stage != Build || got.BounceCount != 0 || got.Approval != nil {
			t.Fatalf("rebuild: %+v", tr)
		}
		if got.PR != 12 || got.Branch != "loop/7-t" || got.Risk != RiskHold || got.HoldReason != "land: on the hold list" {
			t.Fatalf("rebuild lost the PR or the hold: %+v", got)
		}
		if tr.Data["notes"] != "rebase and fix the typo" || tr.Data["approval_voided"] != true {
			t.Fatalf("data %+v", tr.Data)
		}
	})
	t.Run("rebuild needs notes", func(t *testing.T) {
		_, err := ApplyRebuild(item(Held), " ")
		wantCode(t, err, CodeMissingField)
	})
	t.Run("rebuild outside held", func(t *testing.T) {
		for _, s := range []Stage{Land, Review, Build, NeedsYou, QA, Done, Closed} {
			_, err := ApplyRebuild(item(s), "rebuild")
			wantCode(t, err, CodeWrongStage)
		}
	})
}

func TestApplyPatch(t *testing.T) {
	p1, title, bad, empty := "P1", "new title", "P9", " "
	tr, err := ApplyPatch(item(Build), &p1, &title)
	wantCode(t, err, "")
	if tr.Item.Priority != "P1" || tr.Item.Title != "new title" {
		t.Fatalf("patch: %+v", tr.Item)
	}
	_, err = ApplyPatch(item(Build), &bad, nil)
	wantCode(t, err, CodeInvalidField)
	_, err = ApplyPatch(item(Build), nil, &empty)
	wantCode(t, err, CodeInvalidField)
	_, err = ApplyPatch(item(Build), nil, nil)
	wantCode(t, err, CodeMissingField)
	for _, s := range []Stage{Done, Closed} {
		_, err = ApplyPatch(item(s), &p1, nil)
		wantCode(t, err, CodeWrongStage)
	}
	for _, s := range []Stage{Triage, NeedsYou, Held} {
		_, err = ApplyPatch(item(s), &p1, nil)
		wantCode(t, err, "")
	}
}

func TestOvertime(t *testing.T) {
	tr := Overtime(item(Build), "running scripts/check", 45*time.Minute)
	if tr.To != NeedsYou || tr.Item.ReturnStage != Build {
		t.Fatalf("overtime: %+v", tr)
	}
	want := "run exceeded 45 minutes; last note: running scripts/check"
	if tr.Item.Question != want {
		t.Fatalf("question %q, want %q", tr.Item.Question, want)
	}
	if q := Overtime(item(Build), "", 45*time.Minute).Item.Question; q != "run exceeded 45 minutes" {
		t.Fatalf("question without a note: %q", q)
	}
}

func TestDisplay(t *testing.T) {
	cases := []struct {
		s       Stage
		claimed bool
		want    string
	}{
		{Triage, false, "intake"}, {Triage, true, "triage"},
		{Build, false, "ready"}, {Build, true, "build"},
		{Review, false, "review (queued)"}, {Review, true, "review"},
		{Land, false, "land (queued)"}, {Land, true, "land"},
		{NeedsYou, false, "needs-you"}, {Held, false, "held"},
		{Done, false, "done"}, {Closed, false, "closed"},
	}
	for _, c := range cases {
		if got := Display(c.s, c.claimed); got != c.want {
			t.Errorf("Display(%s, %v) = %q, want %q", c.s, c.claimed, got, c.want)
		}
	}
}

func TestNewItemDefaults(t *testing.T) {
	cases := []struct {
		pri, src         string
		wantPri, wantSrc string
		code             string
	}{
		{"", "", "P3", "user", ""},
		{"", "scout", "P4", "scout", ""},
		{"P1", "scout", "P1", "scout", ""},
		{"P2", "nightly", "P2", "nightly", ""},
		{"", "finding", "P3", "finding", ""},
		{"P0", "", "", "", CodeInvalidField},
		{"", "slack", "", "", CodeInvalidField},
	}
	for _, c := range cases {
		it, err := NewItem("title", "ask", "jarvis-server", c.pri, c.src, t0)
		wantCode(t, err, c.code)
		if c.code != "" {
			continue
		}
		if it.Priority != c.wantPri || it.Source != c.wantSrc || it.Stage != Triage || it.Risk != RiskNormal {
			t.Errorf("NewItem(%q,%q) = %+v", c.pri, c.src, it)
		}
	}
	_, err := NewItem(" ", "", "", "", "", t0)
	wantCode(t, err, CodeMissingField)
}

func TestControlValidate(t *testing.T) {
	ok := DefaultControl()
	if ok.ActiveAgents != 4 || ok.MaxRunMinutes != 45 || ok.Limits["gpu"] != 2 || ok.Limits["review"] != 2 {
		t.Fatalf("defaults: %+v", ok)
	}
	wantCode(t, ok.Validate(), "")
	for _, mut := range []func(*Control){
		func(c *Control) { c.ActiveAgents = 7 },
		func(c *Control) { c.ActiveAgents = -1 },
		func(c *Control) { c.MaxRunMinutes = 0 },
		func(c *Control) { c.Limits = map[string]int{"tpu": 1} },
		func(c *Control) { c.Limits = map[string]int{"gpu": -1} },
		func(c *Control) { c.Limits = map[string]int{"build": 1} },
	} {
		c := DefaultControl()
		mut(&c)
		wantCode(t, c.Validate(), CodeInvalidField)
	}
}

// Triage with no repo may route the item first (docs/phases/06b-all-repos.md): routed sets the
// repo and leaves the item in triage, for a run with that one repo checked out.
func TestRouted(t *testing.T) {
	repos := []string{"alexberardi/jarvis-node-mobile", "alexberardi/jarvis-server"}
	cases := []struct {
		name, itemRepo string
		fields         map[string]string
		repos          []string
		code           string
	}{
		{"a configured repo", "", map[string]string{"repo": "alexberardi/jarvis-node-mobile", "why": "the ask is about the app's home screen"}, repos, ""},
		{"spaces trimmed", "", map[string]string{"repo": " alexberardi/jarvis-server ", "why": "x"}, repos, ""},
		{"repo missing", "", map[string]string{"why": "x"}, repos, CodeMissingField},
		{"why missing", "", map[string]string{"repo": "alexberardi/jarvis-server"}, repos, CodeMissingField},
		{"not configured", "", map[string]string{"repo": "alexberardi/other", "why": "x"}, repos, CodeInvalidField},
		{"a short name is not a slug", "", map[string]string{"repo": "jarvis-server", "why": "x"}, repos, CodeInvalidField},
		{"loopd has no repos", "", map[string]string{"repo": "alexberardi/jarvis-server", "why": "x"}, nil, CodeInvalidField},
		{"the item has a repo already", "alexberardi/jarvis-server", map[string]string{"repo": "alexberardi/jarvis-node-mobile", "why": "x"}, repos, CodeInvalidField},
		{"even the same one", "alexberardi/jarvis-server", map[string]string{"repo": "alexberardi/jarvis-server", "why": "x"}, repos, CodeInvalidField},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			it := item(Triage)
			it.Repo = c.itemRepo
			tr, err := ApplyAdvance(it, Advance{Outcome: Routed, Fields: c.fields, Repos: c.repos, Agent: "a"}, t0)
			wantCode(t, err, c.code)
			if c.code != "" {
				return
			}
			want := strings.TrimSpace(c.fields["repo"])
			if tr.Item.Repo != want || tr.To != Triage || tr.Item.Stage != Triage || tr.Data["repo"] != want {
				t.Fatalf("routed: repo %q stage %s data %v; want %q in triage", tr.Item.Repo, tr.Item.Stage, tr.Data, want)
			}
		})
	}
	// Routing needs no evidence (a routing run reads one file), and is triage's only.
	for _, from := range []Stage{Build, Review, Land} {
		it := item(from)
		_, err := ApplyAdvance(it, Advance{Outcome: Routed, Fields: map[string]string{"repo": repos[0], "why": "x"}, Repos: repos}, t0)
		wantCode(t, err, CodeWrongStage)
	}
}

// A failed runner verify sends the build back to build, counting like a review bounce; the
// second one goes to the user.
func TestVerifyFailed(t *testing.T) {
	it := item(Build)
	f := map[string]string{"reason": "verify unit tests failed (exit 1)"}
	_, err := ApplyAdvance(it, Advance{Outcome: VerifyFailed, Fields: f}, t0)
	wantCode(t, err, CodeEvidenceRequired)
	_, err = ApplyAdvance(it, Advance{Outcome: VerifyFailed, Evidence: 1}, t0)
	wantCode(t, err, CodeMissingField)
	tr, err := ApplyAdvance(it, Advance{Outcome: VerifyFailed, Fields: f, Evidence: 1}, t0)
	wantCode(t, err, "")
	if tr.Item.Stage != Build || tr.Item.BounceCount != 1 || tr.Data["bounce_count"] != 1 {
		t.Fatalf("first verify failure: %s, bounces %d", tr.Item.Stage, tr.Item.BounceCount)
	}
	tr, err = ApplyAdvance(tr.Item, Advance{Outcome: VerifyFailed, Fields: f, Evidence: 1}, t0)
	wantCode(t, err, "")
	if tr.Item.Stage != NeedsYou || tr.Item.ReturnStage != Build || tr.Item.Question != f["reason"] {
		t.Fatalf("second: %s (return %s, q %q), want needs_you returning to build", tr.Item.Stage, tr.Item.ReturnStage, tr.Item.Question)
	}
	// A review bounce and a verify failure share the count.
	b := item(Review)
	tr, _ = ApplyAdvance(b, Advance{Outcome: Bounce, Fields: map[string]string{"notes": "x"}, Agent: "r"}, t0)
	tr, err = ApplyAdvance(tr.Item, Advance{Outcome: VerifyFailed, Fields: f, Evidence: 1}, t0)
	wantCode(t, err, "")
	if tr.Item.Stage != NeedsYou {
		t.Fatalf("a bounce then a verify failure: %s, want needs_you", tr.Item.Stage)
	}
	for _, from := range []Stage{Triage, Review, Land} {
		_, err := ApplyAdvance(item(from), Advance{Outcome: VerifyFailed, Fields: f, Evidence: 1}, t0)
		wantCode(t, err, CodeWrongStage)
	}
}

// The review agent holds an item for content a path rule cannot see (a curl | sh pin in a docs
// page, a new secret in a package) by approving with needs_review.
func TestApprovedNeedsReview(t *testing.T) {
	base := map[string]string{"summary": "ok", "head_sha": sha1}
	for _, c := range []struct {
		v    string
		hold bool
		code string
	}{{"", false, ""}, {"false", false, ""}, {"true", true, ""}, {"yes", false, CodeInvalidField}} {
		f := map[string]string{}
		for k, v := range base {
			f[k] = v
		}
		if c.v != "" {
			f["needs_review"] = c.v
		}
		tr, err := ApplyAdvance(item(Review), Advance{Outcome: Approved, Fields: f, Agent: "reviewer"}, t0)
		wantCode(t, err, c.code)
		if c.code != "" {
			continue
		}
		if (tr.Item.Risk == RiskHold) != c.hold || tr.Item.Stage != Land {
			t.Errorf("needs_review %q: risk %s stage %s", c.v, tr.Item.Risk, tr.Item.Stage)
		}
		if c.hold && !strings.Contains(tr.Item.HoldReason, "review: marked needs review") {
			t.Errorf("hold reason %q", tr.Item.HoldReason)
		}
	}
}

// Land's back_to_build for red CI names the head it saw: it counts as a bounce, and the item
// remembers the head so a build that changes nothing does not cycle (docs/decisions/2026-10-15-ci-red-unchanged-head.md).
func TestCIRedBackToBuildCounts(t *testing.T) {
	ci := func(head string) Advance {
		return Advance{Outcome: BackToBuild, Agent: "lander", Fields: map[string]string{"reason": "CI failed on `0123456`: check", "ci_head_sha": head}}
	}
	tr, err := ApplyAdvance(item(Land), ci(sha1), t0)
	wantCode(t, err, "")
	if tr.Item.Stage != Build || tr.Item.BounceCount != 1 || tr.Item.CIRedSHA != sha1 || tr.Data["bounce_count"] != 1 {
		t.Fatalf("first CI bounce: %+v %v", tr.Item, tr.Data)
	}
	second := item(Land)
	second.BounceCount = 1
	tr, err = ApplyAdvance(second, ci(sha1), t0)
	wantCode(t, err, "")
	if tr.Item.Stage != NeedsYou || tr.Item.ReturnStage != Build || tr.Item.BounceCount != 2 {
		t.Fatalf("second bounce: %+v", tr.Item)
	}
	_, err = ApplyAdvance(item(Land), ci("HEAD"), t0)
	wantCode(t, err, CodeInvalidField)

	// A conflict or missing-evidence back_to_build names no CI head and is not counted.
	tr, err = ApplyAdvance(item(Land), Advance{Outcome: BackToBuild, Fields: map[string]string{"reason": "conflict"}}, t0)
	wantCode(t, err, "")
	if tr.Item.Stage != Build || tr.Item.BounceCount != 0 || tr.Item.CIRedSHA != "" {
		t.Fatalf("conflict bounce: %+v", tr.Item)
	}
}

func TestPROpenedOnTheCIRedHead(t *testing.T) {
	it := item(Build)
	it.BounceCount, it.CIRedSHA = 1, sha2
	tr, err := ApplyAdvance(it, prOpened(sha2, []string{"M internal/x.go"}, nil), t0)
	wantCode(t, err, "")
	if tr.Item.Stage != NeedsYou || tr.Item.ReturnStage != Land {
		t.Fatalf("same head: %+v", tr.Item)
	}
	if want := "CI failed on fedcba9 and the build changed nothing: rerun CI, fix main, or drop"; tr.Item.Question != want {
		t.Fatalf("question %q", tr.Item.Question)
	}
	if tr.Item.HeadSHA != sha2 {
		t.Fatalf("head %q", tr.Item.HeadSHA)
	}

	// A new head goes to review and clears the mark.
	tr, err = ApplyAdvance(it, prOpened(sha1, []string{"M internal/x.go"}, nil), t0)
	wantCode(t, err, "")
	if tr.Item.Stage != Review || tr.Item.CIRedSHA != "" {
		t.Fatalf("new head: %+v", tr.Item)
	}
}

// A qwen build's session is kept for the next build to resume; the second strike (a review
// bounce, a verify failure or red CI, counted as bounces are) flips the builder to claude for
// the rest, with a builder_fallback note (docs/decisions/2026-10-16-qwen-builder.md).
func TestQwenBuilderStrikes(t *testing.T) {
	qwen := func(s Stage) Item {
		it := item(s)
		it.Builder = BuilderQwen
		return it
	}
	built := func(sess string) map[string]string {
		return map[string]string{"builder": BuilderQwen, "session_id": sess}
	}
	// pr_opened from a qwen run stores its session.
	it := qwen(Build)
	tr, err := ApplyAdvance(it, prOpened(sha2, []string{"M internal/x.go"}, built("ses_1")), t0)
	wantCode(t, err, "")
	if tr.Item.BuildSession != "ses_1" || tr.Item.Builder != BuilderQwen || len(tr.Also) != 0 {
		t.Fatalf("pr_opened: session %q builder %q also %v", tr.Item.BuildSession, tr.Item.Builder, tr.Also)
	}
	// A claude run (the runner had no qwen config) stores none.
	tr2, _ := ApplyAdvance(it, prOpened(sha2, []string{"M internal/x.go"}, map[string]string{"builder": BuilderClaude, "session_id": "c1"}), t0)
	if tr2.Item.BuildSession != "" {
		t.Fatalf("a claude run's session was stored: %q", tr2.Item.BuildSession)
	}

	// First strike: back to build, still qwen, session kept for the resume.
	rv := tr.Item
	rv.Stage = Review
	tr, err = ApplyAdvance(rv, Advance{Outcome: Bounce, Fields: map[string]string{"notes": "fix x"}, Agent: "r"}, t0)
	wantCode(t, err, "")
	if tr.Item.Stage != Build || tr.Item.Builder != BuilderQwen || tr.Item.BuildSession != "ses_1" || len(tr.Also) != 0 {
		t.Fatalf("first strike: %+v also %v", tr.Item, tr.Also)
	}

	// Second strike (here a verify failure of the resumed session): claude from now on.
	f := built("ses_1")
	f["reason"] = "verify failed"
	tr, err = ApplyAdvance(tr.Item, Advance{Outcome: VerifyFailed, Fields: f, Evidence: 1}, t0)
	wantCode(t, err, "")
	if tr.Item.Builder != BuilderClaude || tr.Item.BuildSession != "" || tr.Item.Stage != NeedsYou || tr.Item.ReturnStage != Build {
		t.Fatalf("second strike: %+v", tr.Item)
	}
	if len(tr.Also) != 1 || tr.Also[0].Kind != "builder_fallback" || tr.Also[0].Data["from"] != BuilderQwen ||
		tr.Also[0].Data["to"] != BuilderClaude || tr.Also[0].Data["bounce_count"] != 2 {
		t.Fatalf("also: %+v", tr.Also)
	}
	if tr.Data["builder_fallback"] != true {
		t.Fatalf("data: %v", tr.Data)
	}

	// A claude item's second bounce changes no builder.
	cl := item(Review)
	cl.Builder, cl.BounceCount = BuilderClaude, 1
	tr, _ = ApplyAdvance(cl, Advance{Outcome: Bounce, Fields: map[string]string{"notes": "x"}, Agent: "r"}, t0)
	if tr.Item.Builder != BuilderClaude || len(tr.Also) != 0 {
		t.Fatalf("claude item: %+v %v", tr.Item, tr.Also)
	}
	// Red CI counts as a strike too.
	ci := qwen(Land)
	ci.BounceCount, ci.BuildSession = 1, "ses_9"
	tr, _ = ApplyAdvance(ci, Advance{Outcome: BackToBuild, Agent: "lander", Fields: map[string]string{"reason": "CI failed", "ci_head_sha": sha1}}, t0)
	if tr.Item.Builder != BuilderClaude || tr.Item.BuildSession != "" || len(tr.Also) != 1 {
		t.Fatalf("CI strike: %+v %v", tr.Item, tr.Also)
	}
}
