# The runner merges the base before a build when the base changed .claude/ (2026-10-20)

Item #349. jarvis-server's main 38b8e1b (#356) moved the vendored design skills into
`.claude/skills/`. Claude Code's sandbox keeps `.claude/skills`, `.claude/hooks` and
`.claude/settings*.json` read-only inside the agent's worktree, so an agent's
`git merge origin/main` on any branch from before that commit aborts as a whole
(`Read-only file system`, nothing merged). #248 went to needs_you for it; the user merged by hand.
The same happens for every later change under `.claude/`.

## Decision

Before a build (`prepareBuild`, after the worktree is made and before the run's start head is
taken, so a session resume and the result see it), the runner (`Git.PreMerge`, `premerge.go`),
outside the sandbox, merges `origin/<base>` into the branch when:

- the branch is behind the base, and
- the base's diff since their merge base touches a sandbox-protected path (`sandboxProtected`:
  `.claude/` for now; one list, so it can grow).

A clean merge is committed (`Merge <base> into <branch> (loop runner, before the build)`). A
conflict under a protected path takes the base's side (its version, or its deletion). Any other
conflict is left for the agent with the merge in progress; the brief names those files and says
to `git add` them and `git commit --no-edit`, and not to abort it. With only protected conflicts
the runner commits the merge itself.

The evidence line (`runner merged origin/<base> at <sha7>; protected paths taken from <base>`,
plus the files taken and the ones left) is in the runner's log, the run's `pre_merge` field and
the PR's Verification footer.

## What it does not do

- It does not read notes for "merge main" (review's, loopd's red-main note): matching words is
  fuzzy, and the path trigger alone covers every case the sandbox breaks. A merge that touches no
  protected path the agent can still do itself.
- The branch's own change to a conflicting protected file is dropped in favour of the base's; the
  agent could not have written it either. The brief lists those files.
- Approval carry (`carry.go`) treats the merge like any merge of the base: the taken files are
  among its conflicts, so loopd's hold-rule check of conflicted files applies to them.
