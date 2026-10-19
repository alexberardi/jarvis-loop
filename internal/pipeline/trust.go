package pipeline

import (
	"strings"
	"time"
)

// Trust is the approver's OK for the runner's verify to run one exact commit that changes a file
// the runner trusts (a repo's deny_write paths: the harness verify itself runs). The runner's
// refusal records the commit and its trusted-file diff stat; the approver's OK binds to that sha,
// and it stays valid for that sha until a verify of it reaches a verdict on the code (pr_opened or
// verify_failed); a run that ends otherwise leaves it (docs/decisions/2026-10-16-trusted-files.md).
// A later head whose trusted files' diff is byte-identical carries the OK (#277,
// docs/decisions/2026-10-19-trust-carries-an-unchanged-diff.md).
type Trust struct {
	// HeadSHA is the commit verify refused, and the only one the OK lets it run.
	HeadSHA string `json:"head_sha"`
	// Paths are the trusted files the commit changes; Diffstat is git's --stat of them.
	Paths    []string `json:"paths,omitempty"`
	Diffstat string   `json:"diffstat,omitempty"`
	// DiffSHA256 is the sha256 of the trusted files' diff against the merge base at the refusal:
	// the bytes the user looked at. Empty on an OK recorded before #277 (nothing carries it).
	DiffSHA256 string `json:"diff_sha256,omitempty"`
	// CarriedFrom is the head the OK was for before it carried to HeadSHA (the same diff).
	CarriedFrom string `json:"carried_from,omitempty"`
	// By and At are the approver's OK; empty until given.
	By string    `json:"by,omitempty"`
	At time.Time `json:"at,omitzero"`
	// Used: a verify of the commit under this OK reached a verdict on the code (pr_opened or
	// verify_failed); another run needs another OK. A runner fault, a question or the usage
	// limit leaves it unused.
	Used bool `json:"used,omitempty"`
}

// TrustAllows reports whether the item's trust OK lets verify run head: given by the approver,
// for exactly head, and not used yet.
func TrustAllows(t *Trust, head string) bool {
	return t != nil && t.By != "" && !t.Used && head != "" && t.HeadSHA == head
}

// TrustReason is the hold reason a trust OK adds: land holds the PR for the user.
const TrustReason = "trusted files changed (verify ran the agent's version on the user's OK)"

// The advance fields for trust: the runner's refusal (a failed from build) names the commit and
// its trusted files and their diff's digest; trust_used names the commit a verify ran under the
// OK; trust_carried names a new head the runner carried the OK to, with the digest of its trusted
// files' diff (trusted_diff_sha256), which loopd compares with the one the approver OK'd.
const (
	FieldTrustSHA      = "trust_sha"
	FieldTrustPaths    = "trusted_paths"
	FieldTrustDiffstat = "trusted_diffstat"
	FieldTrustDiffSHA  = "trusted_diff_sha256"
	FieldTrustUsed     = "trust_used"
	FieldTrustCarried  = "trust_carried"
)

// KindTrustCarried is the event an OK carried to a new head records: {from, to, diff_sha256, by}.
const KindTrustCarried = "trust_carried"

// applyTrustFields records what an advance from build says about trust: an OK carried to a new
// head whose trusted files' diff is the one the approver OK'd, the OK used by a verify (it must
// be the item's OK, for that commit; only a verdict on the code uses it up), and a new refusal
// waiting for an OK. It returns the trust_carried note, if any.
func applyTrustFields(it *Item, from Stage, o Outcome, f fields, data map[string]any) ([]Note, error) {
	_, used := f[FieldTrustUsed]
	_, refused := f[FieldTrustSHA]
	_, carried := f[FieldTrustCarried]
	if (used || refused || carried) && from != Build {
		return nil, errf(CodeInvalidField, "trust fields are for a build's verify, not %s", from)
	}
	var notes []Note
	if carried {
		sha, err := f.sha(FieldTrustCarried)
		if err != nil {
			return nil, err
		}
		if refused || strings.TrimSpace(f[FieldTrustUsed]) != sha {
			return nil, errf(CodeInvalidField, "%s comes with %s for the same commit (a verify that ran)", FieldTrustCarried, FieldTrustUsed)
		}
		t := it.Trust
		if t == nil || t.By == "" || t.DiffSHA256 == "" {
			return nil, errf(CodeInvalidField, "%s: the item has no OK with a diff digest to carry", FieldTrustCarried)
		}
		if sum := strings.TrimSpace(f[FieldTrustDiffSHA]); sum != t.DiffSHA256 {
			return nil, errf(CodeInvalidField, "%s %s: its trusted files' diff (%q) is not the one the approver OK'd at %s", FieldTrustCarried, sha, sum, t.HeadSHA)
		}
		c := *t
		c.CarriedFrom, c.HeadSHA, c.Used = t.HeadSHA, sha, false
		it.Trust = &c
		notes = append(notes, Note{Kind: KindTrustCarried, Data: map[string]any{
			"from": t.HeadSHA, "to": sha, "diff_sha256": t.DiffSHA256, "by": t.By}})
	}
	if used {
		sha, err := f.sha(FieldTrustUsed)
		if err != nil {
			return nil, err
		}
		if !TrustAllows(it.Trust, sha) {
			return nil, errf(CodeInvalidField, "%s %s is not a commit the approver trusted (or its OK was used)", FieldTrustUsed, sha)
		}
		if o == PROpened && strings.TrimSpace(f["head_sha"]) != sha {
			return nil, errf(CodeSHAMismatch, "verify ran %s under the OK but the PR head is %s", sha, f["head_sha"])
		}
		if usesTrust(o) {
			t := *it.Trust
			t.Used = true
			it.Trust = &t
		}
	}
	if refused {
		if o != Failed {
			return nil, errf(CodeInvalidField, "%s comes with failed (verify's refusal), not %s", FieldTrustSHA, o)
		}
		sha, err := f.sha(FieldTrustSHA)
		if err != nil {
			return nil, err
		}
		sum := strings.TrimSpace(f[FieldTrustDiffSHA])
		if sum != "" && !isSHA256(sum) {
			return nil, errf(CodeInvalidField, "%s must be a sha256 (64 lowercase hex), not %q", FieldTrustDiffSHA, sum)
		}
		var paths []string
		for _, p := range strings.Split(f[FieldTrustPaths], ",") {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, p)
			}
		}
		if len(paths) == 0 {
			return nil, errf(CodeMissingField, "%s is required with %s", FieldTrustPaths, FieldTrustSHA)
		}
		it.Trust = &Trust{HeadSHA: sha, Paths: paths, Diffstat: strings.TrimSpace(f[FieldTrustDiffstat]), DiffSHA256: sum}
		data["trust_needed"] = true
	}
	return notes, nil
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// usesTrust reports whether a build outcome is a verdict on the code verify ran under the OK: the
// PR opened, or verify found a real regression (a runner fault is failed since #97). Anything else
// (failed, question, usage_limit) never judged the code, so the OK stays valid for its sha.
func usesTrust(o Outcome) bool {
	return o == PROpened || o == VerifyFailed
}

// ApplyTrust records the approver's OK for verify to run headSHA, the commit verify refused for
// changing trusted files. It is valid only while the item waits on that refusal, and only for that
// commit; the item goes back to build, and land holds its PR for the user.
func ApplyTrust(it Item, by, headSHA string, now time.Time) (Transition, error) {
	if it.Stage != NeedsYou {
		return Transition{}, errf(CodeWrongStage, "item is %s, not needs_you", it.Stage)
	}
	if it.Trust == nil || it.Trust.By != "" {
		return Transition{}, errf(CodeWrongStage, "nothing to trust: verify has not refused a trusted-file change on this item")
	}
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return Transition{}, errf(CodeMissingField, "head_sha is required")
	}
	if headSHA != it.Trust.HeadSHA {
		return Transition{}, errf(CodeSHAMismatch, "the OK is for %s but verify refused %s", headSHA, it.Trust.HeadSHA)
	}
	t := *it.Trust
	t.By, t.At = by, now
	it.Trust = &t
	raiseRisk(&it, []string{TrustReason})
	from := it.Stage
	it.Stage, it.ReturnStage, it.Question = Build, "", ""
	it.UpdatedAt = now
	return Transition{Item: it, From: from, To: Build, Kind: "trusted",
		Data: map[string]any{"by": by, "head_sha": headSHA, "paths": t.Paths}}, nil
}
