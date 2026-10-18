//go:build unix

package harness

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// Main is the loop-harness command (cmd/loop-harness; the runner's tests run it too).
func Main(args []string) int {
	usage := func() int {
		fmt.Fprintln(os.Stderr, "usage: loop-harness up | run '<command>' | down | status\n"+
			"       loop-harness pi '<command>' | pi-put <file> <path on the Pi> | pi-get <path on the Pi> <file>")
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
	case OpPi:
		if len(args) < 2 {
			return usage()
		}
		req.Cmd = strings.Join(args[1:], " ")
	case OpPiPut, OpPiGet:
		if len(args) != 3 {
			return usage()
		}
		req.Src, req.Dst = args[1], args[2]
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
	// A signal (the agent's command timed out or was interrupted) withdraws the request, which
	// ends the command at the runner.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	cancel := make(chan struct{})
	go func() {
		<-sig
		close(cancel)
	}()
	code, err := Call(f, req, os.Stdout, cancel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loop-harness:", err)
	}
	return code
}
