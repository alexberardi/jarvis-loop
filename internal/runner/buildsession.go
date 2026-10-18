package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// A Claude build's session is kept by the runner, not loopd, so that a build that was interrupted
// (/loop stop, the time cap, the runner shutting down or crashing) resumes it on the next claim
// instead of starting over. Runner-local on purpose: a crash reports nothing to loopd, and another
// runner (another machine) has no record, so it starts a new session, which is all it could do.

// MaxBuildResumes is how many times one session is resumed; a build that keeps getting cut off
// (the time cap, again and again) then starts a new one.
const MaxBuildResumes = 2

// buildSession is the record of an item's last Claude build session (WorkRoot/sessions/).
type buildSession struct {
	Session string    `json:"session"`
	Stage   string    `json:"stage"`
	Head    string    `json:"head"` // the branch's head the session's work is at
	Started time.Time `json:"started"`
	// Reason is how the run was interrupted (an end reason, or endCrashed); "" while it runs, so
	// a record still "" on the next claim is a run the runner never saw end.
	Reason      string    `json:"reason,omitempty"`
	Interrupted time.Time `json:"interrupted,omitzero"`
	Resumes     int       `json:"resumes,omitempty"`
}

// endCrashed is a record's reason when the next claim found it still running.
const endCrashed = "crashed"

func (r *Runner) buildSessionPath(id int64, stage string) string {
	return filepath.Join(r.Config().WorkRoot, "sessions", fmt.Sprintf("%d-%s.json", id, stage))
}

// claudeBuild: the run is a build by claude, whose session the runner keeps.
func (ru *run) claudeBuild() bool {
	return ru.stage == StageBuild && ru.builder == pipeline.BuilderClaude && ru.plan == nil && ru.orch == nil
}

func (r *Runner) loadBuildSession(ru *run) *buildSession {
	b, err := os.ReadFile(r.buildSessionPath(ru.item.ID, ru.stage))
	if err != nil {
		return nil
	}
	var s buildSession
	if json.Unmarshal(b, &s) != nil || s.Session == "" {
		return nil
	}
	return &s
}

func (r *Runner) saveBuildSession(ru *run, s *buildSession) {
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := writeAtomic(r.buildSessionPath(ru.item.ID, ru.stage), b); err != nil {
		r.Log.Warn("saving the build's session", "item", ru.item.ID, "err", err)
	}
}

// dropBuildSession forgets the item's session: its build reached a verdict, or it cannot be
// resumed.
func (r *Runner) dropBuildSession(ru *run) {
	if ru.claudeBuild() {
		os.Remove(r.buildSessionPath(ru.item.ID, ru.stage))
	}
}

// recordBuildSession keeps the session a Claude build just started (or resumed).
func (r *Runner) recordBuildSession(ru *run, session string) {
	if !ru.claudeBuild() || session == "" {
		return
	}
	s := &buildSession{Session: session, Stage: ru.stage, Head: ru.startHead, Started: r.Clock.Now()}
	if ru.resumedFrom != nil && ru.resumedFrom.Session == session {
		s.Resumes = ru.resumedFrom.Resumes + 1
	}
	r.saveBuildSession(ru, s)
}

// buildInterrupted marks the session interrupted, at the head its saved work left the branch.
func (r *Runner) buildInterrupted(ctx context.Context, ru *run, reason string) {
	if !ru.claudeBuild() {
		return
	}
	s := r.loadBuildSession(ru)
	if s == nil {
		return
	}
	s.Reason, s.Interrupted = reason, r.Clock.Now()
	if head, err := r.Git.Head(ctx, ru.wt); err == nil {
		s.Head = head
	}
	r.saveBuildSession(ru, s)
}

// saveCrashedWork commits what a crashed run left uncommitted in the item's old worktree (before
// prepareBuild replaces it) and pushes it as WIP, as a stop would have; the session's head is
// then the WIP commit.
func (r *Runner) saveCrashedWork(ctx context.Context, ru *run, s *buildSession, log *slog.Logger) {
	s.Reason, s.Interrupted = endCrashed, r.Clock.Now()
	if _, err := os.Stat(filepath.Join(ru.wt, ".git")); err == nil {
		r.saveWIP(ctx, ru, endCrashed, log)
		if head, err := r.Git.Head(ctx, ru.wt); err == nil {
			s.Head = head
		}
	}
	r.saveBuildSession(ru, s)
}

// resumable says why the session cannot be resumed ("" when it can).
func (s *buildSession) resumable(stage, head string) string {
	switch {
	case s.Stage != stage:
		return "it was a " + s.Stage + " session"
	case s.Head != head:
		return fmt.Sprintf("the branch moved since (%s, the session left it at %s)", short7(head), short7(s.Head))
	case s.Resumes >= MaxBuildResumes:
		return fmt.Sprintf("it was resumed %d times already", s.Resumes)
	}
	return ""
}

// ResumeHeading starts a resumed build's prompt.
const ResumeHeading = "# Your build was interrupted\n"

// ResumeNote is the whole prompt of a resumed build: the session already has the brief.
func ResumeNote(s *buildSession) string {
	why := map[string]string{
		endStopped:  "a stop (`/loop stop`, or loopd ended the lease)",
		endShutdown: "the runner shutting down",
		endTimeCap:  "the run's time cap",
		endCrashed:  "the runner stopping without warning (a crash or a kill)",
	}[s.Reason]
	if why == "" {
		why = s.Reason
	}
	at := s.Interrupted
	if at.IsZero() {
		at = s.Started
	}
	note := fmt.Sprintf("%sYour run was interrupted at %s by %s; this is the same session, resumed. Continue from where you were.\n\n",
		ResumeHeading, at.UTC().Format("2006-01-02 15:04 UTC"), why)
	if s.Reason == endCrashed {
		note += "The runner could not save your work when it stopped: what was committed is on the branch, and what it found uncommitted in the old worktree it committed as WIP. Work written after that may be lost.\n\n"
	} else {
		note += "The runner committed your uncommitted work as a WIP commit and pushed the branch.\n\n"
	}
	note += "The worktree was made again from the branch, so check `git status` and `git log` first. " +
		"Background processes you started are gone. The brief is still in `.loop/brief.md`; finish with `.loop/result.json` as it says.\n"
	return note
}
