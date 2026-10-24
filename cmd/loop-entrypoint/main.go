// Command loop-entrypoint is the image's entrypoint (docs/decisions/2026-10-23-loopd-on-fly.md).
// loopd reads its secrets from files it insists are 0600 (api.LoadTokens, slack.LoadEnv), while
// fly.io delivers them as environment variables, and the image is FROM scratch, so there is no
// shell to bridge the two. This writes each secret that is set to a file under /run/loop at 0600,
// drops it from the environment, and execs loopd with the paths it wrote.
//
// It also unpacks a restore archive left on the data volume, which is how the data gets onto an
// empty volume at all: with no shell in the image there is nothing on the machine that could untar
// one (restore.go).
//
// It never prints a secret's value: an error names the variable or the file, never the contents.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// secretDir holds the files written from the environment. It is on the machine's own (ephemeral)
// filesystem, not the data volume: the secrets come back from fly on every start.
const secretDir = "/run/loop"

// loopdPath is the binary this execs. It is fixed: this entrypoint exists for one image.
const loopdPath = "/loopd"

// secret is one of loopd's secret files: the variable fly.io delivers it in, the file loopd reads,
// and the flag that points loopd at it ("" when loopd finds the file on its own).
type secret struct {
	env  string
	name string
	flag string
	// need says the process cannot start without it.
	need bool
}

var secrets = []secret{
	// The bearer tokens: without them loopd has no user and no runner, so there is nothing to run.
	{env: "LOOP_TOKENS", name: "tokens", flag: "--tokens", need: true},
	// Slack is optional; loopd runs without it (quietly, with no threads).
	{env: "LOOP_SLACK_ENV", name: "slack.env", flag: "--slack-env"},
	// The @orchestrator app's own tokens. loopd looks for this name next to --slack-env's file by
	// default, so it needs no flag.
	{env: "LOOP_ORCHESTRATOR_SLACK_ENV", name: "orchestrator-slack.env"},
}

func main() {
	if err := run(os.Args[1:], secretDir, syscall.Exec); err != nil {
		fmt.Fprintln(os.Stderr, "loop-entrypoint:", err)
		os.Exit(1)
	}
}

// run writes the secret files into dir, drops them from the environment, and execs loopd with
// args plus the flags for the files it wrote. Its own flags come last, so they win over a
// --tokens or --slack-env on the command line: the paths are this entrypoint's to decide.
func run(args []string, dir string, exec func(string, []string, []string) error) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// A directory that already exists keeps its old mode, which may be the image's 0755.
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	// Before loopd opens the database: an archive left on the volume is the data move, and the
	// only way onto an empty volume with no shell in the image (restore.go).
	if data := dataDir(args); data != "" {
		if err := restore(data); err != nil {
			return err
		}
	}
	argv := append([]string{loopdPath}, args...)
	for _, s := range secrets {
		v := os.Getenv(s.env)
		if strings.TrimSpace(v) == "" {
			if s.need {
				return fmt.Errorf("%s is empty: set it with `fly secrets set` (it holds loopd's tokens file)", s.env)
			}
			continue
		}
		path := filepath.Join(dir, s.name)
		if err := os.WriteFile(path, []byte(ensureNewline(v)), 0o600); err != nil {
			return fmt.Errorf("writing %s from %s: %w", path, s.env, err)
		}
		// WriteFile leaves an existing file's mode alone, and loopd refuses any group or other
		// bit (api.LoadTokens, slack.LoadEnv), so say it outright.
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
		// loopd and anything it starts have no business holding the secret twice.
		if err := os.Unsetenv(s.env); err != nil {
			return fmt.Errorf("dropping %s from the environment: %w", s.env, err)
		}
		if s.flag != "" {
			argv = append(argv, s.flag, path)
		}
	}
	// syscall.Exec only returns on failure.
	if err := exec(loopdPath, argv, os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", loopdPath, err)
	}
	return nil
}

// ensureNewline ends the file with one newline: both readers scan lines, and a value pasted into
// `fly secrets set` often loses its last one.
func ensureNewline(v string) string {
	if strings.HasSuffix(v, "\n") {
		return v
	}
	return v + "\n"
}
