# Runbook: move loopd to fly.io

Item #550. Why: [docs/decisions/2026-10-23-loopd-on-fly.md](../decisions/2026-10-23-loopd-on-fly.md).
Hand steps, in order, by the user. Nothing here was run by an agent: the sandbox that built this has
no flyctl, no fly credentials and no Docker, so **every command below is unverified** and the first
pass should be read, not pasted blindly.

The loop keeps working throughout except for step 5, which is the only window where loopd is down.
Budget 30 minutes, and do it when the queue is quiet (`loop status`).

## 0. Decide the two things this repo does not know

1. **The fly org.** `fly orgs list`. It is not a `fly.toml` field, so it is only ever an argument:
   `fly apps create jarvis-loopd -o <org>`.
2. **The region.** `fly.toml` says `primary_region = "ewr"` (Secaucus, NJ: closest to the house and
   to GitHub's us-east). Change it before the first deploy if you want another.

The app name in `fly.toml` is `jarvis-loopd`. If you take a different one, change it there too.

## 1. Create the app and the volume

```sh
fly apps create jarvis-loopd -o <org>
fly volumes create loop_data --region ewr --size 3 --app jarvis-loopd   # GB; see step 6
fly scale count 1 --app jarvis-loopd                                   # one machine, always
```

Size the volume for the database plus `evidence/` plus `regressions/` with room to grow: check what
the prod box has now with `du -sh ~/.local/share/jarvis-loop` (or wherever `--data` points) and take
at least three times it. A volume can be grown later (`fly volumes extend`), never shrunk.

**Exactly one machine**, for the rest of this app's life. loopd is a single SQLite writer and a
volume belongs to one machine. `fly.toml` already says `auto_stop_machines = "off"`,
`auto_start_machines = false`, `min_machines_running = 1` and `[deploy] strategy = "immediate"`;
do not add a second machine and do not change the strategy.

## 2. Fill in loopd's own flags

The `Dockerfile`'s last line is loopd's command line, and it carries only what this repo knows:
`--addr 0.0.0.0:7780 --lan --data /data`. Copy the rest from the prod unit:

```sh
systemctl --user cat loopd            # or: systemctl cat loopd
```

Take its `--repos` / `--repos-file` and `--slack-workspace` and add them to `CMD`. (`--repos-file`
needs the file in the image: commit it and add a `COPY` line, or flatten the list into `--repos`.)
Without them loopd still starts, but triage cannot give a repo-less item a repo and the board has no
links to item threads — and neither failure is loud, which is why this step is not optional.

`--tokens` and `--slack-env` are **not** yours to set: the entrypoint appends them, pointing at the
files it writes in step 3.

Commit this, through the loop like any other change.

## 3. Set the secrets

The entrypoint writes each of these to `/run/loop` at 0600, because loopd refuses a secret file any
wider than that, and then drops it from the environment.

```sh
fly secrets set --app jarvis-loopd --stage \
  LOOP_TOKENS="$(cat ~/.config/jarvis-loop/tokens)"
fly secrets set --app jarvis-loopd --stage \
  LOOP_SLACK_ENV="$(cat ~/.config/jarvis-harness/slack.env)"
fly secrets set --app jarvis-loopd --stage \
  LOOP_ORCHESTRATOR_SLACK_ENV="$(cat ~/.config/jarvis-harness/orchestrator-slack.env)"
```

- `--stage` holds them until the first deploy, so no machine restarts now.
- **The tokens file is tab-separated** (`token<TAB>kind<TAB>name`). `"$(cat …)"` keeps the tabs;
  retyping the value by hand usually does not. If they are lost, loopd says
  `tokens line 1: want token<TAB>kind<TAB>name` and refuses to start — that error means the tabs,
  not the tokens.
- `fly secrets list` shows names and digests, never values. Never put any of these in `[env]`.
- Skip the two Slack lines to bring loopd up without Slack (useful for a first smoke test); set them
  and redeploy when you want the threads.

## 4. Deploy and smoke-test, with prod still running

```sh
fly deploy --app jarvis-loopd --strategy immediate
```

The build fails on purpose if the image would carry no `vcs.revision` (a dirty checkout, or `.git`
missing from the context): deploy from a clean checkout of `main`.

While prod's loopd is still the live one, the fly loopd has an **empty** database. Check the shape,
not the data:

```sh
curl -fsS https://jarvis-loopd.fly.dev/healthz                      # -> ok
curl -isS https://jarvis-loopd.fly.dev/items | head -1              # -> 401, not 200
curl -fsS -H "Authorization: Bearer $LOOP_TOKEN" \
     https://jarvis-loopd.fly.dev/version                           # -> the commit you deployed
fly logs --app jarvis-loopd                                         # "loopd listening", "slack on"
```

The `revision` in `/version` must be the commit you deployed; an empty one means the stamp check was
bypassed and `qa_after_deploy` items will stall. If Slack is on, `fly logs` shows `slack on` and the
bot answers in `#loop` — that is also the proof the CA roots in the image are right (a bad cert path
shows up as an x509 error from the socket, and nothing else).

Then stop it again so there are not two loopds talking to Slack:

```sh
fly machine stop --app jarvis-loopd $(fly machines list --app jarvis-loopd -q)
```

## 5. The data move (loopd is down for this)

The move is **one file**: a `restore.tar.gz` on the volume, which `loop-entrypoint` unpacks on the
next start. There is no shell in the image to untar anything, and copying `loop.db`, `loop.db-wal`
and `loop.db-shm` up separately is three chances to get a half-consistent database, so the archive
is the only supported path. It is the same format `GET /backup` produces, and the round trip is a
test (`TestBackupArchiveRestoresOntoAVolume`).

`loop-entrypoint` **refuses** to unpack it if `/data/loop.db` already exists, so you cannot do this
twice or over live data by accident. After unpacking it renames the archive to
`restore.tar.gz.done-<timestamp>`, and modes are its own (directories 0700, files 0600) whatever the
tar headers say.

```sh
# 1. Stop the queue cleanly: let the runners drain first.
systemctl --user kill -s SIGUSR1 loop-runner          # drain: no new claims
loop status                                           # wait until nothing is running
systemctl --user stop loopd
loop status || true                                   # must now fail to connect

# 2. Note what to compare against afterwards. Write these two numbers down.
cd ~/.local/share/jarvis-loop
sqlite3 loop.db 'SELECT COALESCE(MAX(version),0) FROM schema_migrations;'
sqlite3 loop.db 'SELECT COUNT(*) FROM items;'

# 3. Build the archive. VACUUM INTO, not cp: it reads one transaction and writes a whole, compact
#    database, which is the only consistent read of a WAL database — and with loopd stopped there
#    is then nothing left in loop.db-wal to carry.
rm -rf /tmp/loop-move && mkdir -p /tmp/loop-move
sqlite3 loop.db "VACUUM INTO '/tmp/loop-move/loop.db'"
cp -a evidence regressions /tmp/loop-move/ 2>/dev/null || true
tar czf /tmp/restore.tar.gz -C /tmp/loop-move .
tar tzf /tmp/restore.tar.gz | head                     # loop.db, evidence/…, regressions/…
sqlite3 /tmp/loop-move/loop.db 'SELECT COUNT(*) FROM items;'   # same as step 2

# 4. Put it on the volume and let the entrypoint unpack it.
fly machine start --app jarvis-loopd $(fly machines list --app jarvis-loopd -q)
printf 'put /tmp/restore.tar.gz /data/restore.tar.gz\n' | fly ssh sftp shell --app jarvis-loopd
fly machine restart --app jarvis-loopd $(fly machines list --app jarvis-loopd -q)
fly logs --app jarvis-loopd        # "unpacking restore.tar.gz (N entries) into /data"
```

`tar czf … -C dir .` writes names like `./loop.db`; the entrypoint accepts those (they clean to
`loop.db`) as well as `loop.db`. It refuses an archive with an absolute path, a `..`, a symlink or
anything but files and directories, and refuses it **whole**, before unpacking any of it.

**`fly ssh sftp` is served by fly's own init, not by the image, so it should work on a `scratch`
image — but that was not verified when this was written.** If it does not:

- Deploy once from a `Dockerfile` whose final stage is `FROM alpine:3` instead of `scratch` (same
  two binaries, same `ENTRYPOINT`), which gives you `fly ssh console` and a shell; put the archive
  there, let it unpack, check step 6, then `fly deploy` the real `scratch` image again. The volume
  keeps the data across both deploys. One extra deploy, nothing else.
- Do **not** try to bake the data into the image: it would end up in the registry.

Delete `/tmp/restore.tar.gz` and `/tmp/loop-move` from the dev box when step 6 passes — until then
they are a second copy of the data, which is a feature.

## 6. Check it before trusting it

```sh
fly machine restart --app jarvis-loopd $(fly machines list --app jarvis-loopd -q)
curl -fsS https://jarvis-loopd.fly.dev/healthz
curl -fsS -H "Authorization: Bearer $LOOP_TOKEN" https://jarvis-loopd.fly.dev/items | head -40
```

- **Schema version and item count match step 5.2.** `loop status` against the new URL shows the same
  items in the same stages.
- **The board renders.** Open `https://jarvis-loopd.fly.dev/board?t=<board token>` on the phone: the
  same swim lanes, the same items, and the links to Slack threads work (that is `--slack-workspace`
  from step 2).
- **An evidence file opens** from a recent item's page. That is the proof `evidence/` came along.
- **`/version`** is the deployed commit.
- **Slack**: `/loop status` in `#loop` answers, and only once (prod's loopd must be stopped).
- **Disk**: `fly volumes list --app jarvis-loopd` — if the volume is over about 70% full, extend it
  now rather than at 100%, where SQLite starts failing writes.
- **The used archive**: once all of the above passes, delete `/data/restore.tar.gz.done-<timestamp>`
  (in `fly ssh sftp shell`, if its `rm` is there; otherwise leave it). It is a whole copy of the
  pre-move data taking volume space; the nightly backup already leaves it out, so keeping it costs
  disk and nothing else.

## 7. Point the clients at it

```sh
# Each runner's config (0600): "loopd_url": "https://jarvis-loopd.fly.dev"
$EDITOR ~/.config/jarvis-loop/runner.json
systemctl --user restart loop-runner
# The CLI, in the shell profile:
export LOOP_URL=https://jarvis-loopd.fly.dev
```

The Macs ([docs/runbooks/mac-runner.md](mac-runner.md)) get the same `loopd_url`; the LAN listener
and its ufw rule are no longer needed, and the token now travels over TLS instead of in clear.

Then watch one item go round: `loop add -q 'P5 jarvis-loop: a no-op to watch the pipeline'`, and
check it reaches done. A `qa_after_deploy` item needs **both** loopd and the runner to be on a build
containing its merge now, so restart the runner after a loop deploy as well.

## 8. The nightly backup

A volume snapshot is not a database backup (see the decision). Install the pull:

```sh
install -m 0755 scripts/loopd-backup ~/.local/bin/loopd-backup
umask 077; printf '%s\n' '<the user token>' > ~/.config/jarvis-loop/backup-token
```

`~/.config/systemd/user/loopd-backup.service`:

```ini
[Unit]
Description=Pull loopd's nightly backup off fly.io

[Service]
Type=oneshot
Environment=LOOP_URL=https://jarvis-loopd.fly.dev
Environment=LOOP_TOKEN_FILE=%h/.config/jarvis-loop/backup-token
Environment=LOOP_BACKUP_DIR=%h/backups/jarvis-loop
Environment=LOOP_BACKUP_KEEP=30
ExecStart=%h/.local/bin/loopd-backup
```

`~/.config/systemd/user/loopd-backup.timer`:

```ini
[Unit]
Description=loopd backup, nightly

[Timer]
OnCalendar=*-*-* 03:30:00
Persistent=true
RandomizedDelaySec=15m

[Install]
WantedBy=timers.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now loopd-backup.timer
systemctl --user start loopd-backup.service    # run it once now
ls -l ~/backups/jarvis-loop/
```

Then **restore from it once**, now, while prod's data is still on the box to compare against — a
backup nobody has restored is not a backup:

```sh
mkdir -p /tmp/restore && tar xzf ~/backups/jarvis-loop/loop-backup-*.tar.gz -C /tmp/restore
sqlite3 /tmp/restore/loop.db \
  'SELECT COALESCE(MAX(version),0) FROM schema_migrations; SELECT COUNT(*) FROM items;'
```

Also turn on fly's own volume snapshots (`fly volumes update loop_data --snapshot-retention 14`) as
a second line — they are not a database backup, but they are a cheap machine-level one.

## 9. Keep prod's loopd for a week

**Stopped, not deleted**, until `<date + 7 days>`:

```sh
systemctl --user stop loopd
systemctl --user disable loopd      # do NOT uninstall, do NOT delete ~/.local/share/jarvis-loop
```

Put the date in a calendar reminder. After the week, if nothing has needed it:
`systemctl --user disable --now loopd` and archive the data directory somewhere cold.

## Rollback

Any time in that week, if fly's loopd is wrong or down:

```sh
fly machine stop --app jarvis-loopd $(fly machines list --app jarvis-loopd -q)
systemctl --user start loopd
# Point the clients back: loopd_url in each runner.json, LOOP_URL in the profile.
```

The prod database is where step 5 left it, so anything that happened on fly after the move is lost.
If that matters, pull a backup first (step 8) and restore it over the prod data directory with loopd
stopped: the archive's `loop.db` is a whole database, so it replaces `loop.db`, and the `-wal` and
`-shm` files are **deleted**, not kept.

```sh
systemctl --user stop loopd
cd ~/.local/share/jarvis-loop
mv loop.db loop.db.before-restore && rm -f loop.db-wal loop.db-shm
tar xzf ~/backups/jarvis-loop/loop-backup-<stamp>.tar.gz -C .
systemctl --user start loopd && loop status
```

**Restoring a backup onto fly** is the same archive in the same place as the move: delete
`/data/loop.db*`, put the backup at `/data/restore.tar.gz`, and restart the machine — the entrypoint
unpacks it, and refuses to while a `loop.db` is still there.

## If something is wrong

| What you see | Where to look |
| --- | --- |
| `fly deploy` fails with "no vcs.revision stamp" | Deploying from a dirty checkout, or `.git` is missing from the build context. Commit, or deploy from a clean `main`. |
| The machine restarts in a loop | `fly logs`. `loop-entrypoint: LOOP_TOKENS is empty` means step 3; `tokens line 1: want token<TAB>kind<TAB>name` means the tabs were lost. |
| `/healthz` is 404 | An old image. Check `/version` against the commit you expect. |
| `/items` is 200 without a token | Stop the machine and say so — that is not this build. |
| Slack is quiet, no error | `fly logs` for `slack on`. An x509 error means the image's CA roots; a socket that never opens means the tokens. |
| Slack answers twice | Both loopds are running. Stop prod's. |
| `qa_after_deploy` items sit in qa | `/version` is empty, or the runner's build is behind loopd's. Both halves must contain the merge now; restart the runner. |
| `loop-entrypoint: … loop.db already exists` | The restore archive is still on the volume and the data is already there. Move it aside (it is already renamed `.done-…` if it was used), or delete `/data/loop.db*` if you really mean to replace them. |
| Writes fail, `database or disk is full` | `fly volumes list` and extend. |
