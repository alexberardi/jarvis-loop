# Mac runner: loop-runner on the MacBook Pro (and the Mac mini)

Items that need real macOS (resources `macos`: the Mac server app, the desktop agent's `ioreg`
device id) are built and reviewed by a second runner on a Mac on the LAN. The dev box keeps
everything else. Why it works this way: [the decision](../decisions/2026-10-17-mac-runners.md).

- **MacBook Pro, 10.0.0.103: preferred.** Runner name `mbp`.
- **Mac mini, 10.0.0.210: fallback**, for when the MBP is asleep or away. Runner name `mini`.
  Both may run at once: each claims on its own, and an item goes to whichever asks first.

Each Mac has its own token, its own `work_root` and its own mirrors. Nothing is shared across
machines: no NFS or synced work root, no copied runner config.

## 1. On the dev box: open loopd to the LAN for runners

1. Start loopd with the remote runners' listener (next to its usual flags; the main API stays on
   127.0.0.1):

   ```sh
   loopd ... --runner-addr 0.0.0.0:7781
   ```

   That listener serves the API to **runner tokens only**: user, board and orchestrator tokens
   get 403 there, no token 401.
2. Let only the two Macs reach it:

   ```sh
   sudo ufw allow from 10.0.0.103 to any port 7781 proto tcp comment 'loop-runner mbp'
   sudo ufw allow from 10.0.0.210 to any port 7781 proto tcp comment 'loop-runner mini'
   ```

   It is plain HTTP: the runner's token travels in clear on the LAN. Fine on the home LAN; do not
   forward the port or open it wider than this.
3. Give each Mac its own token: one line per Mac in loopd's tokens file (loopd's `--tokens`,
   mode 0600, `token<TAB>kind<TAB>name`), then restart loopd:

   ```sh
   printf '%s\trunner\tmbp\n'  "$(openssl rand -hex 24)" >> "$TOKENS"
   printf '%s\trunner\tmini\n' "$(openssl rand -hex 24)" >> "$TOKENS"
   ```

   The token's name is the runner's name in leases and events (`runner:mbp`). Copy each token to
   its Mac by hand (not through Slack or a repo).
4. Build the binaries for the Mac (or build them on the Mac, step 2.4):

   ```sh
   CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o dist/darwin-arm64/ ./cmd/loop-runner ./cmd/loop-guard ./cmd/loop-harness
   ```

## 2. On the Mac

1. **Go** with mise: `brew install mise`, then `mise use -g go@1.27` (the version in `go.mod`).
2. **claude** CLI: install it, run `claude` once and log in (the login goes to the keychain).
3. **gh**: `brew install gh`, `gh auth login` (HTTPS, the user's account). The runner pushes and
   opens PRs with it, as on the dev box.
4. **The binaries**: copy `dist/darwin-arm64/*` to `~/.local/bin/` (or clone jarvis-loop and run
   the build above there). `loop-guard` must sit next to `loop-runner` (or set `guard`).
5. **`runner.json`** at loop-runner's default `--config` path (the same place as on the dev box),
   mode 0600. The MBP's is shown; the mini's is the same with `"name": "mini"` and its own token:

   ```json
   {
     "loopd_url": "http://10.0.0.<devbox>:7781",
     "token": "<the mbp token from step 1.3>",
     "name": "mbp",
     "capabilities": ["macos"],
     "only_resources": ["macos"],
     "stages": ["build", "review"],
     "slots": 1,
     "work_root": "~/.cache/jarvis-loop/work",
     "mode": "auto",
     "repos": { "...": "the repos with Mac work, as in the dev box's runner.json" }
   }
   ```

   - `only_resources: ["macos"]` is what keeps the Mac off ordinary items: without it a runner
     with `capabilities [macos]` and stages build/review would claim any build or review item
     that needs no resources.
   - No `land` stage, no orchestrator token: landing and the orchestrator stay on the dev box.
   - `work_root` is the Mac's own disk. Do not point it at a shared folder.
6. **The fence.** Setup, prepare, cleanup and verify commands run under `sandbox-exec` (built into
   macOS, `/usr/bin/sandbox-exec`) instead of bubblewrap: HOME read-only but for the worktree, the
   runner's fence caches and `verify_writable`; Homebrew's prefix
   (`/opt/homebrew`) read-only too; the credential paths unreadable; the keychain's
   service refused; a private `TMPDIR`; `/tmp` read-only (macOS has no private `/tmp`); network
   allowed. The runner probes it at start and refuses to run setup or verify without it. Tools
   that write elsewhere in HOME (Xcode's `~/Library/Developer/Xcode/DerivedData`,
   `~/Library/Caches/...`) need those paths in `verify_writable`.
7. **launchd**: `~/Library/LaunchAgents/net.alexberardi.loop-runner.plist`:

   ```xml
   <?xml version="1.0" encoding="UTF-8"?>
   <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
   <plist version="1.0">
   <dict>
     <key>Label</key><string>net.alexberardi.loop-runner</string>
     <key>ProgramArguments</key>
     <array>
       <string>/Users/alex/.local/bin/loop-runner</string>
     </array>
     <key>EnvironmentVariables</key>
     <dict>
       <key>PATH</key><string>/Users/alex/.local/share/mise/shims:/Users/alex/.local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
     </dict>
     <key>RunAtLoad</key><true/>
     <key>KeepAlive</key><true/>
     <key>StandardOutPath</key><string>/Users/alex/Library/Logs/loop-runner.log</string>
     <key>StandardErrorPath</key><string>/Users/alex/Library/Logs/loop-runner.log</string>
   </dict>
   </plist>
   ```

   A LaunchAgent (not a LaunchDaemon): it runs in the user's session, where the keychain holds
   claude's and gh's logins.

   ```sh
   launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/net.alexberardi.loop-runner.plist
   loop-runner reload                                                # after editing runner.json
   loop-runner drain                                                 # after an update: exits once its runs end
   launchctl bootout gui/$(id -u)/net.alexberardi.loop-runner        # stop
   ```

   `reload` (SIGHUP) applies the config to the next claim and keeps the live runs; the log says
   whether it took the file. `drain` (SIGUSR1) stops claiming and exits once the live runs end;
   `KeepAlive` then starts the new binary. `launchctl kickstart -k` restarts at once and ends the
   live runs: only for a stuck runner.

   On the MBP, sleep stops it; its lease expires and the item goes back to waiting (the mini, if
   running, takes it). "Prevent automatic sleeping when the display is off" on power keeps it up.

## 3. Check that it claims

- `tail -f ~/Library/Logs/loop-runner.log` on the Mac: the fence probe passes and the runner polls.
  A 401 or 403 is the token (wrong, or not a runner token); connection refused is loopd's
  `--runner-addr` or ufw.
- On the dev box, `loop status` or the board's "working now": a macos item (#61, #63) gets a lease
  held by `mbp` (or `mini`), and its "no runner offers macos" flag clears with a `servable` event.
- Ordinary items keep going to the dev box's runner.

## Limits

`active_agents` (default 4) and the per-stage and per-resource limits are **global**: loopd counts
every live lease, whichever runner holds it, so the Macs' agents come out of the same budget as the
dev box's. The `gpu` and `emulator` limits are not per runner either; the Macs offer neither, so
that does not matter here. To allow one Mac item at a time across both Macs, set a `macos` limit
of 1 in loopd's control (`PUT /control` with `{"limits": {"macos": 1}}`, a user token).

**The last slot is held for Mac work while a Mac item can run**
([decision](../decisions/2026-10-18-slot-hold.md)). loopd refuses a claim the last free agent slot
when (a) a runner whose `only_resources` has `macos` claimed or sent a heartbeat within two of its
poll intervals (`poll_seconds`, sent with each claim; 30 s when it did not say), (b) an item needing
`macos` is claimable now by that runner (in one of its own `stages`, waiting, its dependencies
done, not leased, the `macos` limit allows it: a `macos` item in triage or land holds nothing for a
Mac that takes build and review) and (c) the claim would not take a `macos` item. When any of them is false, the
last slot goes to whoever asks. Each such resource (`macos` now, `ios` later) holds at most one
slot. The board's **Runners** panel lists the runners seen today and says when a slot is held, for
what and for which item. So the dev box no longer needs a slot fewer than `active_agents` to leave
the Mac room: set its `slots` to `active_agents` (6 under `active_agents` 6). A Mac runner that is
busy with its own `slots` still counts as around (its heartbeats), so with no `macos` limit a second
Mac item can hold a slot that Mac cannot use yet; the `macos` limit of 1 above avoids that.
