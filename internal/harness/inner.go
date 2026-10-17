//go:build unix

package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// InnerRequest is one command for the keeper's server: bash -c Cmd in Dir with exactly Env (the
// fence's environment as the runner built it, not the keeper shell's after "up" sourced its
// files), or a Cancel of the one running.
type InnerRequest struct {
	Cmd       string   `json:"cmd,omitempty"`
	Env       []string `json:"env,omitempty"`
	Dir       string   `json:"dir,omitempty"`
	TimeoutMS int64    `json:"timeout_ms,omitempty"`
	Cancel    bool     `json:"cancel,omitempty"`
}

// KillGrace is how long a cancelled or timed-out command has between SIGTERM and SIGKILL.
var KillGrace = 5 * time.Second

// ServeInner is `loop-harness serve-inner`, the end of a keeper fence's script: it runs one
// command at a time from in, in its own process group inside the keeper's namespaces (so the
// sandbox's pidfiles mean something to it), and writes Frames to out. It returns at in's end.
func ServeInner(in io.Reader, out io.Writer) error {
	var mu sync.Mutex
	enc := json.NewEncoder(out)
	send := func(f Frame) {
		mu.Lock()
		defer mu.Unlock()
		enc.Encode(f)
	}
	reqs := make(chan InnerRequest)
	go func() {
		defer close(reqs)
		dec := json.NewDecoder(in)
		for {
			var r InnerRequest
			if dec.Decode(&r) != nil {
				return
			}
			reqs <- r
		}
	}()
	for r := range reqs {
		if r.Cancel {
			continue // nothing is running
		}
		code := runInner(r, send, reqs)
		send(Frame{Exit: &code})
	}
	return nil
}

// runInner runs one request; a cancel (or in's end) on reqs stops it.
func runInner(r InnerRequest, send func(Frame), reqs <-chan InnerRequest) int {
	cmd := exec.Command("bash", "-c", r.Cmd)
	cmd.Dir, cmd.Env = r.Dir, r.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw, err := os.Pipe()
	if err != nil {
		send(Frame{Data: []byte("loop-harness: " + err.Error() + "\n")})
		return ExitBroker
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	err = cmd.Start()
	pw.Close()
	if err != nil {
		pr.Close()
		send(Frame{Data: []byte("loop-harness: " + err.Error() + "\n")})
		return ExitBroker
	}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		buf := make([]byte, 32<<10)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				send(Frame{Data: append([]byte(nil), buf[:n]...)})
			}
			if err != nil {
				return
			}
		}
	}()
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	var limit <-chan time.Time
	if r.TimeoutMS > 0 {
		t := time.NewTimer(time.Duration(r.TimeoutMS) * time.Millisecond)
		defer t.Stop()
		limit = t.C
	}
	pid := cmd.Process.Pid
	stop := func() {
		syscall.Kill(-pid, syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(KillGrace):
		}
	}
	timedOut := false
	for done := false; !done; {
		select {
		case <-exited:
			done = true
		case <-limit:
			timedOut, done = true, true
			stop()
		case c, ok := <-reqs:
			if !ok || c.Cancel { // the broker cancelled, or went away
				done = true
				stop()
			}
		}
	}
	// What the command left in its group goes with it.
	syscall.Kill(-pid, syscall.SIGKILL)
	<-exited
	// A process that left the group (setsid) may hold the pipe: stop reading after a moment.
	select {
	case <-copied:
	case <-time.After(2 * time.Second):
		pr.Close()
		<-copied
	}
	pr.Close()
	if timedOut {
		send(Frame{Data: []byte(fmt.Sprintf("\nloop-harness: timed out after %s\n", time.Duration(r.TimeoutMS)*time.Millisecond))})
		return ExitTimeout
	}
	ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return cmd.ProcessState.ExitCode()
}
