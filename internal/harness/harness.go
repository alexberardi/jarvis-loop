// Package harness is the wire between loop-harness (the agent's client, inside Claude Code's OS
// sandbox), the runner's harness broker (internal/runner/harness.go), and the command server that
// runs inside a session's keeper fence (ServeInner). See
// docs/decisions/2026-10-16-harness-broker.md.
//
// Agent to broker, over the runner's Unix socket: one JSON line, a Request; then Frames back, one
// JSON line each, the output as it comes and a last one with the exit code or an error.
//
// Broker to keeper: InnerRequests on the keeper's stdin; Frames back on its fd 3.
package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// File is .loop/harness.json in the worktree: where the broker listens and the run's token.
type File struct {
	Socket string `json:"socket"`
	Token  string `json:"token"`
}

// FileRel is File's path relative to the worktree (.loop is never committed).
const FileRel = ".loop/harness.json"

// The operations a Request may ask for.
const (
	OpUp     = "up"     // start the keeper fence and run the repo's harness "up" in it
	OpRun    = "run"    // run Cmd in the worktree (in the keeper when it is up, else a new fence)
	OpDown   = "down"   // run the repo's harness "down" and end the keeper
	OpStatus = "status" // the session's state
)

// Request is what loop-harness sends.
type Request struct {
	Token string `json:"token"`
	Op    string `json:"op"`
	Cmd   string `json:"cmd,omitempty"`
}

// Frame is one line of a reply: output, or the end (Exit or Error).
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

// Find looks for .loop/harness.json from dir up.
func Find(dir string) (File, error) {
	for d := dir; ; d = filepath.Dir(d) {
		b, err := os.ReadFile(filepath.Join(d, FileRel))
		if err == nil {
			var f File
			if err := json.Unmarshal(b, &f); err != nil || f.Socket == "" || f.Token == "" {
				return File{}, fmt.Errorf("%s: not a harness session file", filepath.Join(d, FileRel))
			}
			return f, nil
		}
		if filepath.Dir(d) == d {
			return File{}, errors.New("no " + FileRel + " here or above: the runner gives one only to build and review runs of a repo with a harness")
		}
	}
}

// Call sends req (with f's token) and copies the output to out. It returns the command's exit
// code, or an error when the broker refused or could not run it.
func Call(f File, req Request, out io.Writer) (int, error) {
	network, addr := "unix", f.Socket
	if a, ok := strings.CutPrefix(f.Socket, "tcp:"); ok { // the runner's tests
		network, addr = "tcp", a
	}
	conn, err := net.Dial(network, addr)
	if err != nil {
		return ExitBroker, fmt.Errorf("the runner's harness socket: %w", err)
	}
	defer conn.Close()
	req.Token = f.Token
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return ExitBroker, err
	}
	dec := json.NewDecoder(conn)
	for {
		var fr Frame
		if err := dec.Decode(&fr); err != nil {
			return ExitBroker, fmt.Errorf("the runner ended the reply early: %w", err)
		}
		if len(fr.Data) > 0 {
			out.Write(fr.Data)
		}
		switch {
		case fr.Error != "":
			return ExitBroker, errors.New(fr.Error)
		case fr.Exit != nil:
			return *fr.Exit, nil
		}
	}
}
