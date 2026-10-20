package runner

import (
	"context"
	"fmt"
	"strings"
)

// The runner's merge of the base before a build (docs/decisions/2026-10-20-runner-merges-protected-paths.md):
// Claude Code's sandbox keeps some of the worktree read-only (.claude/skills, .claude/hooks,
// .claude/settings*.json), so an agent's own `git merge origin/<base>` aborts as a whole once the
// base changed anything there. The runner, outside the sandbox, does that merge before the agent
// starts.

// sandboxProtected are the path prefixes an agent cannot write in its worktree; the runner's
// pre-merge takes the base's side of a conflict under them.
var sandboxProtected = []string{".claude/"}

func isSandboxProtected(name string) bool {
	for _, p := range sandboxProtected {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// PreMerge is what the runner's merge of the base did ("" SHA: nothing, no merge was needed).
type PreMerge struct {
	// SHA is the base's head that was merged.
	SHA string
	// Taken are the protected files whose conflicts took the base's side.
	Taken []string
	// Conflicts are the other conflicted files, left for the agent: the merge is still in
	// progress (Committed false) when there are any.
	Conflicts []string
	// Committed: the merge commit is made.
	Committed bool
}

// Evidence is the line the run keeps of the merge.
func (p PreMerge) Evidence(base string) string {
	s := fmt.Sprintf("runner merged origin/%s at %s; protected paths taken from %s", base, short7(p.SHA), base)
	if len(p.Taken) > 0 {
		s += " (" + strings.Join(p.Taken, ", ") + ")"
	}
	if len(p.Conflicts) > 0 {
		s += "; left in progress for the agent: " + strings.Join(p.Conflicts, ", ")
	}
	return s
}

// PreMerge merges origin/<base> into the worktree wt's branch when the branch is behind it and
// the base's change since their merge base touches a sandbox-protected path. A conflict under a
// protected path takes the base's side; any other conflict is left for the agent, the merge in
// progress. On a failure the merge is aborted and the worktree is as it was.
func (g Git) PreMerge(ctx context.Context, wt, base, branch string) (PreMerge, error) {
	baseRef := "refs/remotes/origin/" + base
	if g.isAncestor(ctx, wt, baseRef, "HEAD") {
		return PreMerge{}, nil
	}
	mb, err := g.run(ctx, wt, "merge-base", baseRef, "HEAD")
	if err != nil {
		return PreMerge{}, err
	}
	changed, err := g.run(ctx, wt, "-c", "core.quotePath=false", "diff", "--name-only", "--no-renames", mb, baseRef)
	if err != nil {
		return PreMerge{}, err
	}
	if !touchesProtected(strings.Split(changed, "\n")) {
		return PreMerge{}, nil
	}
	sha, err := g.run(ctx, wt, "rev-parse", baseRef)
	if err != nil {
		return PreMerge{}, err
	}
	p := PreMerge{SHA: sha}
	ident := []string{"-c", "user.name=jarvis-loop", "-c", "user.email=jarvis-loop@localhost"}
	msg := fmt.Sprintf("Merge %s into %s (loop runner, before the build)", base, branch)
	abort := func(err error) (PreMerge, error) {
		_, _ = g.run(context.WithoutCancel(ctx), wt, "merge", "--abort")
		return PreMerge{}, err
	}
	if _, err := g.run(ctx, wt, append(ident, "merge", "--no-ff", "--no-edit", "-m", msg, baseRef)...); err == nil {
		p.Committed = true
		return p, nil
	} else if !g.hasRef(ctx, wt, "MERGE_HEAD") {
		return PreMerge{}, err // git refused to start it (nothing to abort)
	}
	out, err := g.run(ctx, wt, "-c", "core.quotePath=false", "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return abort(err)
	}
	for _, f := range strings.Split(out, "\n") {
		switch {
		case f == "":
		case isSandboxProtected(f):
			if err := g.takeTheirs(ctx, wt, f); err != nil {
				return abort(err)
			}
			p.Taken = append(p.Taken, f)
		default:
			p.Conflicts = append(p.Conflicts, f)
		}
	}
	if len(p.Conflicts) == 0 {
		if _, err := g.run(ctx, wt, append(ident, "commit", "-q", "--no-edit", "--no-verify")...); err != nil {
			return abort(err)
		}
		p.Committed = true
	}
	return p, nil
}

// takeTheirs resolves a conflicted file to the base's side: its version there, or its removal
// when the base deleted it.
func (g Git) takeTheirs(ctx context.Context, wt, f string) error {
	if _, err := g.run(ctx, wt, "cat-file", "-e", "MERGE_HEAD:"+f); err != nil {
		_, err = g.run(ctx, wt, "rm", "-q", "--", f)
		return err
	}
	if _, err := g.run(ctx, wt, "checkout", "MERGE_HEAD", "--", f); err != nil {
		return err
	}
	_, err := g.run(ctx, wt, "add", "--", f)
	return err
}

func touchesProtected(paths []string) bool {
	for _, f := range paths {
		if isSandboxProtected(f) {
			return true
		}
	}
	return false
}
