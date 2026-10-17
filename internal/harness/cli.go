//go:build unix

package harness

import (
	"fmt"
	"os"
	"strings"
)

// Main is the loop-harness command (cmd/loop-harness; the runner's tests run it too).
func Main(args []string) int {
	usage := func() int {
		fmt.Fprintln(os.Stderr, "usage: loop-harness up | run '<command>' | down | status")
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	req := Request{Op: args[0]}
	switch args[0] {
	case "serve-inner":
		if err := ServeInner(os.Stdin, os.NewFile(3, "frames")); err != nil {
			fmt.Fprintln(os.Stderr, "loop-harness serve-inner:", err)
			return 1
		}
		return 0
	case OpUp, OpDown, OpStatus:
		if len(args) != 1 {
			return usage()
		}
	case OpRun:
		if len(args) < 2 {
			return usage()
		}
		req.Cmd = strings.Join(args[1:], " ")
	default:
		return usage()
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "loop-harness:", err)
		return ExitBroker
	}
	f, err := Find(wd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loop-harness:", err)
		return ExitBroker
	}
	code, err := Call(f, req, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loop-harness:", err)
	}
	return code
}
