package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// stoppedBuild runs a hanging build of a new item and stops it at a heartbeat; the item is back
// in the build queue, unstopped, and its next build plays good.
func (g *rig) stoppedBuild(title string) int64 {
	g.t.Helper()
	id := g.toBuild(title)
	wait := g.runAsync()
	g.clk.BlockUntil(2)
	waitFile(g.t, filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-build", id), "wip.txt"))
	stop := true
	g.st.SetControl(g.ctx, "user:alex", store.ControlPatch{Stopped: &stop})
	g.clk.Advance(60 * time.Second)
	wait()
	stop = false
	g.st.SetControl(g.ctx, "user:alex", store.ControlPatch{Stopped: &stop})
	g.script(id, "build", fakeclaude.Good, "")
	g.clk.Advance(time.Second) // the next run's own dir
	return id
}

func (g *rig) buildSession(id int64) *buildSession {
	g.t.Helper()
	b, err := os.ReadFile(filepath.Join(g.r.Cfg.WorkRoot, "sessions", fmt.Sprintf("%d-build.json", id)))
	if err != nil {
		return nil
	}
	var s buildSession
	if err := json.Unmarshal(b, &s); err != nil {
		g.t.Fatal(err)
	}
	return &s
}

func (g *rig) setBuildSession(id int64, s *buildSession) {
	g.t.Helper()
	b, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(g.r.Cfg.WorkRoot, "sessions", fmt.Sprintf("%d-build.json", id)), b, 0o600); err != nil {
		g.t.Fatal(err)
	}
}

// An interrupted Claude build resumes its session on the next claim: --resume, a short note for a
// prompt, and resumed=true in the run's fields; the verdict forgets the session.
func TestInterruptedBuildResumesItsSession(t *testing.T) {
	g := newRig(t, fakeclaude.Hang)
	id := g.stoppedBuild("Resume me")
	s := g.buildSession(id)
	branch := fmt.Sprintf("loop/%d-resume-me", id)
	if s == nil || s.Session != "fake-session-hang" || s.Reason != endStopped || s.Head != g.remoteHead(branch) || s.Resumes != 0 {
		t.Fatalf("session record %+v (WIP head %s)", s, g.remoteHead(branch))
	}
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Review {
		t.Fatalf("stage %s %q; log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	run := g.lastRun(id, "build")
	argv := argvOf(t, run)
	if i := slices.Index(argv, "--resume"); i < 0 || argv[i+1] != "fake-session-hang" {
		t.Fatalf("argv %q", argv)
	}
	prompt := g.read(filepath.Join(run, "brief.md"))
	if !strings.HasPrefix(prompt, ResumeHeading) || !strings.Contains(prompt, "by a stop") || !strings.Contains(prompt, "`git status`") ||
		strings.Contains(prompt, ItemHeading) {
		t.Fatalf("a resumed build's prompt is the note alone:\n%s", prompt)
	}
	ev := g.advanced(id)
	if ev.Data["resumed"] != "true" || ev.Data["resumed_from"] != "fake-session-hang" {
		t.Fatalf("fields %v", ev.Data)
	}
	if g.buildSession(id) != nil {
		t.Fatal("a build that reached a verdict keeps no session")
	}
	// The resumed run continued the WIP: the branch has it and the feature.
	if files := gitT(t, g.remote, "ls-tree", "--name-only", "-r", g.remoteHead(branch)); !strings.Contains(files, "wip.txt") || !strings.Contains(files, "feature.txt") {
		t.Fatalf("files %q", files)
	}
}

// A new build (no record) says resumed=false.
func TestNewBuildIsNotResumed(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toBuild("Fresh")
	g.runOnce()
	if ev := g.advanced(id); ev.Data["resumed"] != "false" || ev.Data["resumed_from"] != nil {
		t.Fatalf("fields %v", ev.Data)
	}
	if slices.Contains(argvOf(t, g.lastRun(id, "build")), "--resume") {
		t.Fatal("a new build resumes nothing")
	}
}

// claude no longer has the session: the same claim starts a new one, with the whole brief.
func TestResumeFallsBackWhenTheSessionIsGone(t *testing.T) {
	g := newRig(t, fakeclaude.Hang)
	id := g.stoppedBuild("Gone")
	os.RemoveAll(filepath.Join(g.scripts, "sessions"))
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Review {
		t.Fatalf("stage %s %q; log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	run := g.lastRun(id, "build")
	if slices.Contains(argvOf(t, run), "--resume") || !strings.Contains(g.read(filepath.Join(run, "brief.md")), ItemHeading) {
		t.Fatal("the fallback is a new session with the whole brief")
	}
	if !strings.Contains(g.read(filepath.Join(run, "stderr-1.log")), "No conversation found") {
		t.Fatal("the failed resume's stderr is kept")
	}
	if ev := g.advanced(id); ev.Data["resumed"] != "false" || ev.Data["session_id"] != "fake-session-good" {
		t.Fatalf("fields %v", ev.Data)
	}
	if !strings.Contains(g.logs.String(), "session to resume is gone") {
		t.Fatalf("log:\n%s", g.logs.String())
	}
}

// A session is not resumed when the branch moved since (someone else's work), when it was resumed
// MaxBuildResumes times, or when it was another stage's.
func TestBuildSessionNotResumed(t *testing.T) {
	for name, change := range map[string]func(g *rig, id int64, s *buildSession){
		"branch moved":   func(g *rig, id int64, s *buildSession) { s.Head = strings.Repeat("0", 40) },
		"resumed enough": func(g *rig, id int64, s *buildSession) { s.Resumes = MaxBuildResumes },
		"another stage":  func(g *rig, id int64, s *buildSession) { s.Stage = StageReview },
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, fakeclaude.Hang)
			id := g.stoppedBuild("Not resumed")
			s := g.buildSession(id)
			change(g, id, s)
			g.setBuildSession(id, s)
			g.runOnce()
			if d := g.item(id); d.Item.Stage != pipeline.Review {
				t.Fatalf("stage %s; log:\n%s", d.Item.Stage, g.logs.String())
			}
			if slices.Contains(argvOf(t, g.lastRun(id, "build")), "--resume") || g.advanced(id).Data["resumed"] != "false" {
				t.Fatal("resumed")
			}
			if !strings.Contains(g.logs.String(), "not resuming the build's session") {
				t.Fatalf("log:\n%s", g.logs.String())
			}
		})
	}
}

// The runner shutting down keeps the session; a crash (a record still running, and the old
// worktree with uncommitted work) saves that work as WIP first, then resumes.
func TestCrashedBuildSavesWorkAndResumes(t *testing.T) {
	g := newRig(t, fakeclaude.Hang)
	id := g.toBuild("Crash")
	ctx, cancel := context.WithCancel(g.ctx)
	done := make(chan struct{})
	go func() { defer close(done); g.r.ClaimAndRun(ctx) }()
	g.clk.BlockUntil(2)
	waitFile(t, filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-build", id), "wip.txt"))
	cancel()
	<-done
	s := g.buildSession(id)
	if s == nil || s.Reason != endShutdown {
		t.Fatalf("after a shutdown: %+v", s)
	}
	// Make it a crash: the record says running, and the worktree is still there, with work in it.
	s.Reason, s.Interrupted = "", time.Time{}
	g.setBuildSession(id, s)
	branch := fmt.Sprintf("loop/%d-crash", id)
	wt := filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-build", id))
	if _, err := g.r.Git.AddWorktree(g.ctx, filepath.Join(g.r.Cfg.WorkRoot, "mirrors", "demo.git"), wt, branch, "main", Recorded{}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt, "unsaved.txt"), []byte("written before the crash\n"), 0o644)
	// The release lease expires; the item is back in its queue.
	g.loopdClk.Advance(store.LeaseTTL + time.Minute)
	g.script(id, "build", fakeclaude.Good, "")
	g.clk.Advance(time.Second)
	g.runOnce()
	if d := g.item(id); d.Item.Stage != pipeline.Review {
		t.Fatalf("stage %s %q; log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	run := g.lastRun(id, "build")
	if i := slices.Index(argvOf(t, run), "--resume"); i < 0 {
		t.Fatalf("not resumed; log:\n%s", g.logs.String())
	}
	if p := g.read(filepath.Join(run, "brief.md")); !strings.Contains(p, "crash") || !strings.Contains(p, "may be lost") {
		t.Fatalf("prompt:\n%s", p)
	}
	files := gitT(t, g.remote, "ls-tree", "--name-only", "-r", g.remoteHead(branch))
	if !strings.Contains(files, "unsaved.txt") {
		t.Fatalf("the crashed run's work was not saved: %q", files)
	}
	if msgs := gitT(t, g.remote, "log", "--format=%s", g.remoteHead(branch)); !strings.Contains(msgs, "WIP: loop item #"+itoa(id)+" (crashed)") {
		t.Fatalf("commits:\n%s", msgs)
	}
}

// The time cap keeps the session (the item goes to the user, whose rebuild resumes it).
func TestTimeCapKeepsTheSession(t *testing.T) {
	g := newRig(t, fakeclaude.Hang)
	one := 1
	g.st.SetControl(g.ctx, "user:alex", store.ControlPatch{MaxRunMinutes: &one})
	id := g.toBuild("Capped")
	wait := g.runAsync()
	g.clk.BlockUntil(2)
	waitFile(t, filepath.Join(g.r.Cfg.WorkRoot, fmt.Sprintf("%d-build", id), "wip.txt"))
	g.clk.Advance(30 * time.Second)
	wait()
	if s := g.buildSession(id); s == nil || s.Reason != endTimeCap || s.Head != g.remoteHead(fmt.Sprintf("loop/%d-capped", id)) {
		t.Fatalf("%+v", s)
	}
}

func TestResumeNote(t *testing.T) {
	at := time.Date(2026, 10, 17, 21, 5, 0, 0, time.UTC)
	for reason, want := range map[string]string{
		endShutdown: "interrupted at 2026-10-17 21:05 UTC by the runner shutting down",
		endTimeCap:  "by the run's time cap",
		endCrashed:  "may be lost",
	} {
		if n := ResumeNote(&buildSession{Reason: reason, Interrupted: at}); !strings.Contains(n, want) || !strings.Contains(n, "`git log`") {
			t.Errorf("%s:\n%s", reason, n)
		}
	}
}
