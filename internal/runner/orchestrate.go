package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/guard"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Orchestrate turns (docs/phases/08-orchestrator.md, piece 3;
// docs/decisions/2026-10-17-orchestrate-turns.md): one short agent per turn, in the orchestrator's
// own directory <work_root>/orchestrator/ (so `claude -p --resume` finds the day's session there),
// with loopd's snapshot under .loop/context/ and out/, the only place it writes, emptied before
// every turn. It holds no credential and checks nothing out: the runner executes the actions in
// its out/result.json through loopd with the orchestrator's token, which loopd checks against the
// orchestrator's authority.

// StageOrchestrate is the config stage for orchestrate turns: a runner that names it (and has an
// orchestrator_token) claims them too.
const StageOrchestrate = "orchestrate"

// The actions an orchestrate result may ask for (loopd's /orchestrator routes).
const (
	ActAnswer   = "answer"
	ActRetry    = "retry"
	ActPriority = "priority"
	ActAdd      = "add"
	ActLink     = "link"
	ActEscalate = "escalate"
	ActAgents   = "agents"
	ActNote     = "note"
)

// orchFields are each action's fields besides action and reason (every one needs a reason), and
// which of them are required.
var orchFields = map[string]struct{ allowed, required []string }{
	ActAnswer:   {[]string{"item_id", "kind", "text"}, []string{"item_id", "kind", "text"}},
	ActRetry:    {[]string{"item_id"}, []string{"item_id"}},
	ActPriority: {[]string{"item_id", "priority"}, []string{"item_id", "priority"}},
	ActAdd:      {[]string{"title", "ask", "repo", "priority", "depends_on", "signature"}, []string{"title", "signature"}},
	ActLink:     {[]string{"item_id", "after"}, []string{"item_id", "after"}},
	ActEscalate: {[]string{"item_id", "text", "recommendation"}, []string{"text"}},
	ActAgents:   {[]string{"active_agents"}, []string{"active_agents"}},
	ActNote:     {[]string{"text"}, []string{"text"}},
}

// MaxOrchActions is the most actions one turn may ask for.
const MaxOrchActions = 20

// OrchAction is one action of an orchestrate result.
type OrchAction struct {
	Action         string  `json:"action"`
	Reason         string  `json:"reason"`
	ItemID         int64   `json:"item_id,omitempty"`
	Kind           string  `json:"kind,omitempty"`
	Text           string  `json:"text,omitempty"`
	Priority       string  `json:"priority,omitempty"`
	After          int64   `json:"after,omitempty"`
	Recommendation string  `json:"recommendation,omitempty"`
	Title          string  `json:"title,omitempty"`
	Ask            string  `json:"ask,omitempty"`
	Repo           string  `json:"repo,omitempty"`
	DependsOn      []int64 `json:"depends_on,omitempty"`
	Signature      string  `json:"signature,omitempty"`
	ActiveAgents   *int    `json:"active_agents,omitempty"`
}

// OrchActionResult is how an action went, as loopd records it on the turn.
type OrchActionResult struct {
	Action  string `json:"action"`
	ItemID  int64  `json:"item_id,omitempty"`
	OK      bool   `json:"ok"`
	Status  int    `json:"status,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// ValidateOrchResult checks an orchestrate turn's out/result.json, strictly: {"actions": [...]}
// and nothing else; each action one of the known ones with only its own fields, its required ones
// set, and a reason. loopd checks each action's limits again when the runner sends it.
func ValidateOrchResult(raw []byte) ([]OrchAction, error) {
	var top struct {
		Actions []json.RawMessage `json:"actions"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&top); err != nil {
		return nil, fmt.Errorf("out/result.json: %v", err)
	}
	if top.Actions == nil {
		return nil, fmt.Errorf("out/result.json has no actions list (an empty list is fine)")
	}
	if len(top.Actions) > MaxOrchActions {
		return nil, fmt.Errorf("%d actions, at most %d a turn", len(top.Actions), MaxOrchActions)
	}
	var out []OrchAction
	for i, rawA := range top.Actions {
		n := i + 1
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rawA, &fields); err != nil {
			return nil, fmt.Errorf("action %d: %v", n, err)
		}
		var name string
		if err := json.Unmarshal(fields["action"], &name); err != nil || name == "" {
			return nil, fmt.Errorf("action %d: no action", n)
		}
		spec, ok := orchFields[name]
		if !ok {
			names := make([]string, 0, len(orchFields))
			for k := range orchFields {
				names = append(names, k)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("action %d: unknown action %q (have %s)", n, name, strings.Join(names, ", "))
		}
		for k := range fields {
			if k != "action" && k != "reason" && !slices.Contains(spec.allowed, k) {
				return nil, fmt.Errorf("action %d (%s): unknown field %q (it takes %s and reason)", n, name, k, strings.Join(spec.allowed, ", "))
			}
		}
		for _, k := range append([]string{"reason"}, spec.required...) {
			if v, ok := fields[k]; !ok || string(v) == `""` || string(v) == "null" {
				return nil, fmt.Errorf("action %d (%s): %s is required", n, name, k)
			}
		}
		var a OrchAction
		d := json.NewDecoder(bytes.NewReader(rawA))
		d.DisallowUnknownFields()
		if err := d.Decode(&a); err != nil {
			return nil, fmt.Errorf("action %d (%s): %v", n, name, err)
		}
		a.Reason = strings.TrimSpace(a.Reason)
		if a.Reason == "" {
			return nil, fmt.Errorf("action %d (%s): reason is required", n, name)
		}
		out = append(out, a)
	}
	return out, nil
}

// route is the loopd route and body an action goes to.
func (a OrchAction) route() (path string, body map[string]any) {
	body = map[string]any{"reason": a.Reason}
	item := fmt.Sprintf("/orchestrator/items/%d/", a.ItemID)
	switch a.Action {
	case ActAnswer:
		body["kind"], body["text"] = a.Kind, a.Text
		return item + "answer", body
	case ActRetry:
		return item + "retry", body
	case ActPriority:
		body["priority"] = a.Priority
		return item + "priority", body
	case ActLink:
		body["after"] = a.After
		return item + "link", body
	case ActAdd:
		body["title"], body["ask"], body["repo"], body["priority"], body["signature"] = a.Title, a.Ask, a.Repo, a.Priority, a.Signature
		if len(a.DependsOn) > 0 {
			body["depends_on"] = a.DependsOn
		}
		return "/orchestrator/items", body
	case ActEscalate:
		body["item_id"], body["text"], body["recommendation"] = a.ItemID, a.Text, a.Recommendation
		return "/orchestrator/escalations", body
	case ActAgents:
		body["active_agents"] = a.ActiveAgents
		return "/orchestrator/agents", body
	default: // ActNote
		body["text"] = a.Text
		return "/orchestrator/notes", body
	}
}

// OrchTurn is the orchestrate turn a claim carries (loopd's store.OrchTurn).
type OrchTurn struct {
	ID       int64              `json:"id"`
	Triggers []pipeline.Trigger `json:"triggers"`
	Day      string             `json:"day"`
	Resume   string             `json:"resume"`
}

// OrchTurnResult is what the runner reports for a turn.
type OrchTurnResult struct {
	SessionID string             `json:"session_id"`
	Error     string             `json:"error,omitempty"`
	Actions   []OrchActionResult `json:"actions"`
}

// orchClient is loopd with the orchestrator's token: the actions go through it, never the
// runner's own token.
func (r *Runner) orchClient() *Client {
	return &Client{Base: r.Client.Base, Token: r.Cfg.OrchestratorToken, HTTP: r.Client.HTTP}
}

// OrchStatic is the first prompt of each day's session: who it is, its authority, the result
// contract. A stand-in until the orchestrator's skill (piece 4, skills/orchestrator/) replaces the
// first part.
const OrchStatic = `# The orchestrator

You keep the loop's board moving, as a scrum master would (docs/phases/08-orchestrator.md in
jarvis-loop). Each turn you get loopd's snapshot of the board and the triggers that started the
turn. Unstick what you are allowed to, ask the user only for real decisions (with a
recommendation), and write down what you are watching. You have no tools that change anything but
` + "`out/result.json`" + `: the runner carries out the actions you list there, through loopd, which
refuses anything past your authority (the refusal comes back to you in the next turn's snapshot,
` + "`last_turn.actions`" + `).

**Never** ask for: approving a merge, OK'ing a trusted-file change, dropping or closing an item,
changing the runner's config, the hold rules or a gate, deploying, prod, editing code. Recommend
them to the user in an escalation instead.

## The result contract

Every turn, write ` + "`out/result.json`" + ` as your last step:

` + "```json" + `
{"actions": [
  {"action": "retry", "item_id": 12, "reason": "git index.lock: an environment error, first on this head"},
  {"action": "answer", "item_id": 14, "kind": "harness", "text": "list it under not verified; the runner's verify runs it", "reason": "the standing answer"},
  {"action": "priority", "item_id": 15, "priority": "P2", "reason": "…"},
  {"action": "link", "item_id": 16, "after": 12, "reason": "…"},
  {"action": "add", "title": "…", "ask": "…", "repo": "owner/name", "priority": "P1", "depends_on": [], "signature": "the normalised failure", "reason": "…"},
  {"action": "escalate", "item_id": 17, "text": "what needs the user", "recommendation": "what you would do, and why", "reason": "…"},
  {"action": "agents", "active_agents": 2, "reason": "every run fails the same way"},
  {"action": "note", "text": "what you are watching, tried, suspect", "reason": "…"}
]}
` + "```" + `

Every action needs a reason (it is recorded with the action and shown to the user). Each takes only
its own fields. ` + "`answer`" + `'s kind is one of history, harness, bounce, red_ci, loop. An empty list is
a fine turn. A result with an unknown action or field is refused once with the reason, and you are
asked again.
`

// OrchPrompt is an orchestrate turn's prompt: on a session's first turn of the day OrchStatic,
// then the turn; on later turns only the turn.
func OrchPrompt(t OrchTurn, dir string, resume string, context []TriageFile, contextErrs []string, last []OrchActionResult) string {
	var b strings.Builder
	if resume == "" {
		b.WriteString(OrchStatic)
		b.WriteString(TurnHeading)
	} else {
		b.WriteString(strings.TrimPrefix(TurnHeading, "\n"))
	}
	fmt.Fprintf(&b, "\n- Orchestrate: turn %d\n", t.ID)
	if resume == "" {
		b.WriteString("- Session: new (today's first turn; later turns today continue it)\n")
	} else {
		b.WriteString("- Session: continues today's (the board has changed since: read `.loop/context/` again)\n")
	}
	b.WriteString("- Triggers:\n")
	if len(t.Triggers) == 0 {
		b.WriteString("  - none (a sweep)\n")
	}
	for _, tr := range t.Triggers {
		fmt.Fprintf(&b, "  - %s", tr.Reason)
		if tr.ItemID != 0 {
			fmt.Fprintf(&b, " #%d", tr.ItemID)
		}
		if tr.Text != "" {
			fmt.Fprintf(&b, ": %q", clip(oneLineText(tr.Text), 500))
		}
		b.WriteString("\n")
	}
	if len(context) > 0 {
		b.WriteString("- Context from the runner:")
		for _, f := range context {
			writeFile(&b, f.Name, f.Label)
		}
		b.WriteString("\n")
	}
	for _, e := range contextErrs {
		fmt.Fprintf(&b, "- Not available this turn: %s\n", clip(e, 200))
	}
	fmt.Fprintf(&b, "- Working directory (the orchestrator's, the same every turn): `%s`. Write only `out/result.json` (out/ is empty now) and in `.loop/scratch/`.\n", dir)
	if len(last) > 0 {
		b.WriteString("\nYour last turn's actions:\n")
		for _, a := range last {
			fmt.Fprintf(&b, "- %s", a.Action)
			if a.ItemID != 0 {
				fmt.Fprintf(&b, " #%d", a.ItemID)
			}
			if a.OK {
				b.WriteString(": done\n")
			} else {
				fmt.Fprintf(&b, ": refused (%d %s): %s\n", a.Status, a.Code, clip(oneLineText(a.Message), 300))
			}
		}
	}
	return b.String()
}

// orchestrateTurn runs one claimed orchestrate turn: the agent, then its actions, then the report.
func (r *Runner) orchestrateTurn(ctx context.Context, cl *Claim, agent string) {
	log := r.Log.With("agent", agent)
	cleanupCtx := context.WithoutCancel(ctx)
	if cl.Orch == nil {
		log.Error("orchestrate turn", "err", "loopd's claim has no turn")
		return
	}
	t := *cl.Orch
	log = log.With("orchestrate", t.ID)
	ru := &run{stage: StageOrchestrate, lease: cl.Lease.ID, agent: agent, started: r.Clock.Now(), orch: &t, resume: t.Resume}
	ru.wt = filepath.Join(r.Cfg.WorkRoot, "orchestrator")
	ru.scratch = filepath.Join(ru.wt, ".loop", "scratch")
	ru.dir = uniqueRunDir(filepath.Join(r.Cfg.WorkRoot, "runs", fmt.Sprintf("O%d-orchestrate-%s", t.ID, ru.started.Format("20060102T150405Z"))))
	ru.settings = filepath.Join(r.Cfg.WorkRoot, "settings", "orchestrate.json")
	ru.sandbox = "orchestrator"
	ru.maxRun = time.Duration(r.Cfg.PlanTurnMinutes) * time.Minute
	ru.done = ctx.Done()
	if r.Cfg.OrchestratorToken == "" {
		r.sendOrch(cleanupCtx, ru, OrchTurnResult{Error: "this runner has no orchestrator_token"}, log)
		return
	}
	prompt, err := r.prepareOrch(ctx, ru, "")
	if err != nil {
		log.Error("prepare", "err", err)
		r.sendOrch(cleanupCtx, ru, OrchTurnResult{Error: "the runner could not prepare the turn: " + clip(err.Error(), 1000)}, log)
		return
	}
	log.Info("agent starting", "dir", ru.wt, "resume", ru.resume)
	out := r.runAgent(ctx, ru, prompt)
	log.Info("agent ended", "reason", out.reason, "exit", out.exitCode, "detail", out.detail)
	if r.sessionGone(ru, out) {
		log.Warn("the session to resume is gone; starting a new one", "session", ru.resume)
		ru.resume = ""
		ru.dir = uniqueRunDir(ru.dir)
		if prompt, err = r.prepareOrch(ctx, ru, ""); err != nil {
			r.sendOrch(cleanupCtx, ru, OrchTurnResult{Error: "the runner could not prepare the turn: " + clip(err.Error(), 1000)}, log)
			return
		}
		out = r.runAgent(ctx, ru, prompt)
	}
	actions, why, ok := r.orchResult(ru, out)
	if !ok && why != "" && out.reason == endExited && out.exitCode == 0 && out.stats.sessionID != "" && ctx.Err() == nil {
		// One retry, in the same session, with the reason (docs/decisions/2026-10-16-refused-results.md).
		log.Warn("result refused; asking once more", "why", why)
		keepFirstAttempt(ru.dir, log)
		ru.resume, ru.spent, ru.retried = out.stats.sessionID, r.Clock.Now().Sub(ru.started), true
		if prompt, err = r.prepareOrch(ctx, ru, why); err == nil {
			out = r.runAgent(ctx, ru, prompt)
			actions, why, ok = r.orchResult(ru, out)
		}
	}
	res := OrchTurnResult{SessionID: out.stats.sessionID, Actions: []OrchActionResult{}}
	if !ok {
		if why == "" { // stopped or shutting down: loopd puts the turn back
			return
		}
		res.Error = why
		r.sendOrch(cleanupCtx, ru, res, log)
		return
	}
	res.Actions = r.execOrch(cleanupCtx, actions, log)
	r.sendOrch(cleanupCtx, ru, res, log)
}

// orchResult reads and checks how the agent ended: its actions, or why the turn failed ("" when
// the run was stopped and nothing is to be reported).
func (r *Runner) orchResult(ru *run, out outcome) ([]OrchAction, string, bool) {
	r.flushRefusals(context.Background(), ru)
	switch out.reason {
	case endStopped, endShutdown:
		return nil, "", false
	case endTimeCap:
		return nil, out.detail, false
	case endModeMismatch:
		r.paused.Store(out.detail)
		return nil, "boundary check failed: " + out.detail + "; the runner stopped claiming until it is restarted", false
	case endNotArmed, endStartFailed:
		return nil, "boundary check failed (" + out.reason + "): " + out.detail, false
	}
	if hit := out.stats.limit; hit != nil {
		why := "the agent hit the usage limit"
		if hit.Reset != "" {
			why += " (resets " + hit.Reset + ")"
		}
		return nil, why, false
	}
	raw, err := readResult(filepath.Join(ru.wt, "out", "result.json"))
	if err != nil {
		return nil, fmt.Sprintf("the agent exited (code %d) without a usable out/result.json (%v)", out.exitCode, err), false
	}
	acts, err := ValidateOrchResult(raw)
	if err != nil {
		return nil, err.Error(), false
	}
	return acts, "", true
}

// execOrch sends each action to loopd with the orchestrator's token. A refusal (4xx: too_soon,
// limit, forbidden, ...) is recorded and the next action goes on: loopd keeps the outcomes on the
// turn, and the next turn sees them.
func (r *Runner) execOrch(ctx context.Context, actions []OrchAction, log *slog.Logger) []OrchActionResult {
	c := r.orchClient()
	out := []OrchActionResult{}
	for _, a := range actions {
		path, body := a.route()
		res := OrchActionResult{Action: a.Action, ItemID: a.ItemID, OK: true}
		st, err := c.json(ctx, http.MethodPost, path, body, nil)
		res.Status = st
		var ae *APIError
		switch {
		case errors.As(err, &ae):
			res.OK, res.Status, res.Code, res.Message = false, ae.Status, ae.Code, clip(ae.Msg, 500)
			log.Info("action refused", "action", a.Action, "item", a.ItemID, "status", ae.Status, "code", ae.Code, "msg", ae.Msg)
		case err != nil:
			res.OK, res.Message = false, clip(err.Error(), 500)
			log.Warn("action", "action", a.Action, "item", a.ItemID, "err", err)
		default:
			log.Info("action done", "action", a.Action, "item", a.ItemID)
		}
		out = append(out, res)
	}
	return out
}

func (r *Runner) sendOrch(ctx context.Context, ru *run, res OrchTurnResult, log *slog.Logger) {
	res.Error = clip(strings.TrimSpace(res.Error), 1500)
	if res.Actions == nil {
		res.Actions = []OrchActionResult{}
	}
	if err := r.Client.OrchTurnResult(ctx, ru.orch.ID, res); err != nil {
		log.Error("orchestrate turn result", "err", err)
		return
	}
	log.Info("reported", "actions", len(res.Actions), "error", res.Error)
}

// prepareOrch refreshes the orchestrator's directory for a turn (the snapshot, an empty out/) and
// writes the prompt, the guard config and the settings. refused, on the one retry, is why its
// previous result was refused.
func (r *Runner) prepareOrch(ctx context.Context, ru *run, refused string) (string, error) {
	if refused == "" {
		os.RemoveAll(ru.dir) // a retry keeps the refused attempt's files (keepFirstAttempt)
	}
	if err := os.MkdirAll(ru.dir, 0o700); err != nil {
		return "", err
	}
	out := filepath.Join(ru.wt, "out")
	ctxDir := filepath.Join(ru.wt, ".loop", "context")
	for _, d := range []string{out, ru.scratch, ctxDir} {
		os.RemoveAll(d)
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}
	ri := &RunInfo{}
	var last []OrchActionResult
	snap, err := r.orchClient().Snapshot(ctx)
	if err != nil {
		ri.ContextErrs = append(ri.ContextErrs, "loopd's snapshot: "+err.Error())
	} else {
		var s struct {
			LastTurn *struct {
				Actions []OrchActionResult `json:"actions"`
			} `json:"last_turn"`
		}
		if json.Unmarshal(snap, &s) == nil && s.LastTurn != nil {
			last = s.LastTurn.Actions
		}
		var pretty bytes.Buffer
		if json.Indent(&pretty, snap, "", " ") == nil {
			snap = pretty.Bytes()
		}
		writeContext(ru, ri, ".loop/context/snapshot.json", "loopd's snapshot: the open items, what is working, the runners, the items that triggered this turn in full, your notebook, your open escalations, the user's replies since your last turn", string(snap))
	}
	writeContext(ru, ri, ".loop/context/repos.md", "the configured repos", r.reposTable())
	prompt := OrchPrompt(*ru.orch, ru.wt, ru.resume, ri.Context, ri.ContextErrs, last)
	if refused != "" {
		// The retry resumes the refused attempt's session: the turn's part is already in it.
		prompt = strings.TrimPrefix(TurnHeading, "\n") + fmt.Sprintf("\n- Orchestrate: turn %d, once more\n\n%s\n\n%s\n\nWrite `out/result.json` again, following the result contract.\n",
			ru.orch.ID, RetryHeading, clip(refused, 1500))
	}
	for _, p := range []string{filepath.Join(ru.wt, ".loop", "brief.md"), filepath.Join(ru.dir, "brief.md")} {
		if err := os.WriteFile(p, []byte(prompt), 0o644); err != nil {
			return "", err
		}
	}
	home := Home()
	gc := guard.Config{
		Worktree: out, Scratch: ru.scratch, Home: home,
		Protected: r.Cfg.Protected(), Secrets: r.Cfg.Secrets(), Words: []string{r.Cfg.UserEmulator},
		LoopdAddrs: r.Cfg.LoopdAddrs(), ReadOnly: true,
		Report: filepath.Join(ru.dir, "refusals.jsonl"), Armed: filepath.Join(ru.dir, "armed"),
	}
	b, _ := json.MarshalIndent(gc, "", "  ")
	if err := os.WriteFile(filepath.Join(ru.dir, "guard.json"), b, 0o600); err != nil {
		return "", err
	}
	// The plan turns' settings: read-only, nothing per repo.
	settings, err := GenerateSettings(r.planSettingsInput(home))
	if err != nil {
		return "", err
	}
	if err := writeAtomic(ru.settings, settings); err != nil {
		return "", err
	}
	if _, err := os.Stat(r.Cfg.Guard); err != nil {
		return "", fmt.Errorf("the guard binary %s: %w", r.Cfg.Guard, err)
	}
	argv, _ := json.Marshal(r.agentCommand(ru).Args[1:])
	if err := os.WriteFile(filepath.Join(ru.dir, "argv.json"), argv, 0o600); err != nil {
		return "", err
	}
	return prompt, nil
}
