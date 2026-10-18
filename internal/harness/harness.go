// Package harness is the wire between loop-harness (the agent's client, inside Claude Code's OS
// sandbox), the runner's harness broker (internal/runner/harness.go), and the command server that
// runs inside a session's keeper fence (ServeInner). See
// docs/decisions/2026-10-16-harness-broker.md.
//
// Agent to broker, through a spool in the worktree (Claude Code's Linux sandbox refuses every
// Unix socket and loopback, but the agent may write its worktree): the client writes
// req/<id>.json (a Request, renamed into place) and keeps req/<id>.alive fresh while it waits;
// the broker removes the request, appends the output to out/<id>.log, and writes out/<id>.exit
// (an End, renamed into place) last.
//
// Broker to keeper: InnerRequests on the keeper's stdin; Frames back on its fd 3.
package harness

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// File is .loop/harness.json in the worktree: the session's spool and the run's token.
type File struct {
	Spool string `json:"spool"`
	Token string `json:"token"`
}

// FileRel is File's path relative to the worktree (.loop is never committed).
const FileRel = ".loop/harness.json"

// SpoolRel is the spool's path relative to the worktree; it has req/ and out/.
const SpoolRel = ".loop/harness"

// The operations a Request may ask for.
const (
	OpUp     = "up"     // start the keeper fence and run the repo's harness "up" in it
	OpRun    = "run"    // run Cmd in the worktree (in the keeper when it is up, else a new fence)
	OpDown   = "down"   // run the repo's harness "down" and end the keeper
	OpStatus = "status" // the session's state
	// The test Pi, for an item that needs the "pi" resource (the runner's ssh, never the agent's).
	OpPi    = "pi"     // run Cmd in the item's scratch dir on the Pi
	OpPiPut = "pi-put" // copy Src (in the worktree) to Dst (in the item's scratch dir on the Pi)
	OpPiGet = "pi-get" // copy Src (on the Pi) to Dst (in the worktree)
)

// Request is what loop-harness asks for.
type Request struct {
	Token string `json:"token"`
	Op    string `json:"op"`
	Cmd   string `json:"cmd,omitempty"`
	Src   string `json:"src,omitempty"`
	Dst   string `json:"dst,omitempty"`
}

// End is out/<id>.exit: the command's exit code, or why the broker refused or failed it.
type End struct {
	Exit  int    `json:"exit"`
	Error string `json:"error,omitempty"`
}

// Frame is one message from a keeper's serve-inner: output, or the end (Exit).
type Frame struct {
	Data  []byte `json:"data,omitempty"`
	Exit  *int   `json:"exit,omitempty"`
	Error string `json:"error,omitempty"`
}

// ExitBroker is loop-harness's exit code when the broker refused or failed the request (not the
// command's own code).
const ExitBroker = 125

// ExitTimeout is a command's exit code when its time limit ended it (as timeout(1)).
const ExitTimeout = 124

// The client touches req/<id>.alive every Heartbeat; the broker cancels a request whose .alive
// is gone or older than Stale (the client was killed, or the agent's command timed out).
var (
	Heartbeat = time.Second
	Stale     = 10 * time.Second
	// Poll is how often the broker looks for requests and the client for output.
	Poll = 50 * time.Millisecond
)

// IDRe is what a request id looks like (the broker ignores other names in req/).
var IDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Find looks for .loop/harness.json from dir up.
func Find(dir string) (File, error) {
	for d := dir; ; d = filepath.Dir(d) {
		b, err := os.ReadFile(filepath.Join(d, FileRel))
		if err == nil {
			var f File
			if err := json.Unmarshal(b, &f); err != nil || f.Spool == "" || f.Token == "" {
				return File{}, fmt.Errorf("%s: not a harness session file", filepath.Join(d, FileRel))
			}
			return f, nil
		}
		if filepath.Dir(d) == d {
			return File{}, errors.New("no " + FileRel + " here or above: the runner gives one only to build and review runs of a repo with a harness")
		}
	}
}

// Call sends req (with f's token) and copies the output to out as it comes. It returns the
// command's exit code, or an error when the broker refused or could not run it. Closing cancel
// (a signal) withdraws the request: the broker ends the command.
func Call(f File, req Request, out io.Writer, cancel <-chan struct{}) (int, error) {
	idb := make([]byte, 16)
	rand.Read(idb)
	id := hex.EncodeToString(idb)
	reqDir, outDir := filepath.Join(f.Spool, "req"), filepath.Join(f.Spool, "out")
	alive := filepath.Join(reqDir, id+".alive")
	if err := os.WriteFile(alive, nil, 0o600); err != nil {
		return ExitBroker, fmt.Errorf("the harness spool (is the run over?): %w", err)
	}
	defer os.Remove(alive)
	req.Token = f.Token
	b, _ := json.Marshal(req)
	tmp := filepath.Join(reqDir, "."+id+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return ExitBroker, err
	}
	if err := os.Rename(tmp, filepath.Join(reqDir, id+".json")); err != nil {
		os.Remove(tmp)
		return ExitBroker, err
	}
	logPath, endPath := filepath.Join(outDir, id+".log"), filepath.Join(outDir, id+".exit")
	defer os.Remove(logPath)
	defer os.Remove(endPath)
	var log *os.File // there once the broker has taken the request
	tick := time.NewTicker(Poll)
	defer tick.Stop()
	start, beat := time.Now(), time.Now()
	for {
		if log == nil {
			if log, _ = os.Open(logPath); log != nil {
				defer log.Close()
			}
		}
		// The end is written after the last output: read it first, then drain the log.
		eb, eerr := os.ReadFile(endPath)
		if log != nil {
			io.Copy(out, log)
		}
		if eerr == nil {
			var e End
			if err := json.Unmarshal(eb, &e); err != nil {
				return ExitBroker, fmt.Errorf("the runner's answer: %w", err)
			}
			if e.Error != "" {
				return ExitBroker, errors.New(e.Error)
			}
			return e.Exit, nil
		}
		if log == nil && time.Since(start) > Stale && os.Remove(filepath.Join(reqDir, id+".json")) == nil {
			return ExitBroker, errors.New("the runner did not take the request (is the run over?)")
		}
		select {
		case <-cancel:
			return ExitBroker, errors.New("cancelled")
		case <-tick.C:
		}
		if time.Since(beat) >= Heartbeat {
			beat = time.Now()
			os.Chtimes(alive, beat, beat)
		}
	}
}
