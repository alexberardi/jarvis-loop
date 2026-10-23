# loopd on fly.io: one machine, one volume, one port (2026-10-23)

Item #550, rollout phase 9's first piece. `loopd` — the API, the SQLite database, the board and the
Slack socket — moves off the prod box to fly.io. The runbook for the move and the rollback is
[docs/runbooks/loopd-on-fly.md](../runbooks/loopd-on-fly.md).

**Why.** The settled design (DESIGN.md §Security, rollout 9) said "loopd listens on the LAN only; a
Cloudflare Tunnel can come later". That kept loopd's uptime tied to a desk machine's: the loop
stops when the box reboots, and it is reachable from a phone only through the tunnel that was never
built. fly.io gives the same thing with less: a managed TLS port, a machine that restarts itself,
and a volume for the database. This decision replaces the tunnel for loopd. The runners stay where
the work is — they need the GPU, the emulator and the Macs — so loopd and the runners are no longer
deployed together, which is the second half of this change.

## Decision

- **Exactly one machine, one volume.** loopd is a single SQLite writer, and a fly volume belongs to
  one machine, so a second machine would either have no data or be a second writer. `fly.toml`:
  `min_machines_running = 1`, `auto_stop_machines = "off"`, `auto_start_machines = false`,
  `[mounts] loop_data -> /data`, `primary_region = "ewr"`, and `[deploy] strategy = "immediate"`
  **in the file**, not only in the deploy command: a rolling deploy needs a second machine to shift
  traffic to and that machine cannot have the volume. `fly scale count 1`, always.
- **One port.** `internal_port = 7780`, `force_https = true`, and `loopd --addr 0.0.0.0:7780 --lan`
  (`checkAddr` refuses a non-loopback address without `--lan`). `--board-addr` and `--runner-addr`
  are **dropped**: on fly everything arrives on the one TLS port, and the per-route token rules —
  which were always the real check — are what separate the board, the runners and the user.
  `loopd.BoardHandler`, `loopd.RunnerHandler` and `api.NewRunners` stay, for the loop's harness and
  for anyone who wants a listener of its own.
- **`GET /healthz`, unauthenticated.** `ok` and a newline, `text/plain`, `no-store`, 405 with no
  body for any other method, and nothing else: no version, no schema version, no counts, nothing
  about the data or the host. It is registered in `loopd.Handler`, not in `api.New`'s routes, every
  one of which needs a token.
- **A `scratch` image.** CGO off (SQLite is `modernc.org/sqlite`), so the final stage is two static
  binaries plus the CA roots — nothing else in the image to keep patched. `America/New_York` works
  without zoneinfo because `internal/slack/bot.go` imports `time/tzdata`. The build **fails** when
  the binary carries no `vcs.revision` or was built from a dirty checkout: a stampless loopd would
  silently stall every `qa_after_deploy` item (below), so it must not ship.
- **Secrets as files, written by `cmd/loop-entrypoint`.** loopd refuses a secret file any wider than
  0600 (`api.LoadTokens`, `slack.LoadEnv`) and `scratch` has no shell, so a small Go entrypoint
  writes each `fly secrets` value under `/run/loop` at 0600, drops it from the environment it hands
  on, and execs loopd with the paths it wrote: `LOOP_TOKENS` (required), `LOOP_SLACK_ENV` and
  `LOOP_ORCHESTRATOR_SLACK_ENV`. It refuses to start without the tokens rather than let loopd fail
  obscurely, and no error it prints carries a value. The alternative — teaching loopd `--tokens-env`
  and friends — would widen `internal/api/auth.go` and `internal/slack/env.go`, both hold-rule
  files; the entrypoint keeps the change in `cmd/`.
- **The data move is one archive, unpacked by the entrypoint.** `FROM scratch` leaves nothing on
  the machine that could untar anything, and copying `loop.db`, `loop.db-wal` and `loop.db-shm` up
  one at a time is three chances at a half-consistent database. So a `restore.tar.gz` left on the
  volume is unpacked by `loop-entrypoint` before loopd opens the database, in the same format
  `GET /backup` produces — so the move, and a restore from a nightly backup, are one mechanism with
  one test (`TestBackupArchiveRestoresOntoAVolume`). It refuses to unpack over an existing
  `loop.db`, refuses an archive whole if any entry is absolute, climbs out of the directory, or is
  not a plain file or directory, writes 0600/0700 whatever the headers claim, and renames the
  archive aside rather than deleting the one copy of the data on the machine.
- **loopd reports its own build, and a deploy is both halves.** `qa_after_deploy` assumed loopd and
  the runner shipped together, so the runner's own build stood for the loop's
  ([2026-10-16](2026-10-16-qa-after-deploy.md)). That is now false, and the failure would be silent
  and the wrong way round: the dev box's runner contains merges loopd has not deployed, so items
  would reach done before the change was live. So loopd serves its own commit at `GET /version` (a
  read, behind a token), and the runner reports `deployed` only for items whose merge **both** its
  own build and loopd's contain. Either revision unknown reports nothing, said once, and the user
  falls back to `/loop verified`. The revision in the event and in the Slack thread is loopd's: the
  build the user is being asked to check.
- **A nightly backup, pulled.** `GET /backup` (the user's token only) streams a gzipped tar of a
  `VACUUM INTO` snapshot of loop.db plus `evidence/` and `regressions/`;
  [`scripts/loopd-backup`](../../scripts/loopd-backup) pulls it from a systemd timer on the dev box,
  checks the archive before keeping it, and prunes. A volume snapshot is **not** a database backup:
  it copies three WAL files that are only consistent together between writes, which is also why
  copying `loop.db*` out from under a live writer is not it either. `VACUUM INTO` reads one
  transaction and writes a whole, compact database that opens on its own.

## The security change, plainly

Two things get wider, and both are worth saying out loud rather than burying.

- **The status page is reachable from the internet, behind basic auth over TLS and nothing else.**
  [2026-10-14 web page auth](2026-10-14-web-page-auth.md) ends "when the Cloudflare Tunnel arrives
  (rollout 9), Cloudflare Access sits in front of this anyway". On fly it does not. So the basic-auth
  credentials on `GET /` and `POST /ui/control` are the whole guard for the page, where before an
  attacker also had to be on the home LAN. The page's own protections are unchanged (constant-time
  comparison, `CrossOriginProtection` on the one state-changing route, `no-store`, `DENY`, a CSP
  with no scripts), and every bearer token is now only ever sent over TLS, which is better than the
  LAN's plain HTTP for the Mac runners. If this is not enough, the next step is fly's own
  `[http_service]` restrictions or an authenticating proxy in front — not a change in loopd.
- **`GET /backup` serves the entire database and every uploaded file to one bearer token.** That
  token (the user's) could already read every item, event and evidence file one request at a time,
  so this is a change in convenience, not in reach — but it turns "read everything" into a single
  request, which is worth a deliberate decision rather than a quiet one. It is kept off the `reads`
  kinds, so a runner's token and the orchestrator's are refused. The pull shape was chosen over
  loopd pushing to object storage precisely so that the fly machine holds **no** credential that
  can write anywhere outside itself.

**Considered and not chosen.** Pushing the backup to S3/Tigris/B2 on a ticker: a new dependency, two
more secrets, and a machine that can then write somewhere else. Two machines for a rolling deploy:
impossible with one volume and one SQLite writer. A non-root user in the image: fly machines are
single-tenant VMs and the volume and `/run/loop` are root's, so it would buy nothing here.

## Consequences

- `loopd_url` (a runner's config) and `LOOP_URL` (the `loop` CLI) become the `https://` address.
  No client code changes.
- `--repos`/`--repos-file` and `--slack-workspace` are the prod unit's own values, which this repo
  does not hold, so the Dockerfile's `CMD` does not carry them: step 2 of the runbook has the user
  copy them in. The fly org and the app name are theirs too; `ewr` is assumed.
- `cmd/loopd/main_test.go` lost `TestRunWithBoardAddr` and `TestRunWithRunnerAddr` with the flags
  they covered, replaced by `TestRunServesOneListener`.
- The living design doc linked at the top of DESIGN.md says LAN-only and a tunnel. **Only the user
  can edit that**; this repo's DESIGN.md is updated here.
- Prod's loopd stays installed but stopped for a week, so the rollback is: stop the fly machine,
  start the unit, point `loopd_url` and `LOOP_URL` back.
