package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
)

// trustedDiff is the diff of head's trusted files against the merge base with the base branch:
// what the refusal attaches for the user (uncut), and what a carried OK must match byte for byte.
// Full blob ids and real binary patches, with no textconv or external driver, so the digest binds
// the bytes even of a binary trusted file or one the branch marks -diff in .gitattributes (whose
// default diff names only "Binary files differ" and abbreviated, brute-forceable ids).
func (r *Runner) trustedDiff(ctx context.Context, ru *run, head string, paths []string) (string, error) {
	rng := "refs/remotes/origin/" + ru.repo.Base + "..." + head
	return r.Git.run(ctx, ru.wt, append([]string{"diff", "--full-index", "--binary", "--no-textconv", "--no-ext-diff", rng, "--"}, paths...)...)
}

func diffSHA256(diff string) string {
	sum := sha256.Sum256([]byte(diff))
	return hex.EncodeToString(sum[:])
}

// carriedTrust returns the digest of head's trusted files' diff when the approver's OK (given for
// another head, or used) carries to head: the same trusted files, and a byte-identical diff
// against the merge base (#277). "" means verify refuses as before. loopd checks the digest again.
func (r *Runner) carriedTrust(ctx context.Context, ru *run, head string, paths []string) string {
	t := ru.item.Trust
	if t == nil || t.By == "" || t.DiffSHA256 == "" || head == "" {
		return ""
	}
	a, b := slices.Clone(paths), slices.Clone(t.Paths)
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		return ""
	}
	diff, err := r.trustedDiff(ctx, ru, head, paths)
	if err != nil || diff == "" {
		if err != nil {
			r.Log.Warn("trusted files' diff for a carried OK", "item", ru.item.ID, "err", err)
		}
		return ""
	}
	if sum := diffSHA256(diff); sum == t.DiffSHA256 {
		return sum
	}
	return ""
}
