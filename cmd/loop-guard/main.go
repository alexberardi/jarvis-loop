// Command loop-guard is the PreToolUse (and SessionStart) hook every loop agent runs under. The
// runner writes its config per run and names it in the generated Claude Code settings:
//
//	loop-guard                      (config from $LOOP_GUARD_CONFIG, set by the runner)
//	loop-guard --config FILE
//
// It reads the hook input on stdin. A refused tool call exits 2 with a deny decision on stdout
// and the reason on stderr, and is appended to the run's report file for the runner. Everything
// else exits 0 silently, leaving the decision to Claude Code's permission rules, classifier and
// sandbox. See internal/guard.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/guard"
)

func main() {
	path, err := guard.ConfigPath(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loop-guard:", err, "- refusing")
		os.Exit(2)
	}
	os.Exit(guard.Hook(path, os.Stdin, os.Stdout, os.Stderr, time.Now))
}
