# The runner pushes and opens the PR; agents hold no GitHub credentials

2026-10-15 · after the first real build run (item #1 on jarvis-server). Decided by the reviewer.

## What happened

The first real agent run worked (97% prompt-cache hit, tests green) but ended with a `question`:
inside the OS sandbox it had no GitHub credentials, so `git push` failed, and loop-guard refused
`gh auth status` when it tried to find out why. Its commit survived only on the item's branch in
the runner's mirror. Two smaller findings came with it: the guard refused `kill -0 $(cat x.pid)`,
a liveness check, and a re-run would have started again from `origin/main`, because the worktree
was based on the remote branch only (the commit was never pushed).

## Decision

**Agents hold no GitHub credentials at all.** Pushing and opening PRs move to the runner, which
runs outside the sandbox with the user's own git and gh login.

- **Result contract.** For build, the agent commits on the item's branch and writes
  `.loop/result.json` with outcome `ready_for_pr`: `pr_title` (one line, ≤ 256 characters),
  `pr_body` (markdown, ≤ 60,000 characters, evidence named by file name), `summary`,
  `not_verified`, `evidence`, `needs_review`, optionally `head_sha` (must be HEAD). `question` is
  unchanged. `pr_opened` is no longer an agent outcome: it is what the runner reports to loopd.
- **The runner, after the agent exits:** computes HEAD and the changed paths from git as before;
  **refuses** (`failed`, naming the paths) when the worktree has uncommitted changes. It does not
  commit them as WIP: `ready_for_pr` claims the work is done, and a commit the agent did not make
  would put unreviewed files into a PR. The agent's commits stay in the mirror for a re-run.
- **Push:** only `refs/heads/loop/<item>-<slug>` from the mirror to its `origin` (the configured
  remote), to the same name, never forced (`git push origin refs/heads/B:refs/heads/B`). Anything
  not under `loop/`, with `:`, `+`, spaces or glob characters, or not a valid ref name, is refused
  before git runs. A non-fast-forward is refused by the remote and fails the item; nothing is
  overwritten. The runner then fetches and checks the remote branch is HEAD. (Since 2026-10-19 a
  reviewed rebase over the PR's recorded head is pushed with a lease pinned to that head:
  [decision](2026-10-19-rebase-push-with-lease.md).)
- **PR:** `gh pr list --head <branch> --state open`; if one is open (a bounced item), `gh pr edit
  <n> --title … --body-file …`; otherwise `gh pr create --repo <slug> --head <branch> --base <base>
  --title … --body-file …`. Then the existing check (`gh pr view`: open, head ref, head oid = HEAD),
  then evidence upload and `pr_opened` with the PR number. The body file (in the run dir, never
  the worktree) is the agent's body plus a **Verification** footer: the evidence files with their
  labels, and the run's turns, prompt-cache hit ratio, duration and estimated cost.
- **Inside the agent, both layers deny:** deny rules `Bash(git push)`, `Bash(git push *)`,
  `Bash(gh pr create *)`, `Bash(gh pr edit *)`; the guard refuses every `git push` (whatever the
  remote, branch or flags) and `gh pr create|edit`, with a reason that says the runner does it.
  Strict mode's allowlist drops `gh pr create`.
- **Resume:** the worktree starts from the item's branch when it has commits over the base, in the
  mirror (unpushed work) or on the remote (a WIP push, a bounced PR), whichever contains the
  other. If they diverged, prepare fails rather than lose either side. The brief then says "this
  branch already has N commits from a previous run; check them before redoing work".
- **Triage context:** the item's evidence so far is downloaded (`GET /items/{id}/evidence/{n}`)
  into `.loop/triage/<n>-<name>` (ignored by git, like all of `.loop/`) and listed in the brief.
  A failed copy is said in the brief, not fatal.
- **`kill -0`:** allowed as plain `kill -0 <pid>…` or `kill -0 $(cat <file>)`, where the guard
  reads the file and it must hold exactly one pid at that moment (the substitution's words reach
  `kill` unchecked: a file holding `-s 9 1` would otherwise send SIGKILL). Wrapped (`env kill`,
  `xargs kill`, `/bin/kill`), other signals, `-s 0`, jobs, variables and every other kill stay
  refused. The deny rules cannot say "anything but -0", so `Bash(kill *)` became rules for every
  other first argument (`kill`, `-1*`…`-9*`, `-0<digit>*`, `-<letter>*`, `--*`, a digit, `%`, `$`,
  a quote or backquote, `{`, `(`, `~`, …); a table test checks which spellings they catch.

## Why

- A credential inside the sandbox is a credential the agent can use for anything GitHub allows
  that token: other branches, other repos, settings. The deny rules and the guard narrowed what it
  could run, but the token itself would still be readable to every command. With no token, the
  worst a misbehaving agent can do to GitHub is nothing.
- The runner already held the trust (loopd's token, the mirror, WIP pushes); pushing one known
  branch and opening one PR is a small, fully checked addition there.
- The PR's evidence footer is written by the runner from what it uploaded, so it cannot claim
  evidence that is not there.

## Costs and open points

- The agent cannot read a bounced PR's review comments with `gh` (no credentials, and
  `api.github.com` alone does not help without a token). The brief points at the PR; until the
  review stage writes its comments into the item, a bounced re-run relies on the item's notes.
- The settings changed (deny rules), so the first run after this misses the prompt cache once.
