# jarvis-node-setup and jarvis-command-sdk get hold rules (2026-10-23)

Item #549, found while speccing phase 9. Every rule in `pipeline.DefaultHoldRules()` names a repo
and none is unscoped, so a repo no rule names faces **no hold rules whatever**: review and CI, then
merge. Two configured repos were in that state — `alexberardi/jarvis-node-setup` (0 of 117 rules)
and `alexberardi/jarvis-command-sdk` (0; its slug is not under `alexberardi/jarvis-cmd-*`, whose
prefix is `alexberardi/jarvis-cmd-`). Both were already in the runner's `repos` map and both had
already been worked: items #363 (PR #159) and #404 (PR #160) merged into jarvis-node-setup, and
#404 was a token-refresh change — exactly the auth class the hold list exists for.

Phase 6b gave every other repo its lists (`docs/phases/06b-all-repos.md`); these two were missed.

## Decision

`nodeSetupRules` and `commandSDKRules`, both exact slugs — never a `jarvis-node-*` wildcard, which
would also swallow jarvis-node-mobile, nor a wider `jarvis-cmd*`, which would reach repos nobody
surveyed.

**jarvis-node-setup is a "ships every merge" repo like the mobile apps,** not a quiet library:
`.github/workflows/release.yml` runs on `push: branches: [main]` with `contents: write,
packages: write` and pushes `ghcr.io/alexberardi/jarvis-node:latest` and `-audio` (amd64 + arm64) on
every push ("fast enough to run on every push"); a tag publishes the tarball, `install.sh` and a
minisign-signed `checksums.txt`. So its release and image config is held, and so are the paths that
run as root on a user's Pi.

| Rule | What |
|---|---|
| `release-config` | `.github/workflows/release.yml`, `Dockerfile`, `Dockerfile.*`, `.dockerignore`, `**/docker-compose*.y*ml`, `compose/**`, `pyproject.toml`, `**/*requirements*.txt`, `**/requirements*/**/*`, `build/**` |
| `ci` | `.github/**` |
| `auth` | the node's own authentication, secrets and K1/K2 keys: `core/ijarvis_authentication.py`, `core/ijarvis_secret.py`, `services/command_auth_service.py`, `services/secret_service.py`, `repositories/secrets_repository.py`, `models/secret.py`, `models/command_auth.py`, `agents/token_refresh_agent.py`, `utils/set_secret.py`, `utils/encryption_utils.py`, `utils/authorize_node.py`, `utils/generate_dev_k2.py` |
| `pairing` | `provisioning/**`, `services/bluetooth_pair_agent.py`, `services/camera_credentials_handler.py`, `utils/mqtt_credentials.py`, `scripts/run_provisioning.py`, `scripts/setup_mode.py` |
| `install-scripts` | `install.sh`, `setup.sh`, `setup_*.sh`, `setup/**` (the systemd units and the overlay), `scripts/jarvis-*` (the `jarvis-deploy.sudoers` drop-in, `jarvis-self-update`, the post-install and apt helpers), `services/post_install_dropins.py`, `services/update_service.py`, `services/package_install_handler.py`, `scripts/install_command.py`, `scripts/verify_package.py` |
| `deleted-test` | `tests/**`, `**/test_*.py` (`pyTestRules`), plus `**/test_*.sh` on D/R: node-setup's one shell test checks the release signature verification |

The release globs are `**`-prefixed because `*` does not cross `/`: the repo writes
`docker-compose.*.yaml` and `requirements-*.txt` at the root today, and a compose file or
requirements list added in a subdirectory, or with the other YAML extension, should not slip the
release hold (#549 review). On the real tree that already catches one more file,
`tools/wake_model_training/requirements-remote.txt` (79 of 556 blobs held on `M`, up from 78).
`**/requirements*/**/*` is the one glob with no file behind it yet: it is the `requirements/`
directory form the review asked for, and `**/*requirements*.txt` cannot see a `base.txt` inside
it.

**jarvis-command-sdk** is the SDK every command package and the Python node import, and
node-setup's CI installs it straight from git main (`pip install
git+https://github.com/alexberardi/jarvis-command-sdk.git`), so a merge to its main is what
consumers get — there is no tag to gate it. Held: `auth` (`jarvis_command_sdk/auth.py`,
`authentication.py`, `secret.py`, `cc_http.py`, `process.py`), `release-config` (`pyproject.toml`),
`ci` (`.github/**`) and deleted tests. It has no `jarvis_package.yaml` or `*_scripts/`, so
`packageRules` would not have fitted it even if the wildcard had reached it.

**The guard that would have caught this.** `TestEveryNamedRepoHasHoldRules` reads holdrules.go's
repo const block from the source (`go/parser`, not a hand-kept list) and fails when a const has no
rule that `ruleApplies` to it, with a representative slug for the two wildcards. A repo added to
that block without rules now fails CI.

**Old built-in sets stay recognised.** `defaultHoldRules` takes a third flag beside `trusted`, and
`EarlierDefaultHoldRules` gains `defaultHoldRules(loopRules(), true, false)` — the list loopd
shipped between #204 and this. Without it a database holding a frozen copy of that list would stop
being recognised and would never go back to the live defaults (`store.resetBuiltInCopy`), so the new
rules would not take effect there. Same move as #204's `loopRulesV1`. For the same reason the shared
builders (`pyTestRules`, `packageRules`) are not edited: `EarlierDefaultHoldRules` calls them, so a
change there would rewrite the lists earlier loopds shipped.

## Trade-offs

- **Alembic migrations are unheld,** including `alembic/versions/*_create_secrets_table.py` and
  `*_create_command_auth_table.py`. Same call as the loop's own (2026-10-18): a path rule cannot
  tell a migration that touches secrets from one that adds a table, node-setup adds migrations
  often, and review judges them.
- **A modified test is not held, only a deleted or renamed one** — the rule across every repo. The
  review agent's `needs_review` is what catches a test gutted in place.
- **Content-level risks stay with review:** a `JarvisSecret` use added to an ordinary command file,
  a pinned digest edited inside a docs page, `scripts/simplisafe_authorize.py` and the other
  third-party authorize helpers, `alembic_upgrade.sh`. The globs name the files whose *purpose* is
  auth, pairing, release or install.
- **The runner config is the user's.** `~/.config/jarvis-loop/runner.json` is outside every agent's
  sandbox, so the `aliases`, `setup` and `verify` entries #549 also asks for cannot be written by
  the loop. They are specified in `docs/phases/06b-all-repos.md` (with node-setup's and the SDK's
  own CI commands) for the user to paste. Until `verify` is set, those two repos land on review and
  CI alone — now with hold rules on top.
- **No new sandbox scenario.** The loop's harness has three stand-in repos (`sandbox/demo`,
  `sandbox/jarvis-loop`, `sandbox/jarvis-node-mobile`) and `SandboxHoldRules` swaps only those
  three slugs, so exercising a rule keyed on jarvis-node-setup's slug would mean adding a fourth
  sandbox repo. The hold path itself is already covered (scenarios 17, 23); what is new here is
  which paths match, which the table tests prove.

## Also in this PR: the race step gets 20 minutes per test binary

CI (`ci/check`) failed on the first commit of this branch with `panic: test timed out after 12m0s`
in `internal/runner` and **no failing test** — the binary was 88% of the way through its 286 tests
(by the elapsed times it reports here) when the alarm fired. `go test ./...` runs the packages
`GOMAXPROCS` at a time, so on a 4-core GitHub runner the slowest binary shares a core with three
other heavy ones (`internal/sandbox` 689 s, `internal/store` 585 s, `internal/slack` 432 s in that
run), and 12m stopped being enough as the suite grew. This change is nothing the hold rules did:
`internal/runner` takes 230.1 s with `holdrules.go` reset to `origin/main` and 228.1 s at this
head, the same 286 tests either way.

`-timeout 20m` keeps the point of the flag — a hung test prints its goroutines before the job's
25-minute cap cuts the job off — and costs no wall time, since the step already runs ~14 minutes of
work. If the suite keeps growing, the next step is splitting the slow packages into their own job
so each binary gets the whole runner, not a longer budget.
