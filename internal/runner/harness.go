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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/guard"
	"github.com/alexberardi/jarvis-loop/internal/harness"
)

// The harness broker (docs/decisions/2026-10-16-harness-broker.md). Build and review agents run
// in Claude Code's OS sandbox: no loopback, no Unix sockets, no harness credentials, no /dev/kvm,
// and nothing they start outlives the command. So for a repo with a "harness" the runner runs
// harness commands for them, in the same fence as its verify steps: the agent's loop-harness
// client drops a request with the run's token in the session's spool (.loop/harness/ in the
// worktree, which only that run's agent can write), and the runner writes the output back there.
// Every run command passes the run's own guard first, as the agent's Bash commands do.
//
// "up" starts a keeper fence: the repo's up command, then loop-harness serve-inner, which runs
// every later command of the session inside the keeper's namespaces (the sandbox's pidfiles and
// /tmp mean the same thing there). Without a keeper a command gets a fence of its own. When the
// agent's run ends (or the lease is lost, or the runner stops) the keeper is killed, which takes
// everything it started with it, and "down" runs in a fresh fence to remove what is left on disk.

// harnessBinDir is the directory put first on a harness repo's agents' PATH: only loop-harness.
func (c *Config) harnessBinDir() string { return filepath.Join(c.WorkRoot, "bin") }

type harnessBroker struct {
	r    *Runner
	mu   sync.Mutex
	sess []*harnessSession
}

type harnessSession struct {
	b      *harnessBroker
	token  string
	item   int64
	wt     string
	logDir string
	cfg    HarnessConfig
	env    []string
	guard  *guard.Config // the run's guard (its hook's config): every run command passes it first
	// spool and logs are opened before the agent starts, so a symlink it puts in .loop later
	// cannot send the runner's writes out of the worktree.
	spool *os.Root
	logs  *os.Root
	quit  chan struct{} // closed by end: no more requests
	reqs  sync.WaitGroup

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

func (r *Runner) broker() *harnessBroker {
	r.hbMu.Lock()
	defer r.hbMu.Unlock()
	if r.hb == nil {
		r.hb = &harnessBroker{r: r}
	}
	return r.hb
}

// CloseHarness ends every harness session (as at a run's end): loop-runner calls it when it stops.
func (r *Runner) CloseHarness() {
	r.hbMu.Lock()
	b := r.hb
	r.hbMu.Unlock()
	if b == nil {
		return
	}
	b.mu.Lock()
	sess := append([]*harnessSession(nil), b.sess...)
	b.mu.Unlock()
	for _, s := range sess {
		s.end(context.Background())
	}
}

// startHarness opens a session for a build or review run of a repo with a harness: a fresh
// spool, and .loop/harness.json; nil when the repo has none.
func (r *Runner) startHarness(ru *run) (*harnessSession, error) {
	if !ru.hasHarness() {
		return nil, nil
	}
	if _, err := os.Stat(r.cfgOf(ru).LoopHarness); err != nil {
		return nil, fmt.Errorf("the loop-harness binary %s: %w", r.cfgOf(ru).LoopHarness, err)
	}
	if err := r.linkHarnessBin(); err != nil {
		return nil, err
	}
	gc, err := guard.LoadConfig(filepath.Join(ru.dir, "guard.json"))
	if err != nil {
		return nil, fmt.Errorf("the run's guard config: %w", err)
	}
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	wt := ru.wt
	if p, err := filepath.EvalSymlinks(wt); err == nil {
		wt = p
	}
	s := &harnessSession{b: r.broker(), token: hex.EncodeToString(tok), item: ru.item.ID, wt: wt,
		logDir: filepath.Join(ru.scratch, "harness"), cfg: *ru.repo.Harness, fences: map[*fenceProc]bool{},
		guard: gc, quit: make(chan struct{}),
		env: append(r.agentEnv(), "LOOP_ITEM="+strconv.FormatInt(ru.item.ID, 10), "LOOP_WORKTREE="+ru.wt,
			"LOOP_EVIDENCE_DIR="+filepath.Join(ru.wt, ".loop", "evidence"), "LOOP_SANDBOX="+ru.sandbox,
			"LOOP_REPO="+ru.repo.Slug, "LOOP_BASE="+ru.repo.Base, "LOOP_MIRROR="+ru.mirror)}
	spool := filepath.Join(wt, harness.SpoolRel)
	if err := os.RemoveAll(spool); err != nil {
		return nil, err
	}
	for _, d := range []string{filepath.Join(spool, "req"), filepath.Join(spool, "out"), s.logDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	if s.spool, err = os.OpenRoot(spool); err != nil {
		return nil, err
	}
	if s.logs, err = os.OpenRoot(s.logDir); err != nil {
		s.spool.Close()
		return nil, err
	}
	f, _ := json.Marshal(harness.File{Spool: spool, Token: s.token})
	if err := os.WriteFile(filepath.Join(ru.wt, harness.FileRel), append(f, '\n'), 0o600); err != nil {
		s.spool.Close()
		s.logs.Close()
		return nil, err
	}
	b := s.b
	b.mu.Lock()
	b.sess = append(b.sess, s)
	b.mu.Unlock()
	s.reqs.Add(1)
	go s.poll()
	return s, nil
}

// linkHarnessBin makes <work_root>/bin/loop-harness, the agent's way to the client.
func (r *Runner) linkHarnessBin() error {
	dir := r.Config().harnessBinDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	link := filepath.Join(dir, "loop-harness")
	if cur, err := os.Readlink(link); err == nil && cur == r.Config().LoopHarness {
		return nil
	}
	tmp := link + ".tmp"
	os.Remove(tmp)
	if err := os.Symlink(r.Config().LoopHarness, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

// end ends the session: no more requests, every fence killed (the keeper takes what up started
// with it), then the repo's down in a fresh fence when up ever ran, and the spool and
// harness.json removed.
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
	close(s.quit)
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
	s.reqs.Wait() // the requests in flight end with their fences
	if s.upCount > 0 {
		code, err := s.oneShot(ctx, s.cfg.Down, io.Discard, "down (run end)", true)
		if err != nil || code != 0 {
			b.r.Log.Warn("harness down at run end", "item", s.item, "exit", code, "err", err)
		}
	}
	s.spool.Close()
	s.logs.Close()
	os.Remove(filepath.Join(s.wt, harness.FileRel))
	os.RemoveAll(filepath.Join(s.wt, harness.SpoolRel))
}

// poll takes the requests the client drops in req/ until the session ends.
func (s *harnessSession) poll() {
	defer s.reqs.Done()
	t := time.NewTicker(harness.Poll)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
		}
		d, err := s.spool.Open("req")
		if err != nil {
			continue
		}
		names, _ := d.Readdirnames(-1)
		d.Close()
		for _, name := range names {
			id, ok := strings.CutSuffix(name, ".json")
			if !ok || !harness.IDRe.MatchString(id) {
				continue
			}
			req, err := s.take(id)
			s.mu.Lock()
			ended := s.ended
			if !ended {
				s.reqs.Add(1)
			}
			s.mu.Unlock()
			if ended {
				return
			}
			go func() {
				defer s.reqs.Done()
				s.answer(id, req, err)
			}()
		}
	}
}

// take reads and removes req/<id>.json and creates out/<id>.log, which tells the client the
// request is in.
func (s *harnessSession) take(id string) (harness.Request, error) {
	var req harness.Request
	p := "req/" + id + ".json"
	fi, err := s.spool.Lstat(p)
	if err != nil {
		return req, err
	}
	defer s.spool.Remove(p)
	if !fi.Mode().IsRegular() || fi.Size() > 1<<20 {
		return req, errors.New("the request is not a small plain file")
	}
	b, err := s.spool.ReadFile(p)
	if err != nil {
		return req, err
	}
	if err := json.Unmarshal(b, &req); err != nil {
		return req, errors.New("the request is not JSON")
	}
	return req, nil
}

// answer runs one request and writes its output to out/<id>.log and its end to out/<id>.exit.
func (s *harnessSession) answer(id string, req harness.Request, err error) {
	out, oerr := s.spool.OpenFile("out/"+id+".log", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if oerr != nil {
		return // not ours, or the client's id is reused: no answer
	}
	defer out.Close()
	var end harness.End
	if err == nil {
		end.Exit, err = s.serve(id, req, out)
	}
	if err != nil {
		end = harness.End{Exit: harness.ExitBroker, Error: err.Error()}
	}
	b, _ := json.Marshal(end)
	tmp := "out/." + id + ".exit"
	if s.spool.WriteFile(tmp, b, 0o600) == nil {
		s.spool.Rename(tmp, "out/"+id+".exit")
	}
}

func (s *harnessSession) serve(id string, req harness.Request, out io.Writer) (int, error) {
	if subtle.ConstantTimeCompare([]byte(s.token), []byte(req.Token)) != 1 {
		return 0, errors.New("the request does not carry this run's token")
	}
	// The client withdrawing (it was killed, or the agent's command timed out) cancels the request.
	gone := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(harness.Poll * 4)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-s.quit:
			case <-t.C:
				if fi, err := s.spool.Stat("req/" + id + ".alive"); err == nil && time.Since(fi.ModTime()) < harness.Stale {
					continue
				}
			}
			close(gone)
			return
		}
	}()
	switch req.Op {
	case harness.OpStatus:
		return s.status(out)
	case harness.OpUp:
		return s.up(out, gone)
	case harness.OpRun:
		if strings.TrimSpace(req.Cmd) == "" {
			return 0, errors.New("run needs a command")
		}
		// The agent's guard, here too: a client of the agent's own can skip its hook.
		if d := s.guard.Check(guard.Input{Event: "PreToolUse", ToolName: "Bash", ToolInput: map[string]any{"command": req.Cmd}, Cwd: s.wt}); d.Deny {
			s.guard.Record("loop-harness run", d, time.Now())
			return 0, errors.New(d.Reason)
		}
		return s.run(req.Cmd, out, gone)
	case harness.OpDown:
		return s.down(out, gone)
	}
	return 0, fmt.Errorf("unknown op %q (up, run, down, status)", req.Op)
}

var errSessionEnded = errors.New("the harness session ended (the run is over)")

// logFile opens the next .loop/scratch/harness/<n>.log.
func (s *harnessSession) logFile(what string) (*os.File, error) {
	s.mu.Lock()
	s.n++
	n := s.n
	s.mu.Unlock()
	f, err := s.logs.OpenFile(strconv.Itoa(n)+".log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
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
	script += "\nexec " + shellQuote(r.Config().LoopHarness) + " serve-inner\n"
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
