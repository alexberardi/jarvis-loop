package runner

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Carrying a held approval over a merge of the base (docs/decisions/2026-10-18-approval-carry.md):
// the user approved a held PR at one commit; land or a build then merged origin/<base> into the
// branch to resolve a conflict. Review computes what changed between the two heads; loopd moves
// the approval to the new head only when the change is nothing but those merges, each one git's
// own merge except inside its conflict hunks, and no conflicted file is on the hold list.

// ApprovalDelta is what changed on a PR between the head the user approved (From) and a later
// head (To).
type ApprovalDelta struct {
	From, To string
	// Merges are the merges of the base on the branch since From, oldest first.
	Merges []string
	// Conflicts are the files those merges had conflicts in, resolved by the builder or land.
	Conflicts []string
	// Changed are files where a merge is not git's own merge outside the conflict hunks: the
	// PR's own lines changed.
	Changed []string
	// Problem is why the change is not merges of the base at all ("" when it is): a new commit,
	// a rebase, a merge of another branch, or a git failure.
	Problem string
	// Diff is each merge's remerge diff (git's merge, conflict markers in, against the
	// resolution), clipped: the delta's evidence.
	Diff string
}

// Carryable reports whether the approval may move to To on this delta, before loopd's own check
// of the conflicted files against the hold rules.
func (d ApprovalDelta) Carryable() bool {
	return d.Problem == "" && len(d.Changed) == 0 && len(d.Merges) > 0
}

// MaxDeltaDiff caps the remerge diff a delta keeps.
const MaxDeltaDiff = 40_000

// ApprovalDelta computes the delta from from to to in dir (the mirror, fetched), base being the
// base branch's name.
func (g Git) ApprovalDelta(ctx context.Context, dir, base, from, to string) ApprovalDelta {
	d := ApprovalDelta{From: from, To: to}
	baseRef := "refs/remotes/origin/" + base
	if !g.hasRef(ctx, dir, from+"^{commit}") {
		d.Problem = fmt.Sprintf("the approved commit %s is not in the mirror", short7(from))
		return d
	}
	if !g.isAncestor(ctx, dir, from, to) {
		d.Problem = fmt.Sprintf("the approved commit %s is not in the new head's history (the branch was rebased or rewritten)", short7(from))
		return d
	}
	own, err := g.run(ctx, dir, "rev-list", "--no-merges", to, "^"+from, "^"+baseRef)
	if err != nil {
		d.Problem = "git: " + clip(err.Error(), 300)
		return d
	}
	if own = strings.TrimSpace(own); own != "" {
		var shas []string
		for _, s := range strings.Fields(own) {
			shas = append(shas, short7(s))
		}
		d.Problem = "commits on the branch since the approval that are not a merge of origin/" + base + ": " + strings.Join(shas, ", ")
		return d
	}
	merges, err := g.run(ctx, dir, "rev-list", "--merges", "--reverse", to, "^"+from, "^"+baseRef)
	if err != nil {
		d.Problem = "git: " + clip(err.Error(), 300)
		return d
	}
	d.Merges = strings.Fields(merges)
	if len(d.Merges) == 0 {
		d.Problem = "no merge of origin/" + base + " since the approval"
		return d
	}
	var diff strings.Builder
	for _, m := range d.Merges {
		line, err := g.run(ctx, dir, "rev-list", "--parents", "-n", "1", m)
		if err != nil {
			d.Problem = "git: " + clip(err.Error(), 300)
			return d
		}
		parents := strings.Fields(line)[1:]
		if len(parents) != 2 {
			d.Problem = fmt.Sprintf("merge %s has %d parents", short7(m), len(parents))
			return d
		}
		if !g.isAncestor(ctx, dir, parents[1], baseRef) {
			d.Problem = fmt.Sprintf("merge %s merges %s, which is not on origin/%s", short7(m), short7(parents[1]), base)
			return d
		}
		conflicts, changed, err := g.mergeDelta(ctx, dir, m, parents[0], parents[1])
		if err != nil {
			d.Problem = "git: " + clip(err.Error(), 300)
			return d
		}
		for _, f := range conflicts {
			if !slices.Contains(d.Conflicts, f) {
				d.Conflicts = append(d.Conflicts, f)
			}
		}
		for _, f := range changed {
			if !slices.Contains(d.Changed, f) {
				d.Changed = append(d.Changed, f)
			}
		}
		if rd, err := g.run(ctx, dir, "show", "--no-color", "--no-ext-diff", "--remerge-diff", "--format=### merge %h: %s", m); err == nil {
			diff.WriteString(rd + "\n")
		}
	}
	slices.Sort(d.Conflicts)
	slices.Sort(d.Changed)
	d.Diff = clip(diff.String(), MaxDeltaDiff)
	return d
}

// mergeDelta redoes merge m of p1 and p2 with git's merge-tree: the files that conflict, and the
// files where m differs from git's merge other than inside a conflict hunk.
func (g Git) mergeDelta(ctx context.Context, dir, m, p1, p2 string) (conflicts, changed []string, err error) {
	// Exit 1 is a merge with conflicts: the tree (markers in) and the conflicted files are printed.
	out, mErr := g.run(ctx, dir, "merge-tree", "--write-tree", "--name-only", "--no-messages", p1, p2)
	lines := strings.Split(out, "\n")
	if len(lines) == 0 || !shaRe.MatchString(strings.TrimSpace(lines[0])) {
		if mErr == nil {
			mErr = fmt.Errorf("merge-tree printed no tree")
		}
		return nil, nil, mErr
	}
	tree := strings.TrimSpace(lines[0])
	for _, l := range lines[1:] {
		if l = strings.TrimSpace(l); l != "" && !slices.Contains(conflicts, l) {
			conflicts = append(conflicts, l)
		}
	}
	names, err := g.run(ctx, dir, "diff", "--name-only", "--no-renames", tree, m)
	if err != nil {
		return nil, nil, err
	}
	for _, f := range strings.Fields(names) {
		if !slices.Contains(conflicts, f) {
			changed = append(changed, f)
			continue
		}
		merged, err1 := g.run(ctx, dir, "cat-file", "blob", tree+":"+f)
		resolved, err2 := g.run(ctx, dir, "cat-file", "blob", m+":"+f)
		if err1 != nil || err2 != nil || !keptOutsideConflicts(merged, resolved) {
			changed = append(changed, f)
		}
	}
	return conflicts, changed, nil
}

// keptOutsideConflicts reports whether resolved keeps every line of merged (a file with git's
// conflict markers) outside the conflict hunks, in order, with only the hunks replaced.
func keptOutsideConflicts(merged, resolved string) bool {
	var segs [][]string
	cur, in := []string{}, false
	for _, l := range splitLines(merged) {
		switch {
		case !in && strings.HasPrefix(l, "<<<<<<< "):
			segs, cur, in = append(segs, cur), []string{}, true
		case in && strings.HasPrefix(l, ">>>>>>> "):
			in = false
		case !in:
			cur = append(cur, l)
		}
	}
	segs = append(segs, cur)
	res := splitLines(resolved)
	if len(segs) == 1 {
		return slices.Equal(segs[0], res)
	}
	first, last := segs[0], segs[len(segs)-1]
	if len(res) < len(first)+len(last) || !slices.Equal(res[:len(first)], first) || !slices.Equal(res[len(res)-len(last):], last) {
		return false
	}
	rest := res[len(first) : len(res)-len(last)]
	for _, s := range segs[1 : len(segs)-1] {
		i := indexLines(rest, s)
		if i < 0 {
			return false
		}
		rest = rest[i+len(s):]
	}
	return true
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// indexLines is the first index of sub in lines, or -1.
func indexLines(lines, sub []string) int {
	for i := 0; i+len(sub) <= len(lines); i++ {
		if slices.Equal(lines[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

// ApprovalDeltaFile is the review evidence file with the delta.
const ApprovalDeltaFile = "approval-delta.md"

// ApprovalDeltaDoc is the delta as the review's context and evidence file.
func ApprovalDeltaDoc(d ApprovalDelta, by, base string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Approval delta: `%s` → `%s`\n\n%s approved `%s`; the PR's head is now `%s`. ", short7(d.From), short7(d.To), by, short7(d.From), short7(d.To))
	fmt.Fprintf(&b, "loopd carries the approval to the new head only if the change is merges of origin/%s alone, each git's own merge outside its conflict hunks, with no conflict in a file on the hold list, and the review approves without marking it needs review.\n\n", base)
	list := func(name string, xs []string, short bool) {
		if len(xs) == 0 {
			fmt.Fprintf(&b, "- %s: none\n", name)
			return
		}
		var s []string
		for _, x := range xs {
			if short {
				x = short7(x)
			}
			s = append(s, "`"+x+"`")
		}
		fmt.Fprintf(&b, "- %s: %s\n", name, strings.Join(s, ", "))
	}
	if d.Problem != "" {
		fmt.Fprintf(&b, "- **Not only merges of origin/%s:** %s\n", base, d.Problem)
	}
	list("Merges of origin/"+base, d.Merges, true)
	list("Files with conflicts (resolved)", d.Conflicts, false)
	list("Files changed outside the conflict hunks (the PR's own lines)", d.Changed, false)
	verdict := "carryable"
	if !d.Carryable() {
		verdict = "not carryable"
	}
	fmt.Fprintf(&b, "- Git's verdict: **%s** (loopd checks the conflicted files against the hold rules)\n", verdict)
	if d.Diff != "" {
		b.WriteString("\n## Each merge against git's own merge (`git show --remerge-diff`)\n\n```diff\n" + strings.ReplaceAll(d.Diff, "```", "` ` `") + "\n```\n")
	}
	return b.String()
}
