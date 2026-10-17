package runner

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/harness"
)

// The harness broker (docs/decisions/2026-10-16-harness-broker.md). Build and review agents run
// in Claude Code's OS sandbox: no loopback, no harness credentials, no /dev/kvm, and nothing they
// start outlives the command. So for a repo with a "harness" the runner runs harness commands for
// them, in the same fence as its verify steps: the agent's loop-harness client sends a request
// over the runner's Unix socket with the run's token, and the runner streams the output back.
//
// "up" starts a keeper fence: the repo's up command, then loop-harness serve-inner, which runs
// every later command of the session inside the keeper's namespaces (the sandbox's pidfiles and
// /tmp mean the same thing there). Without a keeper a command gets a fence of its own. When the
// agent's run ends (or the lease is lost, or the runner stops) the keeper is killed, which takes
// everything it started with it, and "down" runs in a fresh fence to remove what is left on disk.

// HarnessSocket is the broker's socket: one per runner, the same path in every repo's settings.
func (c *Config) HarnessSocket() string { return filepath.Join(c.WorkRoot, "harness.sock") }

// harnessBinDir is the directory put first on a harness repo's agents' PATH: only loop-harness.
func (c *Config) harnessBinDir() string { return filepath.Join(c.WorkRoot, "bin") }

type harnessBroker struct {
	r    *Runner
	ln   net.Listener
	addr string // what harness.json names: the socket path (or tcp:<addr> in tests)
	mu   sync.Mutex
	sess []*harnessSession
	wg   sync.WaitGroup
}

type harnessSession struct {
	b      *harnessBroker
	token  string
	item   int64
	wt     string
	logDir string
	cfg    HarnessConfig
	env    []string

	op sync.Mutex // one request at a time (status excepted)

	mu      sync.Mutex
	keeper  *fenceProc
	fences  map[*fenceProc]bool // every live fence of the session, the keeper too
	upRC    int
	upAt    time.Time
	upCount int // up requests: a down at the end runs only when there was one
	n       int // log number
	ended   bool
}

// fenceProc is a fence ending in loop-harness serve-inner: requests go on stdin, frames come back.
type fenceProc struct {
	cmd    *exec.Cmd
	env    []string
	stdin  io.WriteCloser
	frames *json.Decoder
	exited chan struct{}
}

func (f *fenceProc) send(req harness.InnerRequest) error {
	b, _ := json.Marshal(req)
	_, err := f.stdin.Write(append(b, '\n'))
	return err
}

func (f *fenceProc) kill() {
	if f.alive() {
		_ = signalGroup(f.cmd.Process.Pid, syscall.SIGKILL)
	}
	<-f.exited
}

func (f *fenceProc) alive() bool {
	select {
	case <-f.exited:
		return false
	default:
		return true
	}
}

// broker starts the broker on first use.
func (r *Runner) broker() (*harnessBroker, error) {
	r.hbMu.Lock()
	defer r.hbMu.Unlock()
	if r.hb != nil {
		return r.hb, nil
	}
	if r.harnessTCP { // tests in a sandbox that refuses Unix sockets
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		b := &harnessBroker{r: r, ln: ln, addr: "tcp:" + ln.Addr().String()}
		b.wg.Add(1)
		go b.serve()
		r.hb = b
		return b, nil
	}
	sock := r.Cfg.HarnessSocket()
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return nil, err
	}
	os.Remove(sock) // a stale one from a runner that did not stop cleanly
	old := syscall.Umask(0o177)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	syscall.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("the harness socket: %w", err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	b := &harnessBroker{r: r, ln: ln, addr: sock}
	b.wg.Add(1)
	go b.serve()
	r.hb = b
	return b, nil
}

// CloseHarness ends every harness session (as at a run's end) and closes the socket: loop-runner
// calls it when it stops.
func (r *Runner) CloseHarness() {
	r.hbMu.Lock()
	b := r.hb
	r.hb = nil
	r.hbMu.Unlock()
	if b == nil {
		return
	}
	b.ln.Close()
	b.mu.Lock()
	sess := append([]*harnessSession(nil), b.sess...)
	b.mu.Unlock()
	for _, s := range sess {
		s.end(context.Background())
	}
	b.wg.Wait()
	os.Remove(r.Cfg.HarnessSocket())
}

func (b *harnessBroker) serve() {
	defer b.wg.Done()
	for {
		c, err := b.ln.Accept()
		if err != nil {
			return
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer c.Close()
			b.handle(c)
		}()
	}
}

// startHarness opens a session for a build or review run of a repo with a harness and writes
// .loop/harness.json; nil when the repo has none.
func (r *Runner) startHarness(ru *run) (*harnessSession, error) {
	if ru.repo.Harness == nil || (ru.stage != StageBuild && ru.stage != StageReview) {
		return nil, nil
	}
	if _, err := os.Stat(r.Cfg.LoopHarness); err != nil {
		return nil, fmt.Errorf("the loop-harness binary %s: %w", r.Cfg.LoopHarness, err)
	}
	if err := r.linkHarnessBin(); err != nil {
		return nil, err
	}
	b, err := r.broker()
	if err != nil {
		return nil, err
	}
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	wt := ru.wt
	if p, err := filepath.EvalSymlinks(wt); err == nil {
		wt = p
	}
	s := &harnessSession{b: b, token: hex.EncodeToString(tok), item: ru.item.ID, wt: wt,
		logDir: filepath.Join(ru.scratch, "harness"), cfg: *ru.repo.Harness, fences: map[*fenceProc]bool{},
		env: append(r.agentEnv(), "LOOP_ITEM="+strconv.FormatInt(ru.item.ID, 10), "LOOP_WORKTREE="+ru.wt,
			"LOOP_EVIDENCE_DIR="+filepath.Join(ru.wt, ".loop", "evidence"), "LOOP_SANDBOX="+ru.sandbox,
			"LOOP_REPO="+ru.repo.Slug, "LOOP_BASE="+ru.repo.Base, "LOOP_MIRROR="+ru.mirror)}
	if err := os.MkdirAll(s.logDir, 0o755); err != nil {
		return nil, err
	}
	f, _ := json.Marshal(harness.File{Socket: b.addr, Token: s.token})
	if err := os.WriteFile(filepath.Join(ru.wt, harness.FileRel), append(f, '\n'), 0o600); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.sess = append(b.sess, s)
	b.mu.Unlock()
	return s, nil
}

// linkHarnessBin makes <work_root>/bin/loop-harness, the agent's way to the client.
func (r *Runner) linkHarnessBin() error {
	dir := r.Cfg.harnessBinDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	link := filepath.Join(dir, "loop-harness")
	if cur, err := os.Readlink(link); err == nil && cur == r.Cfg.LoopHarness {
		return nil
	}
	tmp := link + ".tmp"
	os.Remove(tmp)
	if err := os.Symlink(r.Cfg.LoopHarness, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

// end ends the session: no more requests, every fence killed (the keeper takes what up started
// with it), then the repo's down in a fresh fence when up ever ran, and harness.json removed.
func (s *harnessSession) end(ctx context.Context) {
	b := s.b
	b.mu.Lock()
	for i, x := range b.sess {
		if x == s {
			b.sess = append(b.sess[:i], b.sess[i+1:]...)
			break
		}
	}
	b.mu.Unlock()
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	var live []*fenceProc
	for f := range s.fences {
		live = append(live, f)
	}
	s.mu.Unlock()
	for _, f := range live {
		f.kill()
	}
	s.mu.Lock()
	s.fences, s.keeper = map[*fenceProc]bool{}, nil
	s.mu.Unlock()
	s.op.Lock() // a request in flight ends with its fence
	defer s.op.Unlock()
	if s.upCount > 0 {
		code, err := s.oneShot(ctx, s.cfg.Down, io.Discard, "down (run end)", true)
		if err != nil || code != 0 {
			b.r.Log.Warn("harness down at run end", "item", s.item, "exit", code, "err", err)
		}
	}
	os.Remove(filepath.Join(s.wt, harness.FileRel))
}

func (b *harnessBroker) session(token string) *harnessSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	var found *harnessSession
	for _, s := range b.sess {
		if subtle.ConstantTimeCompare([]byte(s.token), []byte(token)) == 1 {
			found = s
		}
	}
	return found
}

func (b *harnessBroker) handle(c net.Conn) {
	enc := json.NewEncoder(c)
	var mu sync.Mutex
	send := func(f harness.Frame) {
		mu.Lock()
		defer mu.Unlock()
		enc.Encode(f)
	}
	refuse := func(msg string) { send(harness.Frame{Error: msg}) }
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(c, 1<<20)).ReadBytes('\n')
	if err != nil {
		refuse("no request")
		return
	}
	c.SetReadDeadline(time.Time{})
	var req harness.Request
	if err := json.Unmarshal(line, &req); err != nil {
		refuse("the request is not JSON")
		return
	}
	s := b.session(req.Token)
	if s == nil {
		refuse("no harness session has that token (the run may have ended)")
		return
	}
	// The token is in a file other worktrees' agents could read: the caller must also be this
	// user's process working inside the session's worktree.
	if !b.r.harnessTCP {
		uc, _ := c.(*net.UnixConn)
		if err := peerIn(uc, s.wt); err != nil {
			refuse(err.Error())
			return
		}
	}
	// The client going away (the agent's command ended or timed out) cancels what it asked for.
	gone := make(chan struct{})
	go func() {
		io.Copy(io.Discard, c)
		close(gone)
	}()
	out := frameWriter{send}
	var code int
	switch req.Op {
	case harness.OpStatus:
		code, err = s.status(out)
	case harness.OpUp:
		code, err = s.up(out, gone)
	case harness.OpRun:
		if strings.TrimSpace(req.Cmd) == "" {
			refuse("run needs a command")
			return
		}
		code, err = s.run(req.Cmd, out, gone)
	case harness.OpDown:
		code, err = s.down(out, gone)
	default:
		refuse(fmt.Sprintf("unknown op %q (up, run, down, status)", req.Op))
		return
	}
	if err != nil {
		refuse(err.Error())
		return
	}
	send(harness.Frame{Exit: &code})
}

type frameWriter struct{ send func(harness.Frame) }

func (w frameWriter) Write(p []byte) (int, error) {
	w.send(harness.Frame{Data: append([]byte(nil), p...)})
	return len(p), nil
}

var errSessionEnded = errors.New("the harness session ended (the run is over)")

// logFile opens the next .loop/scratch/harness/<n>.log.
func (s *harnessSession) logFile(what string) (*os.File, error) {
	s.mu.Lock()
	s.n++
	n := s.n
	s.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(s.logDir, strconv.Itoa(n)+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(f, "$ %s\n", what)
	return f, nil
}

// startFence starts a fence in the worktree running script (which ends in serve-inner); its
// stdout (before serve-inner, the up command's output) is returned to read.
func (s *harnessSession) startFence(script string, atEnd bool) (*fenceProc, io.ReadCloser, error) {
	s.mu.Lock()
	ended := s.ended
	s.mu.Unlock()
	if ended && !atEnd {
		return nil, nil, errSessionEnded
	}
	r := s.b.r
	script += "\nexec " + shellQuote(r.Cfg.LoopHarness) + " serve-inner\n"
	cmd, err := r.fence(s.wt, script, s.env, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("the fence: %w", err)
	}
	fr, fw, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	defer fw.Close()
	cmd.ExtraFiles = []*os.File{fw} // fd 3: the frames
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fr.Close()
		return nil, nil, err
	}
	or, ow, err := os.Pipe()
	if err != nil {
		fr.Close()
		return nil, nil, err
	}
	defer ow.Close()
	cmd.Stdout, cmd.Stderr = ow, ow
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		fr.Close()
		or.Close()
		return nil, nil, err
	}
	f := &fenceProc{cmd: cmd, env: cmd.Env, stdin: stdin, frames: json.NewDecoder(fr), exited: make(chan struct{})}
	go func() {
		cmd.Wait()
		fr.Close()
		close(f.exited)
	}()
	s.mu.Lock()
	if s.ended && !atEnd { // end ran while this one started
		s.mu.Unlock()
		f.kill()
		return nil, nil, errSessionEnded
	}
	s.fences[f] = true
	s.mu.Unlock()
	return f, or, nil
}

func (s *harnessSession) forget(f *fenceProc) {
	s.mu.Lock()
	delete(s.fences, f)
	if s.keeper == f {
		s.keeper = nil
	}
	s.mu.Unlock()
}

// exec runs cmd through f's serve-inner, writing its output to out; gone cancels it.
func (s *harnessSession) exec(f *fenceProc, cmd string, out io.Writer, gone <-chan struct{}) (int, error) {
	if err := f.send(harness.InnerRequest{Cmd: cmd, Env: f.env, Dir: s.wt, TimeoutMS: s.cfg.Timeout().Milliseconds()}); err != nil {
		return 0, errors.New("the harness fence is gone")
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-gone:
			f.send(harness.InnerRequest{Cancel: true})
		case <-stop:
		}
	}()
	for {
		var fr harness.Frame
		if err := f.frames.Decode(&fr); err != nil {
			return 0, errors.New("the harness fence ended before the command did")
		}
		if len(fr.Data) > 0 {
			out.Write(fr.Data)
		}
		if fr.Exit != nil {
			return *fr.Exit, nil
		}
	}
}

// oneShot runs cmd in a fence of its own.
func (s *harnessSession) oneShot(ctx context.Context, cmd string, client io.Writer, what string, atEnd bool) (int, error) {
	logf, err := s.logFile(what)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	f, stdout, err := s.startFence("", atEnd)
	if err != nil {
		return 0, err
	}
	defer s.forget(f)
	defer f.kill()
	go io.Copy(logf, stdout)
	gone := make(chan struct{})
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				close(gone)
			case <-f.exited:
			}
		}()
	}
	code, err := s.exec(f, cmd, io.MultiWriter(logf, client), gone)
	fmt.Fprintf(logf, "\n[exit %d]\n", code)
	f.stdin.Close()
	return code, err
}

func (s *harnessSession) run(cmd string, out io.Writer, gone <-chan struct{}) (int, error) {
	s.op.Lock()
	defer s.op.Unlock()
	s.mu.Lock()
	k := s.keeper
	s.mu.Unlock()
	if k == nil || !k.alive() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-gone:
				cancel()
			case <-ctx.Done():
			}
		}()
		return s.oneShot(ctx, cmd, out, cmd, false)
	}
	logf, err := s.logFile(cmd)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	code, err := s.exec(k, cmd, io.MultiWriter(logf, out), gone)
	fmt.Fprintf(logf, "\n[exit %d]\n", code)
	return code, err
}

// up starts the keeper: the repo's up command, then serve-inner. It answers with up's output and
// exit code; an up that failed keeps the keeper, so the agent can look around and run down.
func (s *harnessSession) up(out io.Writer, gone <-chan struct{}) (int, error) {
	s.op.Lock()
	defer s.op.Unlock()
	s.mu.Lock()
	k := s.keeper
	s.upCount++
	s.mu.Unlock()
	if k != nil && k.alive() {
		fmt.Fprintf(out, "loop-harness: already up (up exited %d at %s); `loop-harness down` first to start again\n", s.upRC, s.upAt.Format(time.Kitchen))
		return s.upRC, nil
	}
	logf, err := s.logFile("up: " + s.cfg.Up)
	if err != nil {
		return 0, err
	}
	// up's output, then the line serve-inner's frames are not on: up's own stdin and fd 3 are closed.
	script := "( " + s.cfg.Up + "\n) </dev/null 3>&- 2>&1\necho \"LOOP-HARNESS-READY rc=$?\""
	f, stdout, err := s.startFence(script, false)
	if err != nil {
		logf.Close()
		return 0, err
	}
	lines := make(chan string)
	go func() {
		defer close(lines)
		br := bufio.NewReader(stdout)
		for {
			l, err := br.ReadString('\n')
			if l != "" {
				lines <- l
			}
			if err != nil {
				return
			}
		}
	}()
	limit := time.NewTimer(s.cfg.Timeout())
	defer limit.Stop()
	fail := func(msg string, code int) (int, error) {
		fmt.Fprintf(io.MultiWriter(logf, out), "loop-harness: %s\n", msg)
		f.kill()
		s.forget(f)
		logf.Close()
		go func() {
			for range lines {
			}
		}()
		return code, nil
	}
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				return fail("the harness fence ended before up finished", harness.ExitBroker)
			}
			if rc, isReady := strings.CutPrefix(strings.TrimSpace(l), "LOOP-HARNESS-READY rc="); isReady {
				code, _ := strconv.Atoi(rc)
				fmt.Fprintf(logf, "[up exit %d; the keeper's later output follows]\n", code)
				go func() { // what up's processes still print
					for l := range lines {
						logf.WriteString(l)
					}
					logf.Close()
				}()
				s.mu.Lock()
				s.keeper, s.upRC, s.upAt = f, code, time.Now()
				s.mu.Unlock()
				return code, nil
			}
			io.WriteString(io.MultiWriter(logf, out), l)
		case <-limit.C:
			return fail("up timed out after "+s.cfg.Timeout().String(), harness.ExitTimeout)
		case <-gone:
			return fail("cancelled", harness.ExitBroker)
		}
	}
}

// down runs the repo's down in the keeper (where its pidfiles make sense), or in a fence of its
// own when nothing is up, then ends the keeper.
func (s *harnessSession) down(out io.Writer, gone <-chan struct{}) (int, error) {
	s.op.Lock()
	defer s.op.Unlock()
	s.mu.Lock()
	k := s.keeper
	s.mu.Unlock()
	if k == nil || !k.alive() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-gone:
				cancel()
			case <-ctx.Done():
			}
		}()
		return s.oneShot(ctx, s.cfg.Down, out, "down: "+s.cfg.Down, false)
	}
	logf, err := s.logFile("down: " + s.cfg.Down)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	code, err := s.exec(k, s.cfg.Down, io.MultiWriter(logf, out), gone)
	fmt.Fprintf(logf, "\n[exit %d]\n", code)
	k.kill()
	s.forget(k)
	return code, err
}

func (s *harnessSession) status(out io.Writer) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keeper == nil || !s.keeper.alive() {
		fmt.Fprintf(out, "harness: not up (worktree %s)\n", s.wt)
		return 1, nil
	}
	fmt.Fprintf(out, "harness: up since %s (up exited %d), worktree %s; logs in %s\n",
		s.upAt.Format(time.Kitchen), s.upRC, s.wt, s.logDir)
	return 0, nil
}
