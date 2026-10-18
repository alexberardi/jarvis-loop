package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/harness"
)

// The test Pi (docs/decisions/2026-10-18-pi-device.md). An item that needs the "pi" resource gets,
// through the harness broker, a shell on one configured Pi: `loop-harness pi '<command>'`,
// `pi-put` and `pi-get`. The runner, outside the agent's sandbox, runs ssh itself with the host,
// user, key and known_hosts from its own config; the agent names none of them, and its own ssh
// stays denied. What runs on the Pi is not limited: the device exists to be tested on. The Pi is
// one item's at a time (a lock in the broker, besides loopd's "pi" limit), it is put back in its
// normal state (Restore) before an item first uses it and when the item's run ends, and each item
// works in its own scratch dir there.

// ResourcePi is the resource and capability of an item that needs the test Pi.
const ResourcePi = "pi"

// PiConfig is the runner's test Pi.
type PiConfig struct {
	Host string `json:"host"`
	User string `json:"user"`
	Port int    `json:"port,omitempty"`
	// Key is the loop's own private key for the Pi (never the user's ~/.ssh key), and KnownHosts
	// the file that pins the Pi's host key; both stay secret to agents and verify steps.
	Key        string `json:"key"`
	KnownHosts string `json:"known_hosts"`
	// Restore puts the Pi back as the user keeps it (e.g. "sudo systemctl start jarvis-node"):
	// run before an item's first command and after its run ends.
	Restore string `json:"restore"`
	// TimeoutText is a Go duration (default 20m): the limit of one command.
	TimeoutText string `json:"timeout,omitempty"`
	// SSH is the ssh binary (default "ssh"; the tests use a fake).
	SSH string `json:"ssh,omitempty"`

	timeout time.Duration
}

var (
	piHostRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
	piUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
)

// PiScratch is the dir (under the remote user's home) items work in, one subdir per item.
const PiScratch = "loop-scratch"

func (p *PiConfig) normalize(caps []string) error {
	if !slices.Contains(caps, ResourcePi) {
		return errors.New(`config: "pi" is set but "pi" is not one of the capabilities`)
	}
	if !piHostRe.MatchString(p.Host) {
		return fmt.Errorf("config: pi.host %q is not a host name", p.Host)
	}
	if !piUserRe.MatchString(p.User) {
		return fmt.Errorf("config: pi.user %q is not a user name", p.User)
	}
	if p.Port == 0 {
		p.Port = 22
	}
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("config: pi.port %d", p.Port)
	}
	if strings.TrimSpace(p.Restore) == "" {
		return errors.New("config: pi.restore is required (what puts the Pi back, e.g. starting its node service)")
	}
	p.Key, p.KnownHosts = Expand(p.Key), Expand(p.KnownHosts)
	st, err := os.Stat(p.Key)
	if err != nil {
		return fmt.Errorf("config: pi.key: %w", err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("config: pi.key %s must be a plain file of mode 0600", p.Key)
	}
	if st, err := os.Stat(p.KnownHosts); err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("config: pi.known_hosts %s must be a file pinning the Pi's host key (%v)", p.KnownHosts, err)
	}
	p.timeout = DefaultVerifyTimeout
	if p.TimeoutText != "" {
		d, err := time.ParseDuration(p.TimeoutText)
		if err != nil || d <= 0 {
			return fmt.Errorf("config: pi.timeout must be a positive duration, not %q", p.TimeoutText)
		}
		p.timeout = d
	}
	if p.SSH == "" {
		p.SSH = "ssh"
	}
	return nil
}

// Timeout is the limit of one command on the Pi.
func (p *PiConfig) Timeout() time.Duration {
	if p.timeout > 0 {
		return p.timeout
	}
	return DefaultVerifyTimeout
}

// sshArgs is ssh's command line for script on the Pi: no config file, only the loop's key, a
// pinned host key, no forwarding of any kind and no prompts.
func (p *PiConfig) sshArgs(script string) []string {
	return []string{"-F", "/dev/null", "-i", p.Key,
		"-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + p.KnownHosts, "-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes", "-o", "PermitLocalCommand=no",
		"-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4",
		"-p", strconv.Itoa(p.Port), "-l", p.User, "--", p.Host, script}
}

// hasPi: the run's agent may use the test Pi (a build or review of an item that needs it, on a
// runner that has one).
func (r *Runner) hasPi(ru *run) bool {
	return r.Cfg.Pi != nil && (ru.stage == StageBuild || ru.stage == StageReview) &&
		slices.Contains(ru.item.Resources, ResourcePi)
}

// itemScratch is the item's dir on the Pi, relative to the remote home.
func (s *harnessSession) itemScratch() string { return PiScratch + "/" + strconv.FormatInt(s.item, 10) }

// remotePath checks a path the agent gave for the Pi: relative, inside the item's scratch dir.
func remotePath(p string) (string, error) {
	if p == "" || !filepath.IsLocal(p) || strings.ContainsAny(p, "\x00\n") {
		return "", fmt.Errorf("%q: a path on the Pi is relative to the item's scratch dir there, with no ..", p)
	}
	return path.Clean(filepath.ToSlash(p)), nil
}

// acquirePi takes the Pi for this session: refused while another item holds it. The first time,
// the Pi is put back in its normal state and the item's scratch dir made fresh.
func (s *harnessSession) acquirePi(out io.Writer, gone <-chan struct{}) error {
	b := s.b
	b.mu.Lock()
	switch {
	case b.piHolder == 0:
		b.piHolder = s.item
	case b.piHolder != s.item:
		holder := b.piHolder
		b.mu.Unlock()
		return fmt.Errorf("the Pi is in use by item #%d; try again later", holder)
	}
	b.mu.Unlock()
	s.mu.Lock()
	first := !s.piUsed
	s.piUsed = true
	s.mu.Unlock()
	if !first {
		return nil
	}
	fmt.Fprintln(out, "[loop-harness] preparing the Pi: its normal state, and a fresh scratch dir")
	code, err := s.pi.exec(context.Background(), s.prepareScript(), nil, out, out, gone)
	if err != nil || code != 0 {
		s.mu.Lock()
		s.piUsed = false // the next command prepares it again
		s.mu.Unlock()
		return fmt.Errorf("preparing the Pi failed (exit %d): %v", code, err)
	}
	return nil
}

func (s *harnessSession) prepareScript() string {
	return "rm -rf " + shellQuote(PiScratch) + " && mkdir -p " + shellQuote(s.itemScratch()) + "\n" + s.pi.Restore
}

// releasePi puts the Pi back after the item's run (when the item used it) and frees it for others.
func (s *harnessSession) releasePi() {
	defer func() {
		s.b.mu.Lock()
		if s.b.piHolder == s.item {
			s.b.piHolder = 0
		}
		s.b.mu.Unlock()
	}()
	s.mu.Lock()
	used := s.piUsed
	s.mu.Unlock()
	if !used {
		return
	}
	script := "rm -rf " + shellQuote(s.itemScratch()) + "\n" + s.pi.Restore
	ctx, cancel := context.WithTimeout(context.Background(), s.pi.Timeout())
	defer cancel()
	code, err := s.pi.exec(ctx, script, nil, io.Discard, io.Discard, nil)
	if err != nil || code != 0 {
		s.b.r.Log.Warn("putting the Pi back at run end", "item", s.item, "exit", code, "err", err)
	}
}

// exec runs script on the Pi with stdin, streaming its output; the exit code is the remote
// command's (255 when ssh itself failed), ExitTimeout when the time limit ended it.
func (p *PiConfig) exec(ctx context.Context, script string, stdin io.Reader, stdout, stderr io.Writer, gone <-chan struct{}) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, p.SSH, p.sshArgs(script)...)
	// The runner's environment without credentials: no agent socket, no askpass.
	cmd.Env = fencedEnv(os.Environ(), os.DevNull)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-gone:
			cancel()
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	if ctx.Err() == context.DeadlineExceeded {
		return harness.ExitTimeout, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}

// piRun runs the agent's command in the item's scratch dir on the Pi.
func (s *harnessSession) piRun(cmd string, out io.Writer, gone <-chan struct{}) (int, error) {
	if strings.TrimSpace(cmd) == "" {
		return 0, errors.New("pi needs a command")
	}
	if err := s.acquirePi(out, gone); err != nil {
		return 0, err
	}
	logf, err := s.logFile("pi: " + cmd)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	script := "mkdir -p " + shellQuote(s.itemScratch()) + " && cd " + shellQuote(s.itemScratch()) + " || exit 125\n" + cmd
	w := io.MultiWriter(logf, out)
	code, err := s.pi.exec(context.Background(), script, nil, w, w, gone)
	fmt.Fprintf(logf, "\n[exit %d]\n", code)
	return code, err
}

// piPut copies a file from the worktree to the item's scratch dir on the Pi.
func (s *harnessSession) piPut(src, dst string, out io.Writer, gone <-chan struct{}) (int, error) {
	rdst, err := remotePath(dst)
	if err != nil {
		return 0, err
	}
	root, err := os.OpenRoot(s.wt)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	f, err := root.Open(src) // within the worktree: no .., no symlink out of it
	if err != nil {
		return 0, fmt.Errorf("pi-put %s: %w", src, err)
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return 0, fmt.Errorf("pi-put %s: not a plain file", src)
	}
	if err := s.acquirePi(out, gone); err != nil {
		return 0, err
	}
	full := s.itemScratch() + "/" + rdst
	script := "mkdir -p " + shellQuote(path.Dir(full)) + " && cat > " + shellQuote(full)
	return s.pi.exec(context.Background(), script, f, out, out, gone)
}

// piGet copies a file from the item's scratch dir on the Pi into the worktree.
func (s *harnessSession) piGet(src, dst string, out io.Writer, gone <-chan struct{}) (int, error) {
	rsrc, err := remotePath(src)
	if err != nil {
		return 0, err
	}
	if !filepath.IsLocal(dst) {
		return 0, fmt.Errorf("pi-get: %q must be a path inside the worktree", dst)
	}
	if err := s.acquirePi(out, gone); err != nil {
		return 0, err
	}
	root, err := os.OpenRoot(s.wt)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	if d := filepath.Dir(dst); d != "." {
		if err := root.MkdirAll(d, 0o755); err != nil {
			return 0, err
		}
	}
	tmp := dst + ".pi-get.tmp"
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	code, err := s.pi.exec(context.Background(), "cat -- "+shellQuote(s.itemScratch()+"/"+rsrc), nil, f, out, gone)
	f.Close()
	if err != nil || code != 0 {
		root.Remove(tmp)
		return code, err
	}
	if err := root.Rename(tmp, dst); err != nil {
		root.Remove(tmp)
		return 0, err
	}
	fmt.Fprintf(out, "[loop-harness] %s → %s\n", src, dst)
	return 0, nil
}
