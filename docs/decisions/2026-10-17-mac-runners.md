# Mac runners: loop-runner on the MacBook Pro and the Mac mini (2026-10-17)

Item #94. Items that need real macOS (#61, #63; resources `macos`) waited forever: no runner
offered `macos` (#92 made it visible). The user OK'd two Macs on the LAN, the MacBook Pro
(10.0.0.103, preferred) and the Mac mini (10.0.0.210, fallback), as runners with capabilities
`[macos]` for build and review. Setup: [the runbook](../runbooks/mac-runner.md).

## Decision

- **A listener for remote runners** (`loopd --runner-addr 0.0.0.0:7781`, `loopd.RunnerHandler`).
  It serves the API, but only to runner tokens: the bearer token is looked up before routing, and
  any other kind (user, board, orchestrator) gets 403, no token 401. The main `--addr` stays on
  127.0.0.1, so the user's and the orchestrator's tokens never work from the LAN. Like
  `--board-addr` it needs no `--lan`. It is plain HTTP on the home LAN (the token is in clear);
  ufw allows port 7781 from the two Macs only.
- **Claim scope: `only_resources`.** The claim only checked `resources ⊆ capabilities`, so a Mac
  with `[macos]` and stages build/review would also have taken every ordinary item (no resources).
  A runner's `only_resources` (each one of its capabilities) goes in the claim, and loopd then
  hands it only items needing at least one of them. The Macs set `["macos"]`; the dev box keeps
  everything else.
- **The macOS fence is sandbox-exec**, chosen by GOOS (`fence_bwrap.go` for `!darwin`,
  `fence_darwin.go`), with the same probe-or-refuse rule and the same masks. The profile
  (`sbplProfile`, built without a build tag so Linux CI tests it too): allow default (the host's
  filesystem and the network, as under bubblewrap); refuse writes under HOME, the work root,
  `/tmp` and Homebrew's prefixes (`/opt/homebrew`, `/usr/local`, when they exist); allow them again
  under the worktree, its git admin dir, the fence caches, `verify_writable` and a private TMPDIR
  (`<work_root>/fence-tmp/t*`, removed when the command ends by a shell outside the fence; what a
  kill leaves is cleared at the runner's start); refuse them again on the worktree's git plumbing and the read-only paths; the masks
  (`DefaultVerifyMask`, `verify_mask`, the runner's config) can be neither read nor written. Paths
  are resolved (sandbox-exec matches real paths: `/tmp` is `/private/tmp`) and escaped as SBPL
  strings, so a path cannot add rules.
  - Homebrew's prefix belongs to the user who installed it (on Linux `/usr/local` is root's), and
    the runner's launchd PATH puts `/opt/homebrew/bin` before `/usr/bin`: a fenced command that
    could write there could replace `gh` or `git` for the runner's next unfenced call, which holds
    the keychain login. It is the hole `~/.local/bin` and mise are read-only for on Linux
    (2026-10-15-all-repos.md). `TestFenceHomeIsReadOnly` tries `/opt/homebrew/bin` on macos-14.
  - macOS keeps gh's and claude's logins in the keychain, which the file masks do not cover
    (`security find-generic-password` asks securityd). The profile refuses the
    `com.apple.SecurityServer` service, and `Library/Keychains` is masked.
  - No mount namespace: `/tmp` cannot be private, so it is read-only and TMPDIR is the private
    temp dir. `Visible` needs nothing. No `--unshare-pid`: kill-on-cancel is the process group
    (Setpgid), as before.
  - `sandbox_exec` in runner.json overrides `/usr/bin/sandbox-exec`, as `bwrap` does on Linux.
  - CI: a `macos-14` job runs the fence tests (`TestFenceHidesSecrets`, `TestFenceHomeIsReadOnly`,
    `TestFenceFailsClosed`) and builds loop-runner.
- **Nothing shared across machines.** Each runner has its own token (its name in leases), its own
  `work_root` and mirrors. Review independence holds across runners: the agent id is
  `<runner name>-<random>`, so a build on `mbp` is never reviewed by the same agent.
- **Limits stay global.** `active_agents` and the stage and resource limits count every live lease
  whichever runner holds it, so the Macs share the dev box's budget. `gpu` and `emulator` are not
  per runner either; the Macs offer neither. A `macos` limit of 1 keeps one Mac item at a time. Since
  2026-10-18 the last free slot is held for a Mac item that can run
  ([2026-10-18-slot-hold](2026-10-18-slot-hold.md)).

## Not done

- No TLS on the runner listener (home LAN, two allowed hosts).
- The fence was only exercised on Linux here; its behaviour on macOS rests on the macos-14 CI job
  and the first run on the MBP. In particular, refusing `com.apple.SecurityServer` might break
  something a verify step needs (TLS trust is trustd, which stays allowed).
