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
	"strconv"
	"strings"
	"time"

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
	Steps  []Step   `json:"steps"`
	Final  *Expect  `json:"expect,omitempty"`
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
	// script: how the fake claude plays the item's runs of a stage (default build), and the repo
	// a triage run picks
	For   string `json:"for,omitempty"` // the stage the script is for: triage, build (default), review
	Play  string `json:"play,omitempty"`
	File  string `json:"file,omitempty"`
	Picks string `json:"picks,omitempty"` // the repo a triage run picks
	// runner: which item the claim must take ("none": nothing)
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
	// the workflow runs on main's commits go (name -> pass, fail, pending)
	Checks map[string]string `json:"checks,omitempty"`
	Main   map[string]string `json:"main,omitempty"`
	// push: a commit by someone else to the item's branch, or to Branch ("main") of the demo
	// repo, changing File to Text
	Branch string `json:"branch,omitempty"`
	// github: GitHub refuses the next RefuseMerges merges; MergeByHand merges the item's PR
	// outside the loop
	RefuseMerges *int `json:"refuse_merges,omitempty"`
	MergeByHand  bool `json:"merge_by_hand,omitempty"`
	// control (as the CLI's PUT /control)
	ActiveAgents  *int  `json:"active_agents,omitempty"`
	MaxRunMinutes *int  `json:"max_run_minutes,omitempty"`
	Stopped       *bool `json:"stopped,omitempty"`

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
	SamePrefix []string `json:"same_prefix,omitempty"`
}

// EventMatch selects loopd events.
type EventMatch struct {
	Item    string            `json:"item,omitempty"`
	Kind    string            `json:"kind,omitempty"`
	Actor   string            `json:"actor,omitempty"` // approver, other, runner, hand, user, loopd, or literal
	Outcome string            `json:"outcome,omitempty"`
	Data    map[string]string `json:"data,omitempty"`  // key -> regexp on the value
	Count   *int              `json:"count,omitempty"` // exactly this many (default: at least one)
}

// MsgMatch selects Slack messages.
type MsgMatch struct {
	Thread  string   `json:"thread,omitempty"`  // a reply in this item's thread
	Parent  string   `json:"parent,omitempty"`  // this item's first message
	Channel string   `json:"channel,omitempty"` // loop or digest
	From    string   `json:"from,omitempty"`    // bot, approver, other
	Kind    string   `json:"kind,omitempty"`    // message, ephemeral, command, response
	To      string   `json:"to,omitempty"`      // an ephemeral's reader
	Text    string   `json:"text,omitempty"`    // regexp on the visible text
	Buttons []string `json:"buttons,omitempty"` // labels that must be on it
	Pinned  *bool    `json:"pinned,omitempty"`
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
	ctx   context.Context
}

func (p *player) ref(alias string) (itemRef, error) {
	r, ok := p.items[alias]
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
	p := &player{s: s, sc: sc, items: map[string]itemRef{}, ctx: context.Background()}
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
	for _, kv := range [][2]string{{"item", st.Item}, {"as", st.As}, {"text", st.Text}, {"button", st.Button},
		{"play", st.Play}, {"advance", st.Advance}, {"outcome", st.Outcome}, {"claims", st.Claims}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+strconv.Quote(kv[1]))
		}
	}
	return strings.Join(parts, " ")
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
		return s.hand.Advance(p.ctx, r.id, "reproduced", map[string]string{"area": "sandbox", "size": "S", "risk": "normal", "resources": ""}, nil)
	case "script":
		r, err := p.ref(st.Item)
		if err != nil {
			return err
		}
		return s.SetScript(r.id, st.For, st.Play, st.File, st.Picks)
	case "runner":
		id, err := s.Poll(30 * time.Second)
		if err != nil {
			return err
		}
		switch st.Claims {
		case "":
		case "none":
			if id != 0 {
				return fmt.Errorf("the runner claimed #%d, want nothing", id)
			}
		default:
			r, err := p.ref(st.Claims)
			if err != nil {
				return err
			}
			if id != r.id {
				return fmt.Errorf("the runner claimed #%d, want %s (#%d)", id, st.Claims, r.id)
			}
		}
		return nil
	case "agent_running":
		r, err := p.ref(st.Item)
		if err != nil {
			return err
		}
		return eventually(30*time.Second, func() error {
			for _, x := range s.AgentState(r.id) {
				if x == "hanging" {
					return nil
				}
			}
			return fmt.Errorf("the agent for %s is not running yet (state %v)", st.Item, s.AgentState(r.id))
		})
	case "runner_idle":
		return s.WaitRuns(60 * time.Second)
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
		if st.Main != nil {
			s.GitHub.SetMainCI(st.Main)
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
	case "github":
		if st.RefuseMerges != nil {
			s.GitHub.RefuseMerges(*st.RefuseMerges)
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
		return s.user.do("PUT", "/control", patch, nil)
	case "expect", "wait":
		return p.check(st.Expect)
	}
	return fmt.Errorf("unknown step %q", st.Do)
}

func (p *player) add(st Step) error {
	if st.Item == "" || st.Title == "" {
		return errors.New("add needs item (an alias) and title")
	}
	if _, dup := p.items[st.Item]; dup {
		return fmt.Errorf("item %q added twice", st.Item)
	}
	switch st.Via {
	case "", "cli":
		var it pipeline.Item
		if err := p.s.user.do("POST", "/items", store.NewItem{Title: st.Title, Ask: st.Title, Repo: RepoKey, Priority: st.Priority}, &it); err != nil {
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

// press finds the newest message (in the item's thread, or the control message) with the button
// or menu and acts on it as a person would.
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
		case st.Item != "":
			r, err := p.ref(st.Item)
			if err != nil {
				return err
			}
			thread = p.s.Thread(r.id)
		default:
			return errors.New("press needs item or message: control")
		}
		msgs := p.s.Slack.Messages()
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			if m.User != slackfake.BotUser || !hasElement(m, st.Button) {
				continue
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
