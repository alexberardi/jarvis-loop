// Package fakeclaude is a stand-in for `claude -p --output-format stream-json`, for the runner's
// tests and its dry run. It speaks the stream-json shapes the runner reads, runs the hooks from
// the --settings file the way Claude Code does (SessionStart, then PreToolUse before every tool
// call, honouring a deny), and plays a scripted scenario chosen by FAKE_CLAUDE_SCENARIO. It never
// talks to a model, GitHub or loopd.
//
// For the loop's harness (internal/sandbox) it also takes a script per item: with
// FAKE_CLAUDE_SCRIPTS=<dir>, the file <dir>/<item id>.json ({"play": "<scenario>", "file":
// "<path to change>"}) overrides the scenario for that item's runs, and the fake appends its
// progress ("started", "hanging") to <dir>/<item id>.state, so the harness knows when a hanging
// agent is really running. These files are the harness's plumbing, not tool calls.
//
// Called as `run ...` it is a stand-in for `opencode run --format json` instead (the runner's
// qwen builder): it reads the provider from $OPENCODE_CONFIG, makes one chat request through it
// (the runner's proxy, which must answer 200), resumes a session it started with -s (in the same
// directory) and plays the same build scripts, speaking OpenCode's JSON events. It runs no hooks.
package fakeclaude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Scenarios.
const (
	Good   = "good"   // commit on the item branch, evidence, a ready_for_pr result (the runner pushes)
	Escape = "escape" // try to write outside the worktree and push main first (refused), then good
	// Repoint the worktree's git (its .git file, the admin dir's commondir and config.worktree,
	// renaming the admin dir and the mirror's worktrees dir away), then good.
	GitTamper = "git_tamper"
	// Give the mirror a commondir (with a test that left the mirror writable), then good.
	GitTamperMirror = "git_tamper_mirror"
	// Plant in the shared mirror: its commondir, another worktree's commondir and a file in its
	// worktrees dir, then good.
	GitTamperShared = "git_tamper_shared"
	Lying           = "lying"      // good, but the result's head_sha is not HEAD
	Malformed       = "malformed"  // commit, then a result that is not JSON
	NoResult        = "noresult"   // commit, exit 0, no result
	Question        = "question"   // a question result
	CommitAsk       = "commit_ask" // commit, then a question (the commit stays in the runner's mirror)
	Dirty           = "dirty"      // good, but a change left uncommitted
	Hang            = "hang"       // write a file, then wait forever
	// Wait reports hanging, waits until the harness releases the item (<dir>/<item id>.release),
	// then plays good: a run still going while something else happens.
	Wait       = "wait_release"
	IgnoreTerm = "ignore_term" // like hang, but ignore SIGTERM
	// TodosHang is hang after a to-do list (TodoWrite) and a described Bash call whose command
	// holds SECRET_CMD_MARKER (which must never reach the board).
	TodosHang = "todos_hang"
	Exit3     = "exit3" // print something and exit 3
	// LockError prints git's stale index.lock error (with the worktree's path) and exits 3, as
	// the runs of 2026-10-16 did.
	LockError = "lock_error"
	// HarnessQuestion asks to skip what it can't run in its sandbox (the emulator).
	HarnessQuestion = "harness_question"
	UsageLimit      = "usage_limit" // the plan's session limit
	NoArm           = "noarm"       // run tools without the hooks (settings not applied)
	WrongMode       = "wrong_mode"  // start in default mode whatever was asked
	SymlinkEv       = "symlink_evidence"
	LyingOnce       = "lying_once" // lying, then good on the runner's one retry
	LowCache        = "low_cache"  // good, but with a cold prompt cache (10% read over 6 turns)
	// SuggestVerify is good, but the agent also leaves a verify script of its own in .loop/ and
	// asks for it in the PR body; the runner must never run it (it would touch
	// <scripts dir>/../agent-verify-ran).
	SuggestVerify = "suggest_verify"
	// NoChange is good on a fresh branch; on a resumed one (a rerun after a bounce) it commits
	// nothing and reports the same head, as an agent that found nothing to fix would.
	NoChange = "no_change"
	// TrustedChange changes the script's file with a shell command (the guard refuses Write and
	// Edit there: it is one of the repo's deny_write paths) and commits it. On a rerun whose brief
	// says the user OK'd that commit for verify, it commits nothing; on any other rerun it commits
	// another change to the file (a new head, which needs a new OK).
	TrustedChange = "trusted_change"
	// Harness is good, after using the repo's harness through the runner: loop-harness up, then a
	// run that writes .loop/evidence/harness.txt. HarnessHang does the same, then hangs.
	Harness     = "harness"
	HarnessHang = "harness_hang"

	// Triage plays (a triage run of a build play plays Reproduce).
	Reproduce      = "reproduce"       // reproduced, with notes and a repro file; picks the script's repo when the item has none
	Duplicate      = "duplicate"       // not_reproducible: the open item with the same title, and its PR
	TriageQuestion = "triage_question" // a question
	TriageBadRepo  = "triage_bad_repo" // reproduced, naming a repo that is not configured
	// Route names the script's repo for an item with none (routed); once the item has a repo, it
	// plays Reproduce.
	Route = "route"

	// Review plays (a review run of a build play plays Approve).
	Approve      = "approve"       // approved, with a review for the PR comment
	Bounce       = "bounce"        // bounce, with notes naming the changed file
	ReviewCommit = "review_commit" // tries to commit a fixup (the guard refuses), then approves
	ApproveHold  = "approve_hold"  // approved, with needs_review (content a path rule cannot see)
	ApproveQA    = "approve_qa"    // approved, with needs_qa (the user should check it once merged)
	// ApproveToolState is approve, with a tool's own state left in .loop/evidence/.claude/ (as on
	// #26); ApproveNamesDir also names that directory as evidence, which the runner refuses;
	// ApproveNamesDirOnce names it only until the runner's one retry.
	ApproveToolState    = "approve_tool_state"
	ApproveNamesDir     = "approve_names_dir"
	ApproveNamesDirOnce = "approve_names_dir_once"

	// Plan turns (a prompt with "- Plan: P<id>" and "- Persona: <persona>"; the script is
	// <dir>/P<id>-<persona>.json). Every play writes out/result.json with a reply.
	PlanBrief     = "brief"         // product: out/brief.md (the turn's messages in it), ready
	PlanBlueprint = "blueprint"     // engineering: a 3-piece blueprint (2 and 3 depend on 1), ready
	BackToBrief   = "back_to_brief" // engineering: sends the plan back to the brief
	PlanCycle     = "cycle"         // engineering: a blueprint whose pieces depend on each other
	PlanEscape    = "plan_escape"   // tries to write outside out/ (refused), then the persona's usual
	// PlanWait waits until the harness releases it (the file <dir>/P<id>.release), reporting
	// "waiting", then plays the persona's usual.
	PlanWait = "wait"

	// Orchestrate turns (a prompt with "- Orchestrate: turn <n>"; the script is
	// <dir>/orchestrator-orchestrate.json, the state <dir>/orchestrator.state). Every play writes
	// out/result.json.
	OrchReact = "react" // retry the needs_you items the turn's run_failed or due triggers point at, and a note
	OrchNote  = "note"  // a note only
	// OrchAgentsUp asks to raise the active agents (loopd refuses it) and notes the last turn's
	// refusals it was told about.
	OrchAgentsUp    = "agents_up"
	OrchInvalid     = "invalid"      // an action it does not have (approve), every time
	OrchInvalidOnce = "invalid_once" // invalid, then react on the runner's one retry
	// OrchPersona plays the orchestrator's skill as rules on the snapshot (persona.go): retries
	// and failure signatures, the answer kinds, escalations with a recommendation, P1 tickets.
	OrchPersona = "persona"
)

var stagePlays = map[string][]string{
	"triage": {Reproduce, Duplicate, TriageQuestion, TriageBadRepo, Route, Hang, UsageLimit, Exit3},
	"review": {Approve, Bounce, ReviewCommit, ApproveHold, ApproveQA, ApproveToolState, ApproveNamesDir, ApproveNamesDirOnce, Hang, UsageLimit, Exit3},
	// Plan personas.
	"product":     {PlanBrief, PlanEscape, PlanWait, Hang, UsageLimit, Exit3},
	"engineering": {PlanBlueprint, BackToBrief, PlanCycle, PlanEscape, PlanWait, Hang, UsageLimit, Exit3},
	"orchestrate": {OrchReact, OrchNote, OrchAgentsUp, OrchInvalid, OrchInvalidOnce, OrchPersona, Hang, UsageLimit, Exit3},
}

type fake struct {
	// oc: playing opencode, not claude.
	oc        bool
	out       io.Writer
	cwd       string
	mode      string
	pre, sess string
	session   string
	runHooks  bool
	n         int
	turns     int
	scenario  string
}

func (f *fake) emit(v any) {
	b, _ := json.Marshal(v)
	f.out.Write(append(b, '\n'))
}

// Main runs the fake with claude's arguments and returns its exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	f := &fake{out: stdout, runHooks: true}
	if len(args) > 0 && args[0] == "run" {
		f.oc, f.runHooks = true, false
	}
	var settings, resume string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--resume", "-s":
			i++
			resume = args[i]
		case "--settings":
			i++
			settings = args[i]
		case "--permission-mode":
			i++
			f.mode = args[i]
		}
	}
	f.cwd, _ = os.Getwd()
	scenario := os.Getenv("FAKE_CLAUDE_SCENARIO")
	if scenario == "" {
		scenario = Good
	}
	brief, _ := io.ReadAll(stdin)
	meta := brief
	if resume != "" && find(`Item: #(\d+)`, brief) == "" && find(`(?m)^- (Persona|Orchestrate): `, brief) == "" {
		// A resumed build's prompt is a note alone (the session has the brief): the brief's
		// fields come from the copy in the worktree, as the real claude remembers them.
		full, _ := os.ReadFile(filepath.Join(f.cwd, ".loop", "brief.md"))
		meta = append(full, brief...)
	}
	branch := find("Branch: `([^`]+)`", meta)
	base := find("base branch `([^`]+)`", meta)
	item := find(`Item: #(\d+)`, meta)
	stage := find(`(?m)^- Stage: (\w+)$`, meta)
	if persona := find(`(?m)^- Persona: (\w+)$`, brief); persona != "" {
		// A plan turn: the plan stands in for the item, the persona for the stage.
		item, stage = "P"+find(`(?m)^- Plan: P(\d+):`, brief), persona
	}
	orchTurn := find(`(?m)^- Orchestrate: turn (\d+)`, brief)
	if orchTurn != "" {
		item, stage = "orchestrator", "orchestrate"
	}
	if stage == "" {
		stage = "build"
	}
	sc := loadScript(os.Getenv("FAKE_CLAUDE_SCRIPTS"), item, stage)
	if sc.Play != "" {
		scenario = sc.Play
	}
	if plays, ok := stagePlays[stage]; ok && !contains(plays, scenario) {
		scenario = plays[0] // a build play in a triage or review run: that stage's usual
	}
	f.scenario = scenario
	change := "feature.txt"
	if sc.File != "" {
		change = sc.File
	}
	progress := func(what string) {
		if d := os.Getenv("FAKE_CLAUDE_SCRIPTS"); d != "" && item != "" {
			if fh, err := os.OpenFile(filepath.Join(d, item+".state"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
				fmt.Fprintln(fh, what)
				fh.Close()
			}
		}
	}
	progress("started")
	if b, err := os.ReadFile(settings); err == nil {
		var s struct {
			Hooks map[string][]struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"hooks"`
		}
		json.Unmarshal(b, &s)
		if h := s.Hooks["PreToolUse"]; len(h) > 0 && len(h[0].Hooks) > 0 {
			f.pre = h[0].Hooks[0].Command
		}
		if h := s.Hooks["SessionStart"]; len(h) > 0 && len(h[0].Hooks) > 0 {
			f.sess = h[0].Hooks[0].Command
		}
	}
	if scenario == NoArm {
		f.runHooks = false
	}
	if scenario == IgnoreTerm {
		signal.Ignore(syscall.SIGTERM)
	}
	if f.runHooks && f.sess != "" {
		f.hook(f.sess, map[string]any{"hook_event_name": "SessionStart", "source": "startup", "cwd": f.cwd})
	}
	mode := f.mode
	if scenario == WrongMode {
		mode = "default"
	}
	// Sessions: an item run's is named after its scenario; a plan turn starts a session of its own
	// (one per plan and persona). Any run resumes one (an interrupted build, a plan turn), which,
	// as claude's, must have been made in the same working directory.
	f.session = "fake-session-" + scenario
	if strings.HasPrefix(item, "P") {
		f.session = "fake-session-" + item + "-" + stage
	}
	if orchTurn != "" {
		f.session = "fake-session-orchestrator-" + orchTurn // a new one each day: named after its first turn
	}
	if f.oc {
		f.session = "ses_fake_" + item + "_" + scenario
	}
	// Claude's sessions are the harness's; OpenCode keeps its own in its data dir, the one place
	// the fence lets it write besides the worktree.
	sessions := os.Getenv("FAKE_CLAUDE_SCRIPTS")
	if d := os.Getenv("XDG_DATA_HOME"); f.oc && d != "" {
		sessions = filepath.Join(d, "opencode")
	}
	if resume != "" {
		if err := checkSession(sessions, resume, f.cwd); err != nil {
			fmt.Fprintf(stderr, "No conversation found with session ID: %s (%v)\n", resume, err)
			return 1
		}
		f.session = resume
	} else {
		saveSession(sessions, f.session, f.cwd)
	}
	if f.oc {
		progress("opencode " + f.session)
		// The real OpenCode writes a .gitignore into its config dir at start (and installs its
		// provider package there) and exits with "FileSystem.writeFile" when it can't.
		if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
			err := os.MkdirAll(filepath.Join(d, "opencode"), 0o700)
			if err == nil {
				err = os.WriteFile(filepath.Join(d, "opencode", ".gitignore"), []byte("node_modules\n"), 0o600)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: Unexpected error\n\nUnknown: FileSystem.writeFile (%s)\n", filepath.Join(d, "opencode", ".gitignore"))
				return 1
			}
		}
		// OpenCode's session store, which the runner looks for before it resumes.
		if d := os.Getenv("XDG_DATA_HOME"); d != "" {
			os.MkdirAll(filepath.Join(d, "opencode"), 0o700)
			os.WriteFile(filepath.Join(d, "opencode", "opencode.db"), []byte("fake\n"), 0o600)
		}
		if err := chat(os.Getenv("OPENCODE_CONFIG")); err != nil {
			f.emit(map[string]any{"type": "error", "sessionID": f.session, "error": map[string]any{"name": "APIError",
				"data": map[string]any{"message": err.Error()}}})
			return 1
		}
	} else {
		f.emit(map[string]any{"type": "system", "subtype": "init", "session_id": f.session, "cwd": f.cwd, "model": "fake-model",
			"permissionMode": mode, "tools": []string{"Bash", "Read", "Edit", "Write"}, "apiKeySource": "none", "claude_code_version": "fake"})
	}

	if scenario == Harness || scenario == HarnessHang {
		f.bash("loop-harness up && loop-harness run 'echo harness-ok > .loop/evidence/harness.txt'")
		if scenario == HarnessHang {
			scenario = Hang
		}
	}
	if scenario == Wait {
		progress("hanging")
		release := filepath.Join(os.Getenv("FAKE_CLAUDE_SCRIPTS"), item+".release")
		for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if os.Remove(release) == nil {
				break
			}
		}
		scenario = Good
	}
	switch scenario {
	case TodosHang:
		f.tool("TodoWrite", map[string]any{"todos": []any{
			map[string]any{"content": "Read the brief", "activeForm": "Reading the brief", "status": "completed"},
			map[string]any{"content": "Write the failing test", "activeForm": "Writing the failing test", "status": "in_progress"},
			map[string]any{"content": "Fix the <b>board</b>", "activeForm": "Fixing the board", "status": "pending"},
		}}, func() (string, error) { return "Todos have been modified successfully.", nil })
		f.tool("Bash", map[string]any{"command": "true SECRET_CMD_MARKER", "description": "Run the board tests"}, func() (string, error) {
			return "", nil
		})
		f.write("wip.txt", "half done\n")
		f.say("Writing the failing test for the card.")
		progress("hanging")
		for {
			time.Sleep(time.Hour)
		}
	case Hang, IgnoreTerm:
		f.write("wip.txt", "half done\n")
		f.say("working on it")
		progress("hanging")
		for {
			time.Sleep(time.Hour)
		}
	case Exit3:
		f.say("I could not build it: the tests do not compile.")
		return 3
	case LockError:
		f.say("fatal: Unable to create '" + filepath.Join(f.cwd, ".git", "index.lock") + "': File exists.")
		return 3
	case HarnessQuestion:
		f.write(".loop/result.json", `{"outcome":"question","question":"I can't run the emulator in my sandbox: may I open the PR without the screenshots?"}`)
		f.result("Asked a question.")
		return 0
	case UsageLimit:
		f.emit(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "rejected", "resetsAt": 1792079100}})
		f.emit(map[string]any{"type": "assistant", "error": "rate_limit", "message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "You've hit your session limit · resets 3:45pm"}}}})
		f.emit(map[string]any{"type": "result", "subtype": "success", "is_error": true, "num_turns": 1, "duration_ms": 900,
			"total_cost_usd": 0.01, "result": "You've hit your session limit · resets 3:45pm"})
		return 1
	case Question:
		f.write(".loop/result.json", `{"outcome":"question","question":"Should the timer round up or down?"}`)
		f.result("Asked a question.")
		return 0
	case WrongMode:
		f.say("hello from manual mode")
		f.result("nothing done")
		return 0
	case GitTamper, GitTamperMirror, GitTamperShared:
		admin := strings.TrimPrefix(strings.TrimSpace(readFile(filepath.Join(f.cwd, ".git"))), "gitdir: ")
		mirror := filepath.Dir(filepath.Dir(admin))
		evil := filepath.Join(f.cwd, ".loop", "scratch", "evil.git")
		f.bash("git init -q --bare " + shellQuote(evil) + " && git --git-dir=" + shellQuote(evil) + " config core.fsmonitor 'touch " +
			filepath.Join(f.cwd, ".loop", "scratch", "pwned") + "'")
		if scenario == GitTamperMirror {
			f.write(filepath.Join(mirror, "commondir"), evil+"\n")
			break
		}
		if scenario == GitTamperShared {
			f.write(filepath.Join(mirror, "commondir"), evil+"\n")
			ents, _ := os.ReadDir(filepath.Join(mirror, "worktrees"))
			for _, e := range ents {
				if other := filepath.Join(mirror, "worktrees", e.Name()); other != admin {
					f.write(filepath.Join(other, "commondir"), evil+"\n")
				}
			}
			f.write(filepath.Join(mirror, "worktrees", "planted", "gitdir"), evil+"\n")
			break
		}
		f.write(".git", "gitdir: "+evil+"\n")
		f.write(filepath.Join(admin, "commondir"), evil+"\n")
		f.write(filepath.Join(admin, "config.worktree"), "[core]\n\tfsmonitor = touch pwned\n")
		f.bash("mv .git .git.moved")
		f.bash("mv " + shellQuote(admin) + " " + shellQuote(admin+".moved"))
		f.bash("mv " + shellQuote(filepath.Dir(admin)) + " " + shellQuote(filepath.Dir(admin)+".moved"))
	case Escape:
		outside := filepath.Join(filepath.Dir(f.cwd), "escaped.txt")
		f.write(outside, "I got out\n")
		f.bash("git push origin main")
	}
	// An item filed from a plan: the brief points at the plan's approved blueprint for the
	// sections its piece refers to; read it, as an agent would.
	if stage != "product" && stage != "engineering" && bytes.Contains(brief, []byte("`.loop/context/blueprint.md`")) {
		f.bash("cat .loop/context/blueprint.md")
	}
	switch stage {
	case "triage":
		return f.triage(scenario, brief, sc)
	case "review":
		return f.review(scenario, brief)
	case "product", "engineering":
		return f.planTurn(scenario, stage, item, brief, sc, progress)
	case "orchestrate":
		return f.orchestrate(scenario, orchTurn, brief, progress)
	}

	// The common path: a change, committed on the item's branch (a resumed run finds it there).
	// After a bounce, the review's notes are in .loop/review/: address them in a new commit.
	reviewed := ""
	resumed := strings.TrimSpace(git(f.cwd, "rev-list", "--count", "origin/"+base+"..HEAD")) != "0"
	if scenario == NoChange && resumed {
		// Nothing to fix: no commit.
	} else if scenario == TrustedChange {
		if !resumed || !bytes.Contains(brief, []byte(trustedBrief)) {
			f.bash("mkdir -p \"$(dirname " + shellQuote(change) + ")\" && echo 'shoot --dark' >> " + shellQuote(change) + " && git add " + shellQuote(change) +
				" && git -c user.name=fake -c user.email=fake@example.com commit -q -m 'harness: shoot dark mode too'")
		}
	} else if ents, err := os.ReadDir(filepath.Join(f.cwd, ".loop", "review")); err == nil && len(ents) > 0 {
		var names []string
		for _, e := range ents {
			names = append(names, ".loop/review/"+e.Name())
		}
		reviewed = strings.Join(names, ", ")
		old, _ := os.ReadFile(filepath.Join(f.cwd, change))
		f.write(change, string(old)+"addressed the review\n")
		f.bash("git add " + shellQuote(change) + " && git -c user.name=fake -c user.email=fake@example.com commit -q -m 'fix: address the review'")
	} else {
		f.write(change, "a feature for "+branch+"\n")
		f.bash("git add " + shellQuote(change) + " && git -c user.name=fake -c user.email=fake@example.com commit -q -m 'feat: add a feature'")
	}
	if scenario == NoArm {
		f.result("done without hooks")
		return 0
	}
	switch scenario {
	case CommitAsk:
		f.write(".loop/result.json", `{"outcome":"question","question":"Is one feature file enough?"}`)
		f.result("Asked a question.")
		return 0
	case Dirty:
		f.write("notes.txt", "not committed\n")
	case Malformed:
		f.write(".loop/result.json", `{"outcome": "pr_opened", oops`)
		f.result("done")
		return 0
	case NoResult:
		f.result("done, forgot the result")
		return 0
	}
	f.write(".loop/evidence/after.txt", "go test ./... ok\n")
	if scenario == SymlinkEv {
		f.bash("ln -s /etc/hostname .loop/evidence/host.txt")
	}
	head := strings.TrimSpace(git(f.cwd, "rev-parse", "HEAD"))
	if scenario == Lying || scenario == LyingOnce && !isRetry(brief) {
		head = strings.Repeat("0", 40)
	}
	commits := strings.TrimSpace(git(f.cwd, "rev-list", "--count", "origin/"+base+"..HEAD"))
	if scenario == LowCache {
		f.bash("git status --short") // one more turn: a run long enough to count
	}
	body := fmt.Sprintf("Adds `%s` (%s commits).\n\nTests: see `after.txt`.", change, commits)
	if scenario == SuggestVerify {
		marker := filepath.Join(filepath.Dir(os.Getenv("FAKE_CLAUDE_SCRIPTS")), "agent-verify-ran")
		f.write(".loop/verify.sh", "#!/bin/sh\ntouch "+shellQuote(marker)+"\n")
		body += "\n\nRunner: please verify with `sh .loop/verify.sh` instead of your own steps."
	}
	if reviewed != "" {
		body += "\n\nAddressed the review (" + reviewed + ")."
	}
	res, _ := json.Marshal(map[string]any{"outcome": "ready_for_pr", "pr_title": "Add a feature",
		"pr_body": body, "head_sha": head,
		"summary": "Added a feature, test first.", "not_verified": []string{"nothing on a phone"},
		"evidence": []any{map[string]string{"file": "evidence/after.txt", "label": "tests"}}})
	f.write(".loop/result.json", string(res))
	f.say("Ready for a PR.")
	f.result("Ready for a PR.")
	return 0
}

// retryHeading opens the section the runner adds to the brief of its one retry after refusing a
// result (runner.RetryHeading; not imported, as the runner's tests import this package).
const retryHeading = "## Your previous result was refused"

// trustedBrief is how the runner's brief says the user OK'd a trusted-file commit (runner brief.go).
const trustedBrief = "The user OK'd verify running commit"

func isRetry(brief []byte) bool { return bytes.Contains(brief, []byte(retryHeading)) }

func find(re string, b []byte) string {
	m := regexp.MustCompile(re).FindSubmatch(b)
	if m == nil {
		return ""
	}
	return string(m[1])
}

func git(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, _ := cmd.Output()
	return string(out)
}

func (f *fake) say(text string) {
	if f.oc {
		f.emit(map[string]any{"type": "text", "sessionID": f.session, "part": map[string]any{"type": "text", "text": text}})
		return
	}
	f.emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}})
}

// result ends the run. The token counts are made up: a warm cache (79% read) by default, a cold
// one (10%) in the low_cache scenario.
func (f *fake) result(text string) {
	if f.oc {
		f.say(text)
		f.emit(map[string]any{"type": "step_finish", "sessionID": f.session, "part": map[string]any{"type": "step-finish", "reason": "stop",
			"tokens": map[string]any{"input": 12000, "output": 800}, "cost": 0}})
		return
	}
	usage := map[string]any{"input_tokens": 1000, "output_tokens": 4000, "cache_creation_input_tokens": 20000, "cache_read_input_tokens": 79000}
	if f.scenario == LowCache {
		usage = map[string]any{"input_tokens": 70000, "output_tokens": 4000, "cache_creation_input_tokens": 20000, "cache_read_input_tokens": 10000}
	}
	f.emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "num_turns": f.turns + 1,
		"duration_ms": 1234, "total_cost_usd": 0.42, "result": text, "permission_denials": []any{}, "usage": usage,
		"session_id": f.session})
}

// hook runs a hook command as Claude Code does (bash, JSON on stdin) and reports a deny.
func (f *fake) hook(command string, input map[string]any) (bool, string) {
	b, _ := json.Marshal(input)
	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = f.cwd
	cmd.Stdin = bytes.NewReader(b)
	// The runner's tests run this package from the test binary; the hook is that binary too.
	cmd.Env = append(os.Environ(), "LOOP_TEST_AS=guard")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 2 {
		var resp struct {
			HSO struct {
				Reason string `json:"permissionDecisionReason"`
			} `json:"hookSpecificOutput"`
		}
		if json.Unmarshal(out.Bytes(), &resp) == nil && resp.HSO.Reason != "" {
			return true, resp.HSO.Reason
		}
		return true, strings.TrimSpace(errb.String())
	}
	return false, ""
}

// tool plays one tool call: the assistant's tool_use, the PreToolUse hook, the tool, the result.
func (f *fake) tool(name string, input map[string]any, run func() (string, error)) {
	f.n++
	f.turns++
	id := fmt.Sprintf("toolu_%02d", f.n)
	if f.oc {
		out, err := run()
		state := map[string]any{"status": "completed", "input": input, "output": out}
		if err != nil {
			state["status"], state["error"] = "error", err.Error()
		}
		f.emit(map[string]any{"type": "tool_use", "sessionID": f.session, "part": map[string]any{"type": "tool",
			"tool": strings.ToLower(name), "callID": id, "state": state}})
		return
	}
	f.emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}}})
	result, isErr := "", false
	denied, reason := false, ""
	if f.runHooks && f.pre != "" {
		denied, reason = f.hook(f.pre, map[string]any{"hook_event_name": "PreToolUse", "tool_name": name, "tool_input": input,
			"cwd": f.cwd, "permission_mode": f.mode, "tool_use_id": id})
	}
	if denied {
		result, isErr = "PreToolUse:"+name+" hook error: "+reason, true
	} else {
		out, err := run()
		result, isErr = out, err != nil
		if err != nil {
			result = err.Error() + ": " + out
		}
	}
	f.emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": id, "content": result, "is_error": isErr}}}})
}

func (f *fake) write(path, content string) {
	p := path
	if !filepath.IsAbs(p) {
		p = filepath.Join(f.cwd, p)
	}
	f.tool("Write", map[string]any{"file_path": p, "content": content}, func() (string, error) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		return "written", os.WriteFile(p, []byte(content), 0o644)
	})
}

func (f *fake) bash(command string) {
	f.tool("Bash", map[string]any{"command": command, "description": "fake"}, func() (string, error) {
		cmd := exec.Command("bash", "-c", command)
		cmd.Dir = f.cwd
		out, err := cmd.CombinedOutput()
		return string(out), err
	})
}

// script is one item's play in the harness: <item>-<stage>.json for a stage, or <item>.json for
// its build runs.
type script struct {
	Play string `json:"play"`
	File string `json:"file"`
	// Repo is the repo triage picks for an item that has none.
	Repo string `json:"repo"`
}

func loadScript(dir, item, stage string) script {
	var s script
	if dir == "" || item == "" {
		return s
	}
	names := []string{item + "-" + stage + ".json"}
	if stage == "build" {
		names = append(names, item+".json")
	}
	for _, n := range names {
		if b, err := os.ReadFile(filepath.Join(dir, n)); err == nil {
			json.Unmarshal(b, &s)
			return s
		}
	}
	return s
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// triage plays a triage run: it reads the runner's context and writes a triage result.
func (f *fake) triage(play string, brief []byte, sc script) int {
	ctxDir := filepath.Join(f.cwd, ".loop", "context")
	open, _ := os.ReadFile(filepath.Join(ctxDir, "open-items.md"))
	merged, _ := filepath.Glob(filepath.Join(ctxDir, "merged-prs-*.md"))
	f.bash("ls repos")
	switch play {
	case TriageQuestion:
		f.write(".loop/result.json", `{"outcome":"question","question":"Which screen do you mean?"}`)
		f.result("Asked a question.")
		return 0
	case Duplicate:
		title := find(`Item: #\d+: (.*)`, brief)
		dup := ""
		for _, line := range strings.Split(string(open), "\n") {
			cells := strings.Split(line, "|")
			if len(cells) < 6 || strings.Contains(line, "(this item)") {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(cells[2]), strings.TrimSpace(title)) {
				dup = "duplicate of " + strings.TrimSpace(cells[1])
				if pr := strings.TrimSpace(cells[5]); pr != "" {
					dup += " / PR " + pr
				}
				break
			}
		}
		if dup == "" {
			dup = "no duplicate found"
		}
		f.write(".loop/evidence/dedupe.txt", fmt.Sprintf("checked the open items and %d merged-PR lists\n", len(merged)))
		res, _ := json.Marshal(map[string]any{"outcome": "not_reproducible", "summary": dup + ": the same ask is already in flight.",
			"notes": "Same title as an open item; nothing new to build.", "evidence": []any{map[string]string{"file": "evidence/dedupe.txt", "label": "dedupe"}}})
		f.write(".loop/result.json", string(res))
		f.result("Duplicate.")
		return 0
	}
	noRepo := strings.Contains(string(brief), "Repository: none yet")
	if play == Route && noRepo {
		f.bash("cat .loop/context/repos.md")
		res, _ := json.Marshal(map[string]any{"outcome": "routed", "repo": sc.Repo,
			"why": "the ask names the " + sc.Repo + " repo's screen (repos.md: group and aliases)"})
		f.write(".loop/result.json", string(res))
		f.result("Routed.")
		return 0
	}
	repo := sc.Repo
	if play == TriageBadRepo {
		repo = "nobody/nothing"
	}
	if !noRepo && play != TriageBadRepo {
		repo = ""
	}
	f.write(".loop/evidence/repro.txt", fmt.Sprintf("reproduced on main; checked the open items and %d merged-PR lists\n", len(merged)))
	r := map[string]any{"outcome": "reproduced", "area": "sandbox", "size": "S", "risk": "normal", "resources": []string{},
		"summary": "Reproduced on main.", "notes": "Change the feature file; verify with the scenario.",
		"evidence": []any{map[string]string{"file": "evidence/repro.txt", "label": "repro"}}}
	if repo != "" {
		r["repo"] = repo
	}
	res, _ := json.Marshal(r)
	f.write(".loop/result.json", string(res))
	f.result("Triaged.")
	return 0
}

// review plays a review run at the PR's head.
func (f *fake) review(play string, brief []byte) int {
	diff, _ := os.ReadFile(filepath.Join(f.cwd, ".loop", "context", "diff.patch"))
	file := find(`(?m)^\+\+\+ b/(.+)$`, diff)
	f.bash("git status --short")
	if play == ReviewCommit {
		f.bash("git -c user.name=fake -c user.email=fake@example.com commit --allow-empty -q -m 'review fixup'")
	}
	f.write(".loop/evidence/review-check.txt", fmt.Sprintf("re-ran the checks at the head; the diff has %d lines\n", strings.Count(string(diff), "\n")))
	var r map[string]any
	if play == Bounce {
		r = map[string]any{"outcome": "bounce", "notes": "`" + file + ":1`: add a test for the empty case.",
			"review": "Changes needed: the empty case is not tested.", "evidence": []any{map[string]string{"file": "evidence/review-check.txt", "label": "review check"}}}
	} else {
		r = map[string]any{"outcome": "approved", "summary": "The tests cover " + file + " and fail without it.",
			"review":   "**Approved.** The tests cover `" + file + "`; re-ran the checks at the head.",
			"evidence": []any{map[string]string{"file": "evidence/review-check.txt", "label": "review check"}}}
		if play == ApproveHold {
			r["needs_review"] = true
			r["review"] = "**Approved, needs your review.** `" + file + "` adds a `curl … | sh` line."
		}
		if play == ApproveQA {
			r["needs_qa"] = true
		}
	}
	switch play {
	case ApproveToolState, ApproveNamesDir, ApproveNamesDirOnce:
		f.write(".loop/evidence/.claude/settings.local.json", "{}\n")
		if play == ApproveNamesDir || play == ApproveNamesDirOnce && !isRetry(brief) {
			r["evidence"] = append(r["evidence"].([]any), map[string]string{"file": "evidence/.claude", "label": "tool state"})
		}
	}
	res, _ := json.Marshal(r)
	f.write(".loop/result.json", string(res))
	f.result("Reviewed.")
	return 0
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// checkSession finds a session the fake started: it must exist and have been started in cwd
// (claude keeps sessions per working directory).
func checkSession(dir, id, cwd string) error {
	if dir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, "sessions", id))
	if err != nil {
		return fmt.Errorf("no such session")
	}
	if string(b) != cwd {
		return fmt.Errorf("it was started in %s, not %s", b, cwd)
	}
	return nil
}

func saveSession(dir, id, cwd string) {
	if dir == "" {
		return
	}
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o755)
	os.WriteFile(filepath.Join(dir, "sessions", id), []byte(cwd), 0o644)
}

// planTurn plays a plan turn: it reads the turn's messages from the prompt and writes the
// persona's documents and out/result.json.
func (f *fake) planTurn(play, persona, plan string, brief []byte, sc script, progress func(string)) int {
	if play == PlanWait {
		progress("waiting")
		release := filepath.Join(os.Getenv("FAKE_CLAUDE_SCRIPTS"), plan+".release")
		for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if os.Remove(release) == nil {
				break
			}
		}
	}
	if play == PlanEscape {
		f.write("notes.md", "outside out/\n")
	}
	if play == PlanWait || play == PlanEscape {
		play = stagePlays[persona][0]
	}
	var msgs []string
	for _, l := range strings.Split(string(brief), "\n") {
		if strings.HasPrefix(l, "> ") {
			msgs = append(msgs, strings.TrimPrefix(l, "> "))
		}
	}
	turn := find(`(?m)^- Turn: (\d+)`, brief)
	title := find(`(?m)^- Plan: P\d+: (.*)$`, brief)
	f.bash("ls -a .loop/context")
	r := map[string]any{}
	switch play {
	case PlanBrief:
		f.write("out/brief.md", fmt.Sprintf("# %s\n\nTurn %s heard: %s\n", title, turn, strings.Join(msgs, " / ")))
		r = map[string]any{"reply": fmt.Sprintf("Here is the brief (turn %s, %d message(s)).", turn, len(msgs)), "brief": "brief.md", "ready": true}
		if sc.Repo != "" {
			r["repos"] = []string{sc.Repo}
		}
	case PlanBlueprint, PlanCycle:
		dep2, dep1 := "1", "none"
		if play == PlanCycle {
			dep1 = "2"
		}
		f.write("out/blueprint.md", fmt.Sprintf("# Blueprint: %s\n\nApproach: one table, then a reader and a page.\n\n## Pieces\n\n"+
			"### 1. The table\n- size: S\n- builder: qwen\n- depends on: %s\n\nThe table and its migration.\n\n"+
			"### 2. The API\n- size: S\n- depends on: %s\n\nList and add.\n\n"+
			"### 3. The page\n- size: S\n- depends on: 1\n\nShow the list.\n\n## Risks\n\nNone.\n", title, dep1, dep2))
		r = map[string]any{"reply": fmt.Sprintf("Here is the blueprint (turn %s): three pieces.", turn), "blueprint": "out/blueprint.md", "ready": true}
	case BackToBrief:
		r = map[string]any{"reply": "This is bigger than the brief says; sending it back.",
			"back_to_brief": "the brief needs offline sync, which is a project of its own: narrow it to online only"}
	}
	res, _ := json.Marshal(r)
	f.write("out/result.json", string(res))
	f.result("Turn done.")
	return 0
}

// chat is the fake opencode's one model request, through the provider its config names (the
// runner's proxy), with the config's key as the bearer token, as OpenCode's OpenAI-compatible
// provider sends it.
func chat(config string) error {
	b, err := os.ReadFile(config)
	if err != nil {
		return fmt.Errorf("no opencode config: %v", err)
	}
	var c struct {
		Model    string `json:"model"`
		Provider map[string]struct {
			Options struct {
				BaseURL string `json:"baseURL"`
				APIKey  string `json:"apiKey"`
			} `json:"options"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return err
	}
	prov, model, _ := strings.Cut(c.Model, "/")
	p, ok := c.Provider[prov]
	if !ok || p.Options.BaseURL == "" {
		return fmt.Errorf("the config has no provider %q", prov)
	}
	body, _ := json.Marshal(map[string]any{"model": model, "stream": true, "messages": []any{map[string]string{"role": "user", "content": "build it"}}})
	req, err := http.NewRequest("POST", p.Options.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.Options.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("Cannot connect to API: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK || !bytes.Contains(out, []byte("[DONE]")) {
		return fmt.Errorf("the model answered %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return nil
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// orchestrate plays an orchestrate turn from loopd's snapshot in .loop/context/snapshot.json.
func (f *fake) orchestrate(play, turn string, brief []byte, progress func(string)) int {
	progress("turn " + turn)
	if play == OrchInvalidOnce {
		play = OrchInvalid
		if isRetry(brief) {
			play = OrchReact
		}
	}
	var snap struct {
		Turn *struct {
			Triggers []struct {
				Reason string `json:"reason"`
				ItemID int64  `json:"item_id"`
			} `json:"triggers"`
		} `json:"turn"`
		Items []struct {
			ID    int64  `json:"id"`
			Stage string `json:"stage"`
		} `json:"items"`
	}
	f.bash("ls -a .loop/context")
	json.Unmarshal([]byte(readFile(filepath.Join(f.cwd, ".loop", "context", "snapshot.json"))), &snap)
	actions := []any{}
	switch play {
	case OrchInvalid:
		actions = append(actions, map[string]any{"action": "approve", "item_id": 1, "reason": "it looks done"})
	case OrchAgentsUp:
		actions = append(actions, map[string]any{"action": "agents", "active_agents": 6, "reason": "more hands"})
		refused := regexp.MustCompile(`(?m)^- (\w+)( #\d+)?: refused \((\d+) (\w+)\)`).FindAllSubmatch(brief, -1)
		var seen []string
		for _, m := range refused {
			seen = append(seen, fmt.Sprintf("%s%s %s %s", m[1], m[2], m[3], m[4]))
		}
		if len(seen) > 0 {
			actions = append(actions, map[string]any{"action": "note", "text": "refused last turn: " + strings.Join(seen, "; "), "reason": "learning"})
		}
	case OrchNote:
		actions = append(actions, map[string]any{"action": "note", "text": "turn " + turn + ": looked at the board", "reason": "a sweep"})
	case OrchPersona:
		actions = personaActions(turn, []byte(readFile(filepath.Join(f.cwd, ".loop", "context", "snapshot.json"))))
	default: // OrchReact
		stage := map[int64]string{}
		for _, it := range snap.Items {
			stage[it.ID] = it.Stage
		}
		retry := map[int64]bool{}
		var order []int64
		if snap.Turn != nil {
			for _, tr := range snap.Turn.Triggers {
				var ids []int64
				switch tr.Reason {
				case "run_failed":
					ids = []int64{tr.ItemID}
				case "due":
					for _, it := range snap.Items {
						ids = append(ids, it.ID)
					}
				}
				for _, id := range ids {
					if stage[id] == "needs_you" && !retry[id] {
						retry[id] = true
						order = append(order, id)
					}
				}
			}
		}
		for _, id := range order {
			actions = append(actions, map[string]any{"action": "retry", "item_id": id, "reason": "a runner error: retry with the backoff"})
		}
		actions = append(actions, map[string]any{"action": "note", "text": fmt.Sprintf("turn %s: %d item(s) open, %d retried", turn, len(snap.Items), len(order)), "reason": "what I saw"})
	}
	res, _ := json.Marshal(map[string]any{"actions": actions})
	f.write("out/result.json", string(res))
	f.result("Turn done.")
	return 0
}
