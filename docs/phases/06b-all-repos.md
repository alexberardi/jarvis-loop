# Phase 6b: every active repo, driven from Slack

Goal (user, 2026-10-15): "drive all current development across the projects from the Slack
interface". The loop works on jarvis-server and jarvis-loop today; this phase adds the two mobile
apps, the docs, the installer and the 29 command/device packages. **Proves:** an item about any of
them goes from a Slack message to a merged PR with the same gates.

**As built (loop side, 2026-10-15):** `docs/decisions/2026-10-15-all-repos.md`. In short: loopd's
`routed` and `verify_failed` outcomes and review's `needs_review`; `--repos-file` with aliases; wildcard
hold-rule repos and the lists below (plus `*Presence*`); built-in rules no longer freeze on a control
change; runner `group`, `aliases`, `setup`, `verify`, `verify_lock`; routing triage; the verify step
with `LOOP_SANDBOX`, `LOOP_REPO`, `LOOP_BASE`, `LOOP_MIRROR` beyond the spec's variables, steps in
another repo's checkout (`repo`) and `when_changed`; a derived short-name alias counts only as a
leading `[x]` or `x:`. The repo-side changes and jarvis-server's mobile-shot are other PRs.

Inputs: the read-only survey of 2026-10-15 (summarised below) and the user's answers the same day,
recorded in `docs/decisions/2026-10-15-all-repos.md`.

## The repos

| Group | Repos | Stack | Verify (agent, in its sandbox) | A merge to main triggers |
| --- | --- | --- | --- | --- |
| mobile | jarvis-node-mobile, jarvis-recipes-mobile | Expo / RN / TS (node 22 / 20) | `npm ci && npx tsc --noEmit && npm test -- --ci` | node-mobile: iOS + Android store builds to TestFlight / Play internal on **every** push; recipes: EAS preview build + TestFlight |
| docs | jarvis-docs | MkDocs (Python) | venv, `pip install -r requirements-dev.txt && pytest` (`mkdocs build --strict`) | nothing (deploy is the manual `deploy.sh`) |
| installer | jarvis-installer | React / Vite (node 24) | `npm ci && npm test && npm run build` | GitHub Pages deploy of installer.jarvisautomation.io, install-e2e dispatch |
| packages | 17 `jarvis-cmd-*`, 12 `jarvis-device-*` (not archived) | Python, `jdt` template | venv, `pip install` the toolkit, `jdt test . --install-deps`, `pytest tests` when present | nothing (Pantry submission and the `v<version>` tag are manual) |

## User decisions (2026-10-15)

1. **Mobile ships every merge.** Keep node-mobile's push-to-main TestFlight / Play internal
   submission; the testers are the user's household. Release config stays on the hold list.
2. **Add PR CI first** where there is none (jarvis-docs, all packages); the land gate keeps
   requiring CI. These PRs touch `.github/` and are held for one batch approval.
3. **Harness on the dev box, run by the runner.** The user: "run the harness here locally … this
   computer is typically on 24/7 and everything for the harness is already set up". Runners already
   live on the dev box (only loopd moves to the prod box in rollout 7). The open question from phase
   4 was *who* runs it; chosen: **the runner runs per-repo verify steps after the build agent,
   outside the agent's sandbox** (option 3 of `2026-10-15-runner-choices.md`). The agent cannot
   skip or fake them; the review agent judges their output.
4. **Packages: tagging and Pantry submission stay human.** The loop merges code only; version,
   secrets, packages, apt_packages and post_install changes are held.

## Loop changes

### Config

- `runner.json` `repos` gains every repo above: `slug`, `remote`, `base`, `description` (one line:
  what it is, so triage can route), optional `local`, and:
  - `setup`: commands the runner runs in the worktree before the agent (outside the sandbox, the
    runner's own step, logged): e.g. `npm ci` with the repo's node version via `mise exec
    node@22 --`, or `python3 -m venv .venv && .venv/bin/pip install …`. Dependencies are then
    present; the agent's sandbox keeps registry access for what it adds.
  - `verify`: the runner's post-build steps (below).
  - `group`: `server`, `loop`, `mobile`, `docs`, `installer`, `package`; used for hold rules and the
    brief.
- `loopd --repos` takes the same slugs (or `--repos-file`, one slug per line, since the list is long).
- Sandbox network adds PyPI (`pypi.org`, `files.pythonhosted.org`) to the defaults; npm and GitHub
  are there already.

### Triage with many repos: route first

Today an item with no repo checks out **every** configured repo for triage. With 35 repos that is
35 mirrors and worktrees. Instead:

- Item has no repo → triage gets **no checkouts**, only `repos.md` (key, slug, group, description,
  last 5 merged PR titles) and the open items, and a brief that says "pick the repo". Its outcome
  is `reproduced` with `repo` as today **only if** it can confirm from that alone (rare), else the
  new runner-level result `routed` `{repo, why}`: the runner reports `routed` to loopd (sets the repo,
  item stays in triage, an event and a thread line "triage: this is jarvis-node-mobile — <why>"),
  and the next triage claim checks out that one repo and triages normally.
- loopd: outcome `routed` (triage → triage, repo required, valid configured slug, only when the item
  has none).
- Cheap: a routing run reads one small file; its prefix is cache-stable like the rest.

### Slack repo words

`/loop add`, mentions: the existing `in <repo>` / `<repo>:` / `[<repo>]` words, plus **aliases**
from config (`mobile`, `node app` → jarvis-node-mobile; `recipes app` → jarvis-recipes-mobile;
`docs`; `installer`; `server`; a package by its suffix, e.g. `[nest]` → jarvis-device-nest when
unique). Only as a leading `[x]` or `x:` or a trailing `in x`, never words in the middle of the
sentence. Table-tested.

**`loop add --repo`** (and the API's `repo`, and the orchestrator's adds) take a slug, a configured
alias, or a repo's bare name (`jarvis-server`, when only one configured repo has it), any case;
loopd stores the configured slug and refuses a repo it is not configured with (400). With no
`--repos` the name is kept as given. The runner's `Config.Repo` also matches a slug's name part,
for rows stored with a bare name before this (#127).

### Hold rules for the new repos

`HoldRule.repo` accepts a trailing `*` (e.g. `alexberardi/jarvis-cmd-*`). Defaults (globs from the
survey):

- **mobile (both):** `eas.json`, `app.json`, `app.config.*`, `.github/**`, `plugins/**`,
  `targets/**`, `native/**`, `ios/**`, `android/**`, `scripts/store/**`, `modules/jarvis-crypto/**`,
  `src/auth/**`, `src/services/tokenStorage.ts`, `src/services/clearUserData.ts`,
  `src/services/oauthService.ts`, `src/services/qr*Service.ts`, `src/services/*presence*`,
  `src/config/env.ts`, `jest.config.js`; deleted or renamed tests (`**/__tests__/**`,
  `**/*.test.ts*`).
- **docs:** `.well-known/**`, `_headers`, `_redirects`, `app-fallback/**`, `deploy.sh`,
  `mkdocs.yml`, `docs/security/**`, `internal-ops/**`, `.github/**`; deleted tests.
- **installer:** `src/lib/secret-generator.ts`, `src/data/image-digests.json`,
  `src/lib/image-digests.ts`, `src/lib/*generator*.ts`, `public/service-registry.json`,
  `public/CNAME`, `scripts/**`, `.github/**`; deleted tests.
- **packages (both prefixes):** `jarvis_package.yaml` (any change: its `secrets`, `packages`,
  `apt_packages`, `post_install` and `version` are the risky fields and a path rule cannot see
  fields), `*_scripts/**`, `*_shared/**` (OAuth and token code lives there), `.github/**`; deleted
  tests.

Note the content-level risks a path rule misses (a `curl … | sh` pin edited inside an ordinary docs
page; a `JarvisSecret` use added in a command file): the review brief tells the review agent to
return `needs_review: true` for them, which holds the item (that flag exists).

### The runner's verify step (option 3)

After the build agent returns `ready_for_pr` and the runner has checked the commit, before pushing:

- For each `verify` entry of the repo: run it **outside the sandbox** as the runner, cwd the
  worktree (or a named other repo's checkout), with a timeout, env `LOOP_ITEM`, `LOOP_HEAD`,
  `LOOP_WORKTREE`, `LOOP_EVIDENCE_DIR`; collect files it writes to `LOOP_EVIDENCE_DIR` as evidence
  (images included, size limits as today) and its exit code and log tail.
- A failing verify → the build is not pushed; the run goes back to build (next run gets the verify
  log in `.loop/review/verify.md`), counting like a bounce.
- The PR body's Verification footer lists the runner's verify steps and their results separately
  from the agent's own claims.
- The review agent gets the verify outputs (screenshots as files it can view) and judges them.
- The verify commands come from the runner's config, never from the agent or the repo's PR (a PR
  editing the harness scripts it would run is already on jarvis-server's hold list via
  `deny_write`; add the same for `scripts/mobile-shot`, `scripts/sandbox`, `scripts/screenshots`).
- First verify entries:
  - jarvis-server: `scripts/sandbox up --name $LOOP_SANDBOX || exit 3` + `go run ./tools/pipeline run` +
    `scripts/screenshots` when `web/` changed + `down --purge` (`$LOOP_SANDBOX` is verify's own
    `loop-<item>-verify`, apart from the agent's `loop-<item>`; exit 3 gets one retry after a purge:
    `docs/decisions/2026-10-20-verify-sandbox.md`).
  - jarvis-node-mobile: from the runner's jarvis-server checkout, `scripts/sandbox up --name
    loop-$LOOP_ITEM`, `scripts/mobile-shot --sandbox loop-$LOOP_ITEM --ref $LOOP_HEAD` against the
    runner's node-mobile mirror (`JARVIS_MOBILE_REPO`), copy the PNGs and `index.json` to evidence,
    `down --purge`. Needs the mobile-shot changes below.
  - others: none yet (CI covers them).
- One verify at a time per machine (a runner-level lock): the GPU model, the emulator and the
  mobile build dir are single.

### jarvis-server's mobile-shot

- Accept a bare repo / worktree (`git -C "$MOBILE_REPO" rev-parse --git-dir` instead of the
  `.git` directory test) and never fetch into it unless asked.
- A lock on `~/.cache/jarvis-harness/mobile/build` (flock), so two runs cannot clobber each other.

## Repo-side changes (held, one batch)

- **jarvis-docs:** `.github/workflows/ci.yml` on `pull_request` and `push: main`: Python, `pip
  install -r requirements-dev.txt`, `pytest`. A `CLAUDE.md` (it has none): what the repo is, the
  verify command, the merging rule, the hold paths.
- **Packages:** a reusable workflow in `jarvis-developer-toolkit` (`.github/workflows/package-ci.yml`,
  `workflow_call`): Python 3.11, install the toolkit, `jdt test . --install-deps`, `pytest tests` if
  present. Each package gets a 6-line caller `.github/workflows/ci.yml` and a `CLAUDE.md` loop
  section (verify, merging rule, hold paths). One PR per repo, the same text.
- **Mobile, installer:** a CLAUDE.md "Merging (agents)" section like jarvis-server's (squash your
  own PR after CI; hold list = the paths above; merges ship to testers / deploy the site). Fix the
  stale lines the survey found (node-mobile "CI/CD: Not yet set up", installer's `install.sh`).

## Harness (scenarios first)

- Routing: an item with no repo among many → `routed` → the next triage has one checkout →
  reproduced → build.
- Repo aliases in `/loop add` (table test + one scenario).
- Verify step: passes → evidence attached with the runner's label, PR footer lists it; fails →
  back to build with the log, nothing pushed; never runs the agent's suggested commands.
- Hold rules with a repo wildcard (table).

## Rollout (main session, after the user's "merge it")

Rebuild; config for all repos (mirrors are created lazily on first claim); `loopd --repos-file`;
restart both services; re-file the user's mobile item (its text is saved from the stray loopd) and
watch it through to land.
