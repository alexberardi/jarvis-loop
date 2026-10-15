// Command fakeclaude stands in for `claude -p` in the runner's dry run: it plays a scripted
// scenario (FAKE_CLAUDE_SCENARIO, default "good") and runs the generated settings' hooks the way
// Claude Code would. It never calls a model, GitHub or loopd. See internal/fakeclaude.
package main

import (
	"os"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
)

func main() {
	os.Exit(fakeclaude.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
