# Phase 4: the runner and the build stage

Goal (DESIGN.md, Rollout 4): one runner on the dev box claims `build` items and runs the build
brief with a headless `claude -p`, unattended, producing a PR with evidence. Triage and review are
still moved by hand with the `loop` CLI; landing is still by hand. **Proves:** an unattended agent
produces a PR with evidence, inside hard boundaries.

**As built (2026-10-15):** `docs/decisions/2026-10-15-claude-headless-facts.md` (what the installed
CLI really offers, and where this spec was wrong), `2026-10-15-runner-choices.md` (the layers as
implemented, and the harness/sandbox conflict) and `2026-10-15-prompt-caching.md`. **Changed after
the first real run:** `2026-10-15-runner-pushes.md`: the agent holds no GitHub credential; it
commits and returns `ready_for_pr`, and the runner pushes the branch and opens the PR.

## The boundaries (the point of this phase)

An unattended agent on the user's own machine gets four independent layers. Each must hold on its
own; tests prove each one.

1. **The agent holds no loopd credential, and no GitHub credential.** The runner claims,
   heartbeats, uploads evidence and advances; it also pushes the item's branch and opens the PR,
   with the user's git and gh login, outside the sandbox. The agent only writes files in its worktree: `.loop/result.json` and
   `.loop/evidence/*`. The runner validates the result against the stage's outcomes before
   reporting it. A misbehaving agent cannot advance, approve or touch another item.
2. **Permissions** (`--settings <generated file>` per run):
   - `permission-mode`: `auto` (the classifier, as in the user's own sessions), with explicit
     `deny` rules, which are evaluated before the classifier and always block:
     `gh pr merge`, `gh pr review --approve`, `gh repo delete`, `gh api` with `-X DELETE|PUT|PATCH`
     on repo settings, `git push` (all of it: the runner pushes), `gh pr create|edit` (the runner
     opens the PR), `adb` (all; emulator work comes via the harness scripts, which pass `-s`),
     `sudo`, `pkexec`, `systemctl`, `kill`/`pkill`/`killall` of anything except `kill -0 <pid>`
     (a liveness check; cleanup is the runner's),
     `rm -rf` outside the worktree, `curl|wget` to the loopd address, `ssh`, `scp`.
   - Fallback documented and selectable (`--mode strict`): `dontAsk` + an allowlist (go, npm/npx in
     the worktree, git, gh pr view/checks, the harness scripts), for when auto mode is
     unavailable.
3. **A PreToolUse hook** (a small Go binary from this repo, `cmd/loop-guard`, configured in the
   generated settings) that independently refuses, with a reason the agent sees:
   - any Write/Edit/MultiEdit/NotebookEdit whose path resolves (symlinks too) outside the item's
     worktree, its sandbox data dir, or the scratch dir the runner gives it;
   - any Bash command that mentions the user's checkouts by path (`~/jarvis/<repo>` main
     checkouts), `emulator-5554`, the loopd token file, `~/.config/jarvis-harness/` (Slack and
     installed-app credentials), or any deny pattern above (a second line of defence if a deny rule
     is ever loosened);
   - it logs each refusal to the runner, which records it as an item event (`guard_refused`).
4. **The OS sandbox** (Claude Code's built-in sandbox for Bash, Linux): `enabled: true`, writes
   only to the worktree, the item's `.sandbox/` dir, `$TMPDIR`, and the build caches
   (`~/.cache/go-build`, `~/go/pkg/mod`, `~/.npm`, `~/.cache/jarvis-harness` read-mostly); reads
   denied to `~/.ssh`, `~/.config/jarvis-harness`, `~/.config/jarvis-loop`, the loopd data dir;
   network to github.com, api.github.com, the Go proxy and sum DB, the npm registry, and localhost
   binding (sandboxed jarvisd). **Verify** each of these against the installed Claude Code's
   sandbox options; if the sandbox blocks something the harness genuinely needs, record exactly
   what and why in a decision file and keep the other three layers. Do not silently disable it.

Not used: `bypassPermissions`. Check `--bare` before using it: it may skip the OAuth login the
agents need (subscription auth). **Every flag and setting named here must be confirmed against the
installed `claude --help` and its docs before use; the summary this spec was written from was
partly inferred.**

## The runner (`cmd/loop-runner`)

- Config file (0600): loopd URL, the runner token, name, capabilities, `active` slots it may use
  (it still obeys loopd's active-agents number), repos map (`jarvis-server` → its local clone used
  only as a git object source, and the GitHub remote), work root (`~/.cache/jarvis-loop/work`).
- Loop: claim (only stages this runner runs: `build` in this phase) → prepare → run agent →
  report → cleanup. Several items in parallel up to its slots.
- **Prepare:** a fresh worktree at `<work root>/<item>-<stage>` from `origin/main` (or, when the
  item's branch already has commits in the mirror or on the remote, from that branch: a re-run
  continues the work, and the brief says so; if the two diverged, the run fails) on branch
  `loop/<item>-<slug>` (never in the user's checkout; `git worktree add` against a dedicated bare
  mirror clone the runner owns, not the user's clone). For jarvis-server, fetch the sherpa libs
  into the worktree. Copy the item's evidence so far (triage's notes and repro, from loopd's
  `GET /items/{id}/evidence/{n}`) into `.loop/triage/`. Write `.loop/brief.md`: the stage skill
  (see below), the item (title, ask, priority, triage notes and the `.loop/triage/` files, any
  plan link), and the result contract.
- **Run:** `claude -p` with the brief as the prompt, `--output-format stream-json`,
  `--settings <generated>`, the worktree as cwd, its own process group, the per-run time cap from
  loopd's control (45 min), heartbeats every 60 s while it runs. On `stop: true` from a heartbeat:
  SIGTERM the group, then SIGKILL after 10 s; commit and push whatever is in the worktree to the
  item's branch as `WIP` (only that branch), and report nothing (the item returns to its queue).
- **Report:** read `.loop/result.json` (schema below). On `ready_for_pr`: refuse (`failed`, with
  the paths) if the worktree has uncommitted changes; push **only** `loop/<item>-<slug>` from the
  mirror to the repo's remote (`git push origin refs/heads/<branch>:refs/heads/<branch>`, never
  forced, any other ref refused; a non-fast-forward fails the item); check the remote now has
  HEAD; open the PR (`gh pr create --repo <slug> --head <branch> --base <base> --title …
  --body-file …`), or update the open PR for that branch (`gh pr edit`); verify it (`gh pr view`:
  open, head ref and oid); upload `.loop/evidence/*` (≤ 20 MB each, ≤ 20 files), then `advance`
  `pr_opened` with the PR number. Malformed or missing result → `failed` with the agent's last output
  (clipped). A usage/rate-limit failure (from the stream-json events or the final result: match
  the documented error shapes, keep the matcher tested) → `usage_limit` with the reset text.
  Record `num_turns`, duration and `total_cost_usd` (an estimate) on the advance for the digest.
- **Cleanup:** remove the worktree (after the push), stop any sandbox the agent started under the
  item's sandbox name (`scripts/sandbox down --name loop-<item> --purge`), and any emulator it
  started (only by the harness's own stop command). Never kill a process it did not start.

## The build brief (`skills/build/SKILL.md` in this repo)

What the agent is told, in short: implement the item test-first in this worktree only; follow the
target repo's `CLAUDE.md` and skills (e.g. `land-pr`'s gate, but **not** its merge step); verify
with the target repo's harness (sandbox name `loop-<item>`); commit everything on the item's
branch; write `.loop/result.json` with the PR's title and body (evidence named by file name;
screenshots from seeded sandboxes only); never push, open a PR, merge or approve; if blocked or
unsure, return outcome `question` with a precise question instead of guessing.

`.loop/result.json`:

```json
{"outcome": "ready_for_pr", "pr_title": "cc: fix the timer rounding",
 "pr_body": "markdown, at most 60,000 characters; evidence by file name",
 "summary": "one paragraph", "not_verified": ["…"], "evidence": [{"file": "evidence/after.png", "label": "after, phone"}],
 "needs_review": false}
```

or `{"outcome": "question", "question": "…"}`. `head_sha` is optional (if given, it must be HEAD).
The runner computes `changed_paths` and `head_sha` itself from git (`git diff --name-status
origin/main...HEAD`, `git rev-parse HEAD`); an agent's word is not trusted for the hold rules. It
appends a **Verification** footer to the body: the evidence files (uploaded to the item) and the
run's turns, prompt-cache hit ratio, duration and estimated cost. `pr_opened` is the runner's
outcome to loopd, never the agent's.

## Tests (first)

- Guard hook: a table of tool calls (paths inside/outside the worktree incl. `..` and symlinks,
  each denied command family, the protected paths) → allow/deny with reason.
- Settings generation: the deny list and sandbox config are present and well-formed; a golden file.
- Result validation: good result, missing fields, an outcome the stage does not allow, agent paths
  or sha disagreeing with git → rejected.
- Runner with a **fake `claude`** (a test binary that writes a scripted result or misbehaves:
  hangs, exits non-zero, emits a usage-limit event, ignores SIGTERM) against an httptest loopd:
  claim → prepare (against a temp git repo with a bare remote) → run → report (the runner's push to
  the bare remote; PRs through a test double); heartbeats on a fake
  clock; stop mid-run kills the group and pushes WIP; usage limit → `usage_limit`; time cap.
- Usage-limit matcher: fixtures of the documented shapes.
- No real Claude, GitHub or Slack in tests.

## Real run (only after the user approves the boundaries)

Do **not** run a real agent on the user's machine until the user has read the boundaries above
and said go. Prepare everything (build, a dry run with the fake claude end to end against a real
local loopd), then stop and report. The first real item will be a small, safe, real change in
**this** repo (e.g. "add `loop version`"), not jarvis-server.
