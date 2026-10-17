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
type Trust struct {
	// HeadSHA is the commit verify refused, and the only one the OK lets it run.
	HeadSHA string `json:"head_sha"`
	// Paths are the trusted files the commit changes; Diffstat is git's --stat of them.
	Paths    []string `json:"paths,omitempty"`
	Diffstat string   `json:"diffstat,omitempty"`
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
// its trusted files; trust_used names the commit a verify ran under the OK.
const (
	FieldTrustSHA      = "trust_sha"
	FieldTrustPaths    = "trusted_paths"
	FieldTrustDiffstat = "trusted_diffstat"
	FieldTrustUsed     = "trust_used"
)

// applyTrustFields records what an advance from build says about trust: the OK used by a verify
// (it must be the item's OK, for that commit; only a verdict on the code uses it up), and a new
// refusal waiting for an OK.
func applyTrustFields(it *Item, from Stage, o Outcome, f fields, data map[string]any) error {
	_, used := f[FieldTrustUsed]
	_, refused := f[FieldTrustSHA]
	if (used || refused) && from != Build {
		return errf(CodeInvalidField, "trust fields are for a build's verify, not %s", from)
	}
	if used {
		sha, err := f.sha(FieldTrustUsed)
		if err != nil {
			return err
		}
		if !TrustAllows(it.Trust, sha) {
			return errf(CodeInvalidField, "%s %s is not a commit the approver trusted (or its OK was used)", FieldTrustUsed, sha)
		}
		if o == PROpened && strings.TrimSpace(f["head_sha"]) != sha {
			return errf(CodeSHAMismatch, "verify ran %s under the OK but the PR head is %s", sha, f["head_sha"])
		}
		if usesTrust(o) {
			t := *it.Trust
			t.Used = true
			it.Trust = &t
		}
	}
	if refused {
		if o != Failed {
			return errf(CodeInvalidField, "%s comes with failed (verify's refusal), not %s", FieldTrustSHA, o)
		}
		sha, err := f.sha(FieldTrustSHA)
		if err != nil {
			return err
		}
		var paths []string
		for _, p := range strings.Split(f[FieldTrustPaths], ",") {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, p)
			}
		}
		if len(paths) == 0 {
			return errf(CodeMissingField, "%s is required with %s", FieldTrustPaths, FieldTrustSHA)
		}
		it.Trust = &Trust{HeadSHA: sha, Paths: paths, Diffstat: strings.TrimSpace(f[FieldTrustDiffstat])}
		data["trust_needed"] = true
	}
	return nil
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
