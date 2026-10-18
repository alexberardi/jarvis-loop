package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/api"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/slack"
	"github.com/alexberardi/jarvis-loop/internal/slackfake"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// Scenario is one scripted walk through the loop and what must (and must not) come of it. See
// "Scenario format" in docs/phases/04b-loop-harness.md.
type Scenario struct {
	Name  string `json:"name"`
	About string `json:"about"`
	// Stages are the stages the sandbox's runner claims (default: triage, build and review). A
	// scenario that does triage or review by hand while the runner builds names ["build"].
	Stages []string `json:"stages,omitempty"`
	// ExtraRepo: the fake GitHub has a fourth repo (sandbox/extra) that loopd and the runner
	// learn of only from a reload_loopd and a reload_runner step.
	ExtraRepo bool `json:"extra_repo,omitempty"`
	// Slots are the runner's agent slots (default 1): two let a run go on while another starts.
	Slots int     `json:"slots,omitempty"`
	Steps []Step  `json:"steps"`
	Final *Expect `json:"expect,omitempty"`
}

// Step is one thing that happens. Do says what; the other fields are its arguments. An "expect"
// (or "wait") step carries the Expect fields inline.
type Step struct {
	Do   string `json:"do"`
	Item string `json:"item,omitempty"` // an item's alias in this scenario

	// add
	Title    string `json:"title,omitempty"`
	Priority string `json:"priority,omitempty"`
	Via      string `json:"via,omitempty"` // "cli" (default) or "slack"
	// After: the aliases of the items it depends on (CLI: depends_on; Slack: "after #1, #2")
	After []string `json:"after,omitempty"`
	// RepoAs: the CLI's --repo as typed (default the demo repo's config key); with Refused, the
	// add must be refused (a regexp on the error) and adds no item
	RepoAs string `json:"repo_as,omitempty"`
	// triage: what the item needs from a runner (its resources, comma-separated); resources: what
	// the user sets them to (loop resources), "none" for none
	Resources string `json:"resources,omitempty"`
	// hand_claim: what the hand runner offers when it claims (comma-separated), and the only
	// resources it takes (only_resources, as a Mac runner's); with Via "runner_listener" it claims
	// on the remote runners' listener (--runner-addr), and Claims "none" says it must get nothing.
	// RunnerStages: the stages the hand runner takes (runner_stages) and claims in (default build).
	// runner_listener: As ("user", "orchestrator", "board", "none" or "hand", the hand runner)
	// reads GET /items on that listener; with Refused it must be refused (a regexp on the error).
	Capabilities string `json:"capabilities,omitempty"`
	Only         string `json:"only,omitempty"`
	RunnerStages string `json:"runner_stages,omitempty"`
	// script: how the fake claude plays the item's runs of a stage (default build), and the repo
	// a triage run picks
	For   string `json:"for,omitempty"` // the stage the script is for: triage, build (default), review
	Play  string `json:"play,omitempty"`
	File  string `json:"file,omitempty"`
	Picks string `json:"picks,omitempty"` // the repo a triage run picks
	// runner: which item the claim must take ("none": nothing, "orchestrator": an orchestrate
	// turn). script with For "orchestrate" sets the orchestrator's play (no item);
	// orchestrator_message is the user's message (Text) to it, about Item when set.
	Claims string `json:"claims,omitempty"`
	// land
	Outcome string `json:"outcome,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Slack: who acts ("approver" or "other"), what they type, which button or menu, which
	// message ("control" for the active-agents message; otherwise the item's thread)
	As      string `json:"as,omitempty"`
	Text    string `json:"text,omitempty"`
	Button  string `json:"button,omitempty"`
	Value   string `json:"value,omitempty"`
	Message string `json:"message,omitempty"`
	Answer  string `json:"answer,omitempty"` // a /loop command's answer must match this
	// clock
	Advance string `json:"advance,omitempty"`
	// ci: the CI checks on the item's PR head (name -> pass, fail, pending, cancel, skip), and how
	// the workflow runs on main's commits go (name -> pass, fail, pending, cancel)
	Checks map[string]string `json:"checks,omitempty"`
	Main   map[string]string `json:"main,omitempty"`
	// Rerun is how main's jobs go when the runner re-runs their failed jobs (name -> pass, fail,
	// pending); a job not named goes as it went the first time.
	Rerun map[string]string `json:"rerun,omitempty"`
	// MainHead: Main is how the runs went on main's current head only (of the demo repo, or
	// Slug), whether merged or pushed; it wins over the runs set for every commit.
	MainHead bool `json:"main_head,omitempty"`
	// push: a commit by someone else to the item's branch, or to Branch ("main") of the demo
	// repo (or of Slug), changing File to Text
	Branch string `json:"branch,omitempty"`
	Slug   string `json:"slug,omitempty"`
	// github: GitHub refuses the next RefuseMerges merges; MergeByHand merges the item's PR
	// outside the loop, CloseByHand closes it
	RefuseMerges *int `json:"refuse_merges,omitempty"`
	MergeByHand  bool `json:"merge_by_hand,omitempty"`
	CloseByHand  bool `json:"close_by_hand,omitempty"`
	// deploy: the runner restarts on a loop build, "base" (jarvis-loop's main when the stack
	// started) or "main" (its main now)
	Build string `json:"build,omitempty"`
	// control (as the CLI's PUT /control)
	ActiveAgents  *int  `json:"active_agents,omitempty"`
	MaxRunMinutes *int  `json:"max_run_minutes,omitempty"`
	Stopped       *bool `json:"stopped,omitempty"`
	// OrchTurnsPerDay is the orchestrator's daily cap of turns.
	OrchTurnsPerDay *int `json:"orchestrator_turns_per_day,omitempty"`

	// plans (docs/phases/07-planning.md), driven through the API: Plan is a plan's alias. plan
	// starts one (title, priority, text); plan_say is the user's message; plan_turn is a fake
	// runner taking the plan's next turn and reporting Text as the reply with Brief, Blueprint,
	// Ready, BackToBrief (Fails: the turn must fail, a regexp on why); plan_approve approves Doc
	// ("brief", "blueprint") at Version (0: the latest; Refused: must be refused, a regexp);
	// plan_drop drops it; plan_items names the items its hand-off filed, in piece order (Pieces).
	// In Slack (#plan): plan_post is a top-level message in #plan from As with Text (Plan, when
	// set, names the plan it must start; without it nothing may start); plan_reply is As's reply
	// in the plan's thread. press with Plan presses a button in the plan's thread.
	Plan        string   `json:"plan,omitempty"`
	Brief       string   `json:"brief,omitempty"`
	Blueprint   string   `json:"blueprint,omitempty"`
	Ready       bool     `json:"ready,omitempty"`
	BackToBrief string   `json:"back_to_brief,omitempty"`
	Fails       string   `json:"fails,omitempty"`
	Doc         string   `json:"doc,omitempty"`
	Version     int      `json:"version,omitempty"`
	Refused     string   `json:"refused,omitempty"`
	Pieces      []string `json:"pieces,omitempty"`

	// reload_runner: the runner's config file is rewritten and reloaded, as on SIGHUP: Slots sets
	// its slots, AddRepo "extra" adds the ExtraRepo remote's repo, Invalid writes a truncated
	// file; with Refused the reload must be refused (a regexp on the error). reload_loopd: Text is
	// loopd's new --repos-file (added to its --repos), read again as on SIGHUP. drain: the runner
	// drains, as on SIGUSR1. release with Item: the item's "wait" build goes on.
	Slots   int    `json:"slots,omitempty"`
	AddRepo string `json:"add_repo,omitempty"`
	Invalid bool   `json:"invalid,omitempty"`

	// orchestrator: the orchestrator's token calls loopd: Action is "METHOD /path" ({item} is the
	// item's id), Body its JSON (a value "{head}" is the item's head sha, "{id}" its id,
	// "{last_channel}" and "{last_ts}" those of the last message sent the @orchestrator app). With As, it acts on a
	// message As just sent the @orchestrator app (Text, Where: see tell_orchestrator), once loopd's
	// Slack layer has recorded it; with Message "last", on the previous one. Refused: it must be
	// refused, a regexp on the error.
	// tell_orchestrator: As sends the @orchestrator app Text, Where: "dm" (default, a DM),
	// "thread" (a mention in Item's thread) or "loop" (a mention at #loop's top level); the step
	// waits until loopd has recorded it.
	Action string         `json:"action,omitempty"`
	Body   map[string]any `json:"body,omitempty"`
	Where  string         `json:"where,omitempty"`

	Expect
}

// Expect is what must hold. Every check is retried until it holds or Within passes (default
// 10 s), because Slack posts and runner reports arrive asynchronously.
type Expect struct {
	Within     string             `json:"within,omitempty"`
	Stage      map[string]string  `json:"stage,omitempty"`    // alias -> stage
	Repo       map[string]string  `json:"repo,omitempty"`     // alias -> the item's repo (exactly; "" for none)
	Question   map[string]string  `json:"question,omitempty"` // alias -> regexp on the open question
	Leased     map[string]bool    `json:"leased,omitempty"`   // alias -> holds a live lease
	Events     []EventMatch       `json:"events,omitempty"`
	NoEvents   []EventMatch       `json:"no_events,omitempty"`
	Messages   []MsgMatch         `json:"messages,omitempty"` // in this order
	NoMessages []MsgMatch         `json:"no_messages,omitempty"`
	Control    *ControlMatch      `json:"control,omitempty"`
	PRs        *int               `json:"prs,omitempty"` // PRs opened on the fake GitHub
	PR         map[string]PRMatch `json:"pr,omitempty"`  // alias -> the item's PR, as the runner opened it
	Remote     *RemoteMatch       `json:"remote,omitempty"`
	Absent     []string           `json:"absent,omitempty"` // paths under the stack dir that must not exist
	// AgentRuns: stage -> how many agent runs the runner started for it (its run dirs); land
	// starts none.
	AgentRuns map[string]int `json:"agent_runs,omitempty"`
	// SamePrefix: for each stage named, the runner ran it at least twice, and every run's prompt
	// starts with that stage's static brief and has the same command line (one cacheable prefix).
	// For a plan persona (product, engineering): every session's first prompt starts with the
	// persona's static brief, a resumed turn's prompt is the turn alone, and the command lines
	// differ only by --resume <session>.
	SamePrefix []string `json:"same_prefix,omitempty"`
	// RunFiles check files in an item's agent run dirs (the brief, the verify output).
	RunFiles []RunMatch `json:"run_files,omitempty"`
	// Board: alias -> the item's (or plan's) card on the web board, read from the board
	// listener with a board token through the ?t= cookie flow.
	Board map[string]BoardMatch `json:"board,omitempty"`
	// BoardOrchestrator: a regexp the board's Orchestrator panel must match ("" when the board
	// has none: "^$").
	BoardOrchestrator string `json:"board_orchestrator,omitempty"`
	// BoardRunners: a regexp the board's runner notices (draining runners) must match, all of
	// them together ("^$" when there are none).
	BoardRunners string `json:"board_runners,omitempty"`
	// Plans: alias -> the plan as loopd has it; DependsOn: item alias -> the aliases of the items
	// it depends on, exactly.
	Plans     map[string]PlanMatch `json:"plans,omitempty"`
	DependsOn map[string][]string  `json:"depends_on,omitempty"`
	// Evidence: files attached to an item (by name), their label and content.
	Evidence []EvidenceMatch `json:"evidence,omitempty"`
}

// EvidenceMatch is an item's evidence file: Name exactly, Label and Text (its content) regexps,
// NoText a regexp the content must not match; Count how many such files (default: at least one).
type EvidenceMatch struct {
	Item   string `json:"item"`
	Name   string `json:"name"`
	Label  string `json:"label,omitempty"`
	Text   string `json:"text,omitempty"`
	NoText string `json:"no_text,omitempty"`
	Count  *int   `json:"count,omitempty"`
}

// BoardMatch is an item's or a plan's card on the board: its column, a regexp on the card's
// HTML, and one on its page (/board/items/{id} or /board/plans/{id}). Working is a regexp on its
// "Working now" card, both in the board's section and in the fragment live.js refreshes it from
// (/board/working), and WorkingNot one it must not match; NotWorking: it has no such card in
// either.
type BoardMatch struct {
	Column     string `json:"column"`
	Card       string `json:"card,omitempty"`
	Page       string `json:"page,omitempty"`
	Working    string `json:"working,omitempty"`
	WorkingNot string `json:"working_not,omitempty"`
	NotWorking bool   `json:"not_working,omitempty"`
}

// RunMatch checks a file in one of an item's run dirs (work/runs/<id>-<stage>-<time>[-n]/), or
// of a plan's turns (Plan, with the persona as Stage: work/runs/P<id>-<persona>-<time>[-n]/).
type RunMatch struct {
	Item  string `json:"item,omitempty"`
	Plan  string `json:"plan,omitempty"`
	Stage string `json:"stage"`
	// Run is which run of the stage, 1 for the first; 0 for the latest.
	Run int `json:"run,omitempty"`
	// File is relative to the run dir (brief.md, setup.log, verify/<step>/output.log); Absent:
	// it must not exist.
	File   string `json:"file"`
	Absent bool   `json:"absent,omitempty"`
	Text   string `json:"text,omitempty"`    // a regexp the file must match
	NoText string `json:"no_text,omitempty"` // a regexp it must not match
}

// EventMatch selects loopd events.
type EventMatch struct {
	Item    string            `json:"item,omitempty"`
	Plan    string            `json:"plan,omitempty"` // a plan's alias
	Kind    string            `json:"kind,omitempty"`
	Actor   string            `json:"actor,omitempty"` // approver, other, runner, hand, user, loopd, or literal
	Outcome string            `json:"outcome,omitempty"`
	Data    map[string]string `json:"data,omitempty"`  // key -> regexp on the value
	Count   *int              `json:"count,omitempty"` // exactly this many (default: at least one)
}

// MsgMatch selects Slack messages.
type MsgMatch struct {
	Thread string `json:"thread,omitempty"` // a reply in this item's thread
	// Plan: a message in this plan's thread in #plan (its parent or a reply).
	Plan string `json:"plan,omitempty"`
	// Reactions, when set, are exactly the bot's reactions on it ([] for none).
	Reactions []string `json:"reactions,omitempty"`
	Parent    string   `json:"parent,omitempty"`  // this item's first message
	Channel   string   `json:"channel,omitempty"` // loop or digest
	From      string   `json:"from,omitempty"`    // bot, approver, other
	Kind      string   `json:"kind,omitempty"`    // message, ephemeral, command, response, modal, submit
	To        string   `json:"to,omitempty"`      // an ephemeral's reader
	Text      string   `json:"text,omitempty"`    // regexp on the visible text
	Buttons   []string `json:"buttons,omitempty"` // labels that must be on it
	Pinned    *bool    `json:"pinned,omitempty"`
}

// PRMatch checks the PR the runner opened for an item (found by its branch).
type PRMatch struct {
	Number int    `json:"number,omitempty"`
	Repo   string `json:"repo,omitempty"`  // the slug, exactly
	Title  string `json:"title,omitempty"` // regexp
	Body   string `json:"body,omitempty"`  // regexp
	Edits  *int   `json:"edits,omitempty"` // how many times the runner updated it
	// Comments is how many comments the runner posted; Comment a regexp on the newest.
	Comments *int   `json:"comments,omitempty"`
	Comment  string `json:"comment,omitempty"`
	// State is OPEN, MERGED or CLOSED; MergeCalls how many times land asked GitHub to merge it.
	State      string `json:"state,omitempty"`
	MergeCalls *int   `json:"merge_calls,omitempty"`
}

// ControlMatch checks the active-agents control.
type ControlMatch struct {
	ActiveAgents *int  `json:"active_agents,omitempty"`
	Stopped      *bool `json:"stopped,omitempty"`
}

// RemoteMatch checks the fake GitHub remote. Main is always checked: it is where it started, or
// where a merge or a scripted push put it, nowhere else.
type RemoteMatch struct {
	// Main: main's head is this item's squash commit (the land script merged it).
	Main string `json:"main,omitempty"`
	// Branches maps an item to a regexp on its branch's head commit subject.
	Branches map[string]string `json:"branches,omitempty"`
	// Files maps an item to the exact files its branch's head commit changed.
	Files map[string][]string `json:"files,omitempty"`
	// Only: no branch but main and the ones named exists.
	Only bool `json:"only,omitempty"`
	// Repo is the scratch repo the branch checks are about (default the demo repo).
	Repo string `json:"repo,omitempty"`
}

// LoadScenario reads a scenario file, refusing unknown fields (a typo would silently check
// nothing).
func LoadScenario(path string) (*Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sc Scenario
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if sc.Name == "" {
		sc.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return &sc, nil
}

// Result is how a scenario went.
type Result struct {
	Name     string   `json:"name"`
	Pass     bool     `json:"pass"`
	Seconds  float64  `json:"seconds"`
	Failures []string `json:"failures,omitempty"`
	Steps    int      `json:"steps"`
	Dir      string   `json:"dir,omitempty"`
}

type itemRef struct {
	id    int64
	title string
}

type player struct {
	s     *Stack
	sc    *Scenario
	items map[string]itemRef
	plans map[string]int64
	ctx   context.Context
	// the messages orchestrator steps act on: the last one ("<channel>/<ts>")
	lastMsg string
}

func (p *player) ref(alias string) (itemRef, error) {
	r, ok := p.items[alias]
	if n, err := strconv.ParseInt(strings.TrimPrefix(alias, "#"), 10, 64); !ok && strings.HasPrefix(alias, "#") && err == nil {
		// An item the loop filed itself (a red main's P1), by its id.
		d, err := p.s.Store.GetItem(p.ctx, n)
		if err != nil {
			return r, fmt.Errorf("item %s: %v", alias, err)
		}
		return itemRef{id: n, title: d.Item.Title}, nil
	}
	if !ok {
		return r, fmt.Errorf("no item %q in this scenario (add it first)", alias)
	}
	return r, nil
}

func userID(as string) (string, error) {
	switch as {
	case "", "approver":
		return slack.Approver, nil
	case "other":
		return Other, nil
	}
	return "", fmt.Errorf("as %q: use approver or other", as)
}

func randSHA() string {
	b := make([]byte, 20)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Play runs a scenario on a fresh stack and checks it. It never panics on a bad scenario: every
// problem is a failure in the result.
func Play(sc *Scenario, s *Stack) Result {
	start := time.Now()
	p := &player{s: s, sc: sc, items: map[string]itemRef{}, plans: map[string]int64{}, ctx: context.Background()}
	res := Result{Name: sc.Name, Steps: len(sc.Steps)}
	fail := func(format string, args ...any) { res.Failures = append(res.Failures, fmt.Sprintf(format, args...)) }
	for i, st := range sc.Steps {
		if err := p.step(st); err != nil {
			fail("step %d (%s): %v", i+1, describeStep(st), err)
			break
		}
		if v := p.mustNot(); len(v) > 0 {
			for _, x := range v {
				fail("after step %d (%s): must not: %s", i+1, describeStep(st), x)
			}
			break
		}
	}
	if len(res.Failures) == 0 && sc.Final != nil {
		if err := p.check(*sc.Final); err != nil {
			fail("final expectations: %v", err)
		}
	}
	if len(res.Failures) == 0 {
		for _, x := range p.mustNot() {
			fail("must not: %s", x)
		}
	}
	res.Pass = len(res.Failures) == 0
	res.Seconds = time.Since(start).Seconds()
	return res
}

func describeStep(st Step) string {
	parts := []string{st.Do}
	for _, kv := range [][2]string{{"item", st.Item}, {"repo_as", st.RepoAs}, {"as", st.As}, {"text", st.Text}, {"button", st.Button},
		{"play", st.Play}, {"advance", st.Advance}, {"outcome", st.Outcome}, {"claims", st.Claims},
		{"plan", st.Plan}, {"doc", st.Doc}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+strconv.Quote(kv[1]))
		}
	}
	return strings.Join(parts, " ")
}

// refusedAs checks err against a step's Refused: nil when it must succeed, a regexp the error
// must match otherwise.
func refusedAs(refused string, err error) error {
	if refused == "" {
		return err
	}
	if err == nil {
		return fmt.Errorf("not refused, want an error matching %q", refused)
	}
	if ok, rerr := matches(refused, err.Error()); rerr != nil {
		return rerr
	} else if !ok {
		return fmt.Errorf("refused with %q, want %q", err, refused)
	}
	return nil
}

func (p *player) step(st Step) error {
	s := p.s
	switch st.Do {
	case "add":
		return p.add(st)
	case "triage":
		r, err := p.ref(st.Item)
		if err != nil {
			return err
		}
		if err := p.handClaim(r.id, "triage", "hand-triager"); err != nil {
			return err
		}
		f := filepath.Join(s.Dir, "repro.txt")
		os.WriteFile(f, []byte("reproduced on main in the sandbox\n"), 0o644)
		if err := s.hand.Upload(p.ctx, r.id, f, "repro.txt", "repro"); err != nil {
			return err
		}
		return s.hand.Advance(p.ctx, r.id, "reproduced", map[string]string{"area": "sandbox", "size": "S", "risk": "normal", "resources": st.Resources}, nil)
	case "hand_claim":
		var caps, only []string
		if st.Capabilities != "" {
			caps = strings.Split(st.Capabilities, ",")
		}
		if st.Only != "" {
			only = strings.Split(st.Only, ",")
		}
		hand := s.hand
		switch st.Via {
		case "":
		case "runner_listener":
			hand = s.handRemote
		default:
			return fmt.Errorf("hand_claim via %q: want runner_listener or nothing", st.Via)
		}
		stages := []string{"build"}
		if st.RunnerStages != "" {
			c := *hand
			c.Stages = strings.Split(st.RunnerStages, ",")
			hand, stages = &c, c.Stages
		}
		cl, err := hand.ClaimOnly(p.ctx, "hand-builder", caps, only, stages)
		if err != nil {
			return err
		}
		if st.Claims == "none" {
			if cl != nil {
				return fmt.Errorf("the hand runner (%s, only %s) claimed #%d, want nothing", st.Capabilities, st.Only, cl.Item.ID)
			}
			return nil
		}
		r, err := p.ref(st.Item)
		if err != nil {
			return err
		}
		if cl == nil || cl.Item.ID != r.id {
			return fmt.Errorf("the hand runner (%s) claimed %+v in build, want #%d", st.Capabilities, cl, r.id)
		}
		return nil
	case "runner_listener":
		return s.runnerListener(st.As, st.Refused)
	case "script":
		if st.For == "orchestrate" {
			b, _ := json.Marshal(map[string]string{"play": st.Play})
			return os.WriteFile(filepath.Join(s.scripts, "orchestrator-orchestrate.json"), b, 0o644)
		}
		if st.Plan != "" {
			id, err := p.planRef(st.Plan)
			if err != nil {
				return err
			}
			return s.PlanScript(id, st.For, st.Play, st.Picks)
		}
		r, err := p.ref(st.Item)
		if err != nil {
			return err
		}
		return s.SetScript(r.id, st.For, st.Play, st.File, st.Picks)
	case "runner":
		c, err := s.Poll(30 * time.Second)
		if err != nil {
			return err
		}
		got := "nothing"
		switch {
		case c.Item != 0:
			got = fmt.Sprintf("#%d", c.Item)
		case c.Plan != 0:
			got = fmt.Sprintf("a turn of P%d", c.Plan)
		case c.Orch != 0:
			got = fmt.Sprintf("orchestrate turn %d", c.Orch)
		}
		switch st.Claims {
		case "":
		case "none":
			if got != "nothing" {
				return fmt.Errorf("the runner claimed %s, want nothing", got)
			}
		case "orchestrator":
			if c.Orch == 0 {
				return fmt.Errorf("the runner claimed %s, want an orchestrate turn", got)
			}
		default:
			if id, ok := p.plans[st.Claims]; ok {
				if c.Plan != id {
					return fmt.Errorf("the runner claimed %s, want a turn of plan %s (P%d)", got, st.Claims, id)
				}
				return nil
			}
			r, err := p.ref(st.Claims)
			if err != nil {
				return err
			}
			if c.Item != r.id {
				return fmt.Errorf("the runner claimed %s, want %s (#%d)", got, st.Claims, r.id)
			}
		}
		return nil
	case "deploy":
		return s.Deploy(st.Build)
	case "release":
		if st.Item != "" {
			r, err := p.ref(st.Item)
			if err != nil {
				return err
			}
			return s.Release(r.id)
		}
		id, err := p.planRef(st.Plan)
		if err != nil {
			return err
		}
		return s.ReleasePlan(id)
	case "agent_running":
		if st.Plan != "" {
			id, err := p.planRef(st.Plan)
			if err != nil {
				return err
			}
			return eventually(30*time.Second, func() error {
				if slices.Contains(s.PlanAgentState(id), "waiting") {
					return nil
				}
				return fmt.Errorf("the agent for plan %s is not waiting yet (state %v)", st.Plan, s.PlanAgentState(id))
			})
		}
		r, err := p.ref(st.Item)
		if err != nil {
			return err
		}
		return eventually(30*time.Second, func() error {
			for _, x := range s.AgentState(r.id) {
				if x == "hanging" || x == "waiting" {
					return nil
				}
			}
			return fmt.Errorf("the agent for %s is not running yet (state %v)", st.Item, s.AgentState(r.id))
		})
	case "runner_idle":
		return s.WaitRuns(60 * time.Second)
	case "reload_runner":
		return refusedAs(st.Refused, s.ReloadRunner(st.Slots, st.AddRepo, st.Invalid))
	case "reload_loopd":
		return refusedAs(st.Refused, s.ReloadLoopd(st.Text))
	case "drain":
		return s.Runner.Drain(s.ctx)
	case "review":
		r, err := p.ref(st.Item)
		if err != nil {
			return err
		}
		if err := p.handClaim(r.id, "review", "hand-reviewer"); err != nil {
			return err
		}
		d, _ := s.Store.GetItem(p.ctx, r.id)
		return s.hand.Advance(p.ctx, r.id, "approved", map[string]string{"summary": "The tests cover the change and its failure cases.",
			"head_sha": d.Item.HeadSHA}, nil)
	case "land":
		return p.land(st)
	case "ci":
		switch {
		case st.Main != nil && st.MainHead:
			slug := RepoSlug
			if st.Slug != "" {
				slug = st.Slug
			}
			s.GitHub.SetMainCIAt(slug, s.GitHub.MainHead(slug), st.Main)
		case st.Main != nil:
			s.GitHub.SetMainCI(st.Main)
		}
		if st.Rerun != nil {
			s.GitHub.SetMainRerun(st.Rerun)
		}
		if st.Item == "" {
			return nil
		}
		r, err := p.ref(st.Item)
		if err != nil {
			return err
		}
		d, err := s.Store.GetItem(p.ctx, r.id)
		if err != nil {
			return err
		}
		head := s.GitHub.RemoteHead(d.Item.Repo, ItemBranch(r.id, r.title))
		if head == "" {
			return fmt.Errorf("%s has no branch on %q yet", st.Item, d.Item.Repo)
		}
		s.GitHub.SetChecks(d.Item.Repo, head, st.Checks)
		return nil
	case "verify":
		// How the stand-in node-mobile's "mobile shots" verify step goes from now on.
		f, m := filepath.Join(s.Dir, VerifyFailFile), filepath.Join(s.Dir, VerifyMaskedMarker)
		for _, p := range []string{f, m} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		switch st.Outcome {
		case "fail", "masked":
			if st.Outcome == "masked" {
				f = m
			}
			if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
				return err
			}
			return os.WriteFile(f, []byte(st.Outcome+"\n"), 0o644)
		case "pass":
			return nil
		}
		return fmt.Errorf("verify outcome %q: use pass, fail or masked", st.Outcome)
	case "github":
		if st.RefuseMerges != nil {
			s.GitHub.RefuseMerges(*st.RefuseMerges)
		}
		if st.CloseByHand {
			r, err := p.ref(st.Item)
			if err != nil {
				return err
			}
			d, err := s.Store.GetItem(p.ctx, r.id)
			if err != nil {
				return err
			}
			return s.GitHub.CloseByHand(d.Item.Repo, ItemBranch(r.id, r.title))
		}
		if st.MergeByHand {
			r, err := p.ref(st.Item)
			if err != nil {
				return err
			}
			d, err := s.Store.GetItem(p.ctx, r.id)
			if err != nil {
				return err
			}
			return s.GitHub.MergeByHand(d.Item.Repo, ItemBranch(r.id, r.title))
		}
		return nil
	case "push":
		if st.File == "" {
			return errors.New("push needs file (and text)")
		}
		slug, branch := RepoSlug, st.Branch
		if st.Slug != "" {
			slug = st.Slug
		}
		if st.Item != "" {
			r, err := p.ref(st.Item)
			if err != nil {
				return err
			}
			d, err := s.Store.GetItem(p.ctx, r.id)
			if err != nil {
				return err
			}
			slug, branch = d.Item.Repo, ItemBranch(r.id, r.title)
		}
		if branch == "" {
			return errors.New("push needs item or branch")
		}
		_, err := s.GitHub.Push(slug, branch, st.File, st.Text, "someone else's commit")
		return err
	case "reply", "mention":
		user, err := userID(st.As)
		if err != nil {
			return err
		}
		r, err := p.ref(st.Item)
		if err != nil {
			return err
		}
		var ts string
		if err := eventually(10*time.Second, func() error {
			if ts = s.Thread(r.id); ts == "" {
				return fmt.Errorf("%s has no thread yet", st.Item)
			}
			return nil
		}); err != nil {
			return err
		}
		if st.Do == "mention" {
			_, err = s.Slack.Mention(user, slack.ChannelLoop, ts, st.Text)
		} else {
			_, err = s.Slack.Reply(user, slack.ChannelLoop, ts, st.Text)
		}
		return err
	case "press", "choose":
		return p.press(st)
	case "submit":
		// As submits the modal their last press opened (the Rebuild form), with Text as its note
		// when set (else the note it opened with).
		user, err := userID(st.As)
		if err != nil {
			return err
		}
		var values map[string]string
		if st.Text != "" {
			values = map[string]string{"note": st.Text}
		}
		within := 10 * time.Second
		if st.Within != "" {
			if within, err = time.ParseDuration(st.Within); err != nil {
				return err
			}
		}
		// The bot opens the modal after it acked the press: wait for it, not for anything else.
		var serr error
		eventually(within, func() error {
			if serr = s.Slack.Submit(user, values); errors.Is(serr, slackfake.ErrNoView) {
				return serr
			}
			return nil
		})
		return serr
	case "command":
		user, err := userID(st.As)
		if err != nil {
			return err
		}
		ans, err := s.Slack.Command(user, slack.ChannelLoop, "/loop", st.Text)
		if err != nil {
			return err
		}
		if st.Answer != "" {
			re, err := regexp.Compile(st.Answer)
			if err != nil {
				return err
			}
			if !re.MatchString(ans) {
				return fmt.Errorf("/loop %s answered %q, want /%s/", st.Text, ans, st.Answer)
			}
		}
		return nil
	case "clock":
		d, err := time.ParseDuration(st.Advance)
		if err != nil {
			return err
		}
		return s.Advance(d)
	case "resources":
		// The user's loop resources: replace what the item needs from a runner.
		r, err := p.ref(st.Item)
		if err != nil {
			return err
		}
		return s.user.do("POST", fmt.Sprintf("/items/%d/resources", r.id), map[string]any{"resources": strings.Split(st.Resources, ",")}, nil)
	case "control":
		patch := map[string]any{}
		if st.ActiveAgents != nil {
			patch["active_agents"] = *st.ActiveAgents
		}
		if st.MaxRunMinutes != nil {
			patch["max_run_minutes"] = *st.MaxRunMinutes
		}
		if st.Stopped != nil {
			patch["stopped"] = *st.Stopped
		}
		if st.OrchTurnsPerDay != nil {
			patch["orchestrator_turns_per_day"] = *st.OrchTurnsPerDay
		}
		return s.user.do("PUT", "/control", patch, nil)
	case "unfreeze":
		slug := RepoSlug
		if st.Slug != "" {
			slug = st.Slug
		}
		return s.user.do("POST", "/unfreeze", map[string]string{"repo": slug}, nil)
	case "expect", "wait":
		return p.check(st.Expect)
	case "orchestrator_message":
		// The user's message to the orchestrator queued directly; tell_orchestrator sends it through Slack.
		var ids []int64
		if st.Item != "" {
			r, err := p.ref(st.Item)
			if err != nil {
				return err
			}
			ids = append(ids, r.id)
		}
		return s.Store.QueueOrchestrate(p.ctx, pipeline.TriggerMessage, st.Text, ids...)
	case "plan", "plan_say", "plan_turn", "plan_approve", "plan_drop", "plan_items":
		return p.planStep(st)
	case "plan_post", "plan_reply":
		return p.planSlack(st)
	case "orchestrator":
		return p.orchestrate(st)
	case "tell_orchestrator":
		_, err := p.tellOrchestrator(st)
		return err
	}
	return fmt.Errorf("unknown step %q", st.Do)
}

func (p *player) orchestrate(st Step) error {
	method, path, ok := strings.Cut(st.Action, " ")
	if !ok {
		return fmt.Errorf("orchestrator: action %q must be \"METHOD /path\"", st.Action)
	}
	body := map[string]any{}
	for k, v := range st.Body {
		body[k] = v
	}
	if st.Item != "" {
		r, err := p.ref(st.Item)
		if err != nil {
			return err
		}
		path = strings.ReplaceAll(path, "{item}", strconv.FormatInt(r.id, 10))
		d, err := p.s.Store.GetItem(p.ctx, r.id)
		if err != nil {
			return err
		}
		for k, v := range body {
			switch v {
			case "{head}":
				body[k] = d.Item.HeadSHA
			case "{id}":
				body[k] = r.id
			}
		}
	}
	// The message the last step sent the @orchestrator app, for a reply in its thread.
	channel, ts, _ := strings.Cut(p.lastMsg, "/")
	for k, v := range body {
		switch v {
		case "{last_channel}":
			body[k] = channel
		case "{last_ts}":
			body[k] = ts
		}
	}
	var hdr map[string]string
	if st.As != "" {
		if st.Message != "last" {
			if _, err := p.tellOrchestrator(st); err != nil {
				return err
			}
		}
		hdr = map[string]string{api.OnBehalfHeader: p.lastMsg}
	}
	err := p.s.orch.doHeader(method, path, hdr, body, nil)
	if st.Refused == "" {
		return err
	}
	if err == nil {
		return fmt.Errorf("orchestrator %s: not refused, want /%s/", st.Action, st.Refused)
	}
	re, rerr := regexp.Compile(st.Refused)
	if rerr != nil {
		return rerr
	}
	if !re.MatchString(err.Error()) {
		return fmt.Errorf("orchestrator %s: refused with %q, want /%s/", st.Action, err, st.Refused)
	}
	return nil
}

// tellOrchestrator sends the @orchestrator app a message through the fake Slack and waits until
// loopd's Slack layer has recorded it; p.lastMsg is then "<channel>/<ts>".
func (p *player) tellOrchestrator(st Step) (string, error) {
	user, err := userID(st.As)
	if err != nil {
		return "", err
	}
	var channel, ts string
	switch st.Where {
	case "", "dm":
		channel = slackfake.DMChannel(OrchApp, user)
		ts, err = p.s.Slack.DM(OrchApp, user, "", st.Text)
	case "loop":
		channel = slack.ChannelLoop
		ts, err = p.s.Slack.MentionApp(OrchApp, user, channel, "", st.Text)
	case "thread":
		r, rerr := p.ref(st.Item)
		if rerr != nil {
			return "", rerr
		}
		thread := p.s.Thread(r.id)
		if thread == "" {
			return "", fmt.Errorf("%s has no thread yet", st.Item)
		}
		channel = slack.ChannelLoop
		ts, err = p.s.Slack.MentionApp(OrchApp, user, channel, thread, st.Text)
	default:
		return "", fmt.Errorf("where %q: use dm, loop or thread", st.Where)
	}
	if err != nil {
		return "", err
	}
	err = eventually(10*time.Second, func() error {
		msgs, err := p.s.Store.OrchMessages(p.ctx, 0, 0)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.Data["channel"] == channel && m.Data["ts"] == ts {
				return nil
			}
		}
		return errors.New("loopd has not recorded the message to the orchestrator")
	})
	p.lastMsg = channel + "/" + ts
	return ts, err
}

func (p *player) add(st Step) error {
	if st.Item == "" || st.Title == "" {
		return errors.New("add needs item (an alias) and title")
	}
	if _, dup := p.items[st.Item]; dup {
		return fmt.Errorf("item %q added twice", st.Item)
	}
	var after []int64
	var refs []string
	for _, a := range st.After {
		r, err := p.ref(a)
		if err != nil {
			return err
		}
		after = append(after, r.id)
		refs = append(refs, fmt.Sprintf("#%d", r.id))
	}
	switch st.Via {
	case "", "cli":
		repo := RepoKey
		if st.RepoAs != "" {
			repo = st.RepoAs
		}
		var it pipeline.Item
		err := p.s.user.do("POST", "/items", store.NewItem{Title: st.Title, Ask: st.Title, Repo: repo, Priority: st.Priority, DependsOn: after}, &it)
		if st.Refused != "" {
			if err == nil {
				return fmt.Errorf("add --repo %s: not refused, want /%s/", repo, st.Refused)
			}
			if re, rerr := regexp.Compile(st.Refused); rerr != nil || !re.MatchString(err.Error()) {
				return fmt.Errorf("add --repo %s: refused with %q, want /%s/", repo, err, st.Refused)
			}
			return nil
		}
		if err != nil {
			return err
		}
		p.items[st.Item] = itemRef{id: it.ID, title: it.Title}
	case "slack":
		user, err := userID(st.As)
		if err != nil {
			return err
		}
		text := "add " + st.Title
		if st.Text != "" {
			text = "add " + st.Text // the words as typed (repo and priority words); title is the alias's note
		}
		if len(refs) > 0 {
			text += " after " + strings.Join(refs, ", ")
		}
		ans, err := p.s.Slack.Command(user, slack.ChannelLoop, "/loop", text)
		if err != nil {
			return err
		}
		m := regexp.MustCompile(`Added #(\d+)`).FindStringSubmatch(ans)
		if m == nil {
			return fmt.Errorf("/loop %s answered %q", text, ans)
		}
		id, _ := strconv.ParseInt(m[1], 10, 64)
		d, err := p.s.Store.GetItem(p.ctx, id)
		if err != nil {
			return err
		}
		p.items[st.Item] = itemRef{id: id, title: d.Item.Title}
	default:
		return fmt.Errorf("via %q: use cli or slack", st.Via)
	}
	return nil
}

// handClaim claims an item's stage by hand, as `loop claim` does for the stages no runner works
// yet; it must get exactly that item.
func (p *player) handClaim(id int64, stage, agent string) error {
	cl, err := p.s.hand.Claim(p.ctx, agent, nil, []string{stage})
	if err != nil {
		return err
	}
	if cl == nil {
		return fmt.Errorf("nothing to claim in %s", stage)
	}
	if cl.Item.ID != id {
		return fmt.Errorf("claimed #%d in %s, want #%d", cl.Item.ID, stage, id)
	}
	return nil
}

// land finishes the land stage by hand with the PR's changed paths, as the landing script will.
func (p *player) land(st Step) error {
	r, err := p.ref(st.Item)
	if err != nil {
		return err
	}
	if err := p.handClaim(r.id, "land", "hand-lander"); err != nil {
		return err
	}
	d, err := p.s.Store.GetItem(p.ctx, r.id)
	if err != nil {
		return err
	}
	var paths []string
	for _, e := range d.Events {
		if e.Outcome == string(pipeline.PROpened) {
			paths = nil
			if list, ok := e.Data["changed_paths"].([]any); ok {
				for _, x := range list {
					paths = append(paths, fmt.Sprint(x))
				}
			}
		}
	}
	fields := map[string]string{"head_sha": d.Item.HeadSHA}
	switch st.Outcome {
	case "hold":
		fields["reason"] = st.Reason
		if fields["reason"] == "" {
			fields["reason"] = "on the hold list"
		}
	case "merged":
		fields["merge_sha"] = randSHA()
	case "back_to_build":
		fields = map[string]string{"reason": st.Reason}
	default:
		return fmt.Errorf("land outcome %q: use hold, merged or back_to_build", st.Outcome)
	}
	return p.s.hand.Advance(p.ctx, r.id, st.Outcome, fields, paths)
}

func hasElement(m slackfake.Message, actionID string) bool {
	for _, b := range m.Blocks {
		if acc, ok := b["accessory"].(map[string]any); ok && acc["action_id"] == actionID {
			return true
		}
		if list, ok := b["elements"].([]any); ok {
			for _, x := range list {
				if el, ok := x.(map[string]any); ok && el["action_id"] == actionID {
					return true
				}
			}
		}
	}
	return false
}

// press finds the newest message (in the item's or plan's thread, or the control message) with the
// button or menu, and whose body matches Text when that is set, and acts on it as a person would.
func (p *player) press(st Step) error {
	user, err := userID(st.As)
	if err != nil {
		return err
	}
	if st.Button == "" {
		return errors.New("press needs button (an action_id)")
	}
	within := 10 * time.Second
	if st.Within != "" {
		if within, err = time.ParseDuration(st.Within); err != nil {
			return err
		}
	}
	var target slackfake.Message
	err = eventually(within, func() error {
		var thread string
		switch {
		case st.Message == "control":
			thread = ""
		case st.Plan != "":
			id, err := p.planRef(st.Plan)
			if err != nil {
				return err
			}
			if thread = p.s.PlanThread(id); thread == "" {
				return fmt.Errorf("plan %s has no thread yet", st.Plan)
			}
		case st.Item != "":
			r, err := p.ref(st.Item)
			if err != nil {
				return err
			}
			thread = p.s.Thread(r.id)
		default:
			return errors.New("press needs item, plan or message: control")
		}
		msgs := p.s.Slack.Messages()
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			if m.User != slackfake.BotUser && m.User != OrchUser || !hasElement(m, st.Button) {
				continue
			}
			if st.Text != "" {
				if hit, err := matches(st.Text, Body(m)); err != nil {
					return err
				} else if !hit {
					continue
				}
			}
			if st.Message == "control" && m.TS == p.s.ControlTS() || thread != "" && (m.TS == thread || m.ThreadTS == thread) {
				target = m
				return nil
			}
		}
		return fmt.Errorf("no message with %s to press", st.Button)
	})
	if err != nil {
		return err
	}
	if st.Do == "choose" {
		return p.s.Slack.Choose(user, target.Channel, target.TS, st.Button, st.Value)
	}
	return p.s.Slack.Press(user, target.Channel, target.TS, st.Button)
}

// eventually retries fn until it succeeds or the time is up, and returns its last error.
func eventually(within time.Duration, fn func() error) error {
	deadline := time.Now().Add(within)
	for {
		err := fn()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}
