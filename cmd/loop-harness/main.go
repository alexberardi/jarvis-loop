// Command loop-harness lets a build or review agent use its repo's harness through the runner
// (docs/decisions/2026-10-16-harness-broker.md). The agent's own sandbox cannot reach loopback,
// the harness's credentials or /dev/kvm, and kills what a command leaves running; the runner runs
// these commands for it in its verify fence instead:
//
//	loop-harness up              start the repo's harness (scripts/sandbox up) and keep it running
//	loop-harness run '<command>' run a command in the worktree, next to the running harness
//	loop-harness down            stop the harness
//	loop-harness status          the session's state
//
// It finds the session in .loop/harness.json from the working directory up, prints the output
// as it comes, and exits with the command's exit code (125 when the runner refused).
//
// `loop-harness serve-inner` is the runner's: the command server at the end of the keeper fence.
package main

import (
	"os"

	"github.com/alexberardi/jarvis-loop/internal/harness"
)

func main() { os.Exit(harness.Main(os.Args[1:])) }
