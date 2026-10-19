# Only trusted authors' GitHub text reaches an agent

2026-10-19 · item #280. jarvis-server, jarvis-node-mobile, jarvis-node-setup and other loop repos
are public, so any GitHub user can comment on a loop PR. Until now a build run after a bounce got
every PR conversation comment, review and inline comment, word for word, in
`.loop/review/pr-comments.md`, and a review run got the PR's title and body in
`.loop/context/pr.md`. Those agents edit code that the runner then pushes, so a stranger's comment
was a prompt-injection path. Enforced by `TestTrusted` (`runner/config_test.go`),
`TestBounceContextLeavesOutStrangers` and `TestReviewLeavesOutStrangersPRBody`
(`runner/stages_test.go`).

## What was chosen

- **Trusted authors.** A login is trusted for a repo when it is the repo's owner (the slug's
  `owner/` part) or is in the runner config's `trusted_authors` (for a GitHub App or bot, its
  `name[bot]` login). Logins match case-insensitively, as GitHub's do. An empty login is not trusted.
  The runner posts its own review comments and opens its PRs with the user's `gh` login, so on the
  user's repos the default (the owner alone) already covers the loop's own text.
- **Comments.** Comments, reviews and inline comments from anyone else are dropped before the file
  is written: never quoted, not even their login or path. The file says how many were dropped
  (`N comments from other authors left out.`), so the agent knows it is not seeing everything.
- **PR title and body.** `gh pr view` now also reads the PR's `author`. When the PR was not opened
  by a trusted author (an adopted PR, say), the review's `pr.md` leaves the title and body out and
  says who opened it.

## What else was checked

- Land reads the PR body (`land_gh.go`) only to check the Verification footer; it never reaches an
  agent. Drop and the PR check read state only. Triage's dedupe reads merged PRs' titles, which
  only the user's merges produce.
- Nothing in the runner reads issue text from GitHub.
- Commit messages reach an agent only through git in its checkout (`git log`), on branches the loop
  made or the base branch; not covered here.
- Not checked: whether an agent can run `gh` with the user's credentials itself (it has network to
  GitHub) and read the comments directly. If so, that is a separate item.
