package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alexberardi/jarvis-loop/internal/guard"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Plan turns (docs/phases/07-planning.md, "Runner: conversational turns"): one short agent per
// turn, in the plan's own directory <work_root>/plan-<id>/, so `claude -p --resume` finds the
// persona's session there. The directory holds read-only checkouts of the plan's repos at their
// base under repos/<key>/, the runner's context under .loop/context/, and out/, the only place
// the agent writes (with .loop/scratch/), emptied before every turn.

// StagePlan is the config stage for plan turns: a runner that names it claims them too.
const StagePlan = "plan"

// DefaultPlanTurnMinutes is plan_turn_minutes when the config leaves it out.
const DefaultPlanTurnMinutes = 15

// Personas are the agents that take a plan's turns.
var Personas = []string{pipeline.PersonaProduct, pipeline.PersonaEngineering}

// TurnHeading starts the per-turn part of a plan prompt. A persona's first prompt is its static
// brief (the skill and the result contract, the same for every plan) and then this part; a
// resumed session's prompt is this part alone.
const TurnHeading = "\n## This turn\n"

// MaxPlanDocBytes caps a brief or a blueprint the agent writes.
const MaxPlanDocBytes = 256 << 10

// PlanTurnInfo is what a plan turn's prompt says about the plan and the turn.
type PlanTurnInfo struct {
	Plan pipeline.Plan
	Turn pipeline.PlanTurn
	// Dir is the plan's directory (the agent's working directory).
	Dir string
	// Resume is the session the turn continues ("": the persona's first turn, a new session).
	Resume    string
	Checkouts []Checkout
	// Context are the files the runner wrote under .loop/context/; ContextErrs what it could not
	// get.
	Context     []TriageFile
	ContextErrs []string
}

// PlanPrompt is a plan turn's prompt: on a persona's first turn its static brief, then the turn;
// on later turns only the turn, since the resumed session already has the rest.
func PlanPrompt(in PlanTurnInfo) string {
	var b strings.Builder
	persona := in.Turn.Persona
	if in.Resume == "" {
		b.WriteString(StaticBrief(persona))
		b.WriteString(TurnHeading)
	} else {
		b.WriteString(strings.TrimPrefix(TurnHeading, "\n"))
	}
	p := in.Plan
	fmt.Fprintf(&b, "\n- Plan: P%d: %s\n- Persona: %s\n- Priority: %s\n", p.ID, oneLine(p.Title), persona, orDash(p.Priority))
	if in.Resume == "" {
		fmt.Fprintf(&b, "- Turn: %d, your first in this plan (a new session; later turns continue it)\n", in.Turn.ID)
	} else {
		fmt.Fprintf(&b, "- Turn: %d (your session continues: you have the conversation so far; the documents may have changed since, so read `.loop/context/` again)\n", in.Turn.ID)
	}
	switch {
	case len(in.Checkouts) > 0:
		b.WriteString("- Repos (read-only, detached at the base branch, refreshed for this turn):")
		for _, c := range in.Checkouts {
			fmt.Fprintf(&b, " `%s/` (`%s`, `origin/%s`);", c.Dir, c.Slug, c.Base)
		}
		b.WriteString("\n")
	case len(p.Repos) == 0:
		b.WriteString("- Repos: none yet (pick them from `.loop/context/repos.md` when the idea makes them clear)\n")
	default:
		b.WriteString("- Repos: none checked out\n")
	}
	if len(in.Context) > 0 {
		b.WriteString("- Context from the runner:")
		for _, f := range in.Context {
			writeFile(&b, f.Name, f.Label)
		}
		b.WriteString("\n")
	}
	for _, e := range in.ContextErrs {
		fmt.Fprintf(&b, "- Not available this turn: %s\n", clip(e, 200))
	}
	fmt.Fprintf(&b, "- Working directory (the plan's, the same every turn): `%s`. Write only in `out/` (empty now: this turn's documents and `out/result.json`) and `.loop/scratch/`.\n", in.Dir)
	if why := strings.TrimSpace(in.Turn.BackToBrief); why != "" {
		fmt.Fprintf(&b, "\n**Engineering sent the plan back to the brief:**\n\n> %s\n", strings.ReplaceAll(why, "\n", "\n> "))
	}
	if len(in.Turn.Messages) == 0 {
		b.WriteString("\nNo new messages from the user.")
		if persona == pipeline.PersonaEngineering && in.Resume == "" {
			b.WriteString(" The brief was approved: start the blueprint.")
		}
		b.WriteString("\n")
		return b.String()
	}
	b.WriteString("\nThe user's messages since your last turn, verbatim:\n")
	for _, m := range in.Turn.Messages {
		fmt.Fprintf(&b, "\n> %s\n", strings.ReplaceAll(m, "\n", "\n> "))
	}
	return b.String()
}

// planTurn runs one claimed plan turn and reports its result.
func (r *Runner) planTurn(ctx context.Context, cl *Claim, agent string) {
	log := r.Log.With("agent", agent)
	ru, err := r.newPlanRun(cl, agent)
	if err != nil {
		log.Error("plan turn", "err", err)
		if ru != nil {
			r.postTurn(context.WithoutCancel(ctx), ru, outcome{}, err.Error(), nil)
		}
		return
	}
	log = log.With("plan", ru.plan.ID, "turn", ru.turn.ID, "persona", ru.stage)
	ru.done = ctx.Done()
	cleanupCtx := context.WithoutCancel(ctx)
	prompt, err := r.preparePlan(ctx, ru)
	if err != nil {
		log.Error("prepare", "err", err)
		r.postTurn(cleanupCtx, ru, outcome{}, "the runner could not prepare the turn: "+clip(err.Error(), 1000), nil)
		return
	}
	log.Info("agent starting", "dir", ru.wt, "resume", ru.resume)
	out := r.runAgent(ctx, ru, prompt)
	log.Info("agent ended", "reason", out.reason, "exit", out.exitCode, "detail", out.detail)
	if r.sessionGone(ru, out) {
		// claude no longer has the session (its store was cleared, or the directory moved): start
		// a new one, with the persona's whole brief, rather than fail every later turn.
		log.Warn("the session to resume is gone; starting a new one", "session", ru.resume)
		ru.resume = ""
		ru.dir = uniqueRunDir(ru.dir)
		if prompt, err = r.preparePlan(ctx, ru); err != nil {
			r.postTurn(cleanupCtx, ru, outcome{}, "the runner could not prepare the turn: "+clip(err.Error(), 1000), log)
			return
		}
		out = r.runAgent(ctx, ru, prompt)
		log.Info("agent ended", "reason", out.reason, "exit", out.exitCode, "detail", out.detail)
	}
	r.reportPlan(cleanupCtx, ru, out, log)
}

// sessionGone says a resumed turn ended before its session started because claude has no such
// session ("No conversation found with session ID: ...").
func (r *Runner) sessionGone(ru *run, out outcome) bool {
	if ru.resume == "" || out.reason != endExited || out.exitCode == 0 || out.stats.sessionID != "" {
		return false
	}
	b, _ := os.ReadFile(filepath.Join(ru.dir, "stderr.log"))
	return bytes.Contains(b, []byte("No conversation found"))
}

// newPlanRun works out where a plan turn happens. A claim it cannot run still gets a run (with
// the plan and the turn) when it has them, so the turn can be failed.
func (r *Runner) newPlanRun(cl *Claim, agent string) (*run, error) {
	if cl.Plan == nil || cl.Turn == nil {
		return nil, fmt.Errorf("loopd's plan claim has no plan or turn")
	}
	p, t := *cl.Plan, *cl.Turn
	ru := &run{stage: t.Persona, lease: cl.Lease.ID, agent: agent, cfg: r.Config(), started: r.Clock.Now(), plan: &p, turn: &t}
	if !slices.Contains(Personas, t.Persona) {
		return ru, fmt.Errorf("turn %d of plan P%d is for %q, not a persona this runner knows", t.ID, p.ID, t.Persona)
	}
	ru.wt = filepath.Join(ru.cfg.WorkRoot, fmt.Sprintf("plan-%d", p.ID))
	ru.scratch = filepath.Join(ru.wt, ".loop", "scratch")
	ru.dir = uniqueRunDir(filepath.Join(ru.cfg.WorkRoot, "runs", fmt.Sprintf("P%d-%s-%s", p.ID, t.Persona, ru.started.Format("20060102T150405Z"))))
	// One settings file per persona, the same bytes for every plan.
	ru.settings = filepath.Join(ru.cfg.WorkRoot, "settings", "plan-"+t.Persona+".json")
	ru.sandbox = fmt.Sprintf("plan-%d", p.ID)
	ru.resume = p.ProductSession
	if t.Persona == pipeline.PersonaEngineering {
		ru.resume = p.EngineeringSession
	}
	ru.maxRun = time.Duration(ru.cfg.PlanTurnMinutes) * time.Minute
	return ru, nil
}

// uniqueRunDir is base, or base-2, base-3, ... when two runs start within the same second.
func uniqueRunDir(base string) string {
	dir := base
	for n := 2; ; n++ {
		if _, err := os.Lstat(dir); os.IsNotExist(err) {
			return dir
		}
		dir = fmt.Sprintf("%s-%d", base, n)
	}
}

// preparePlan refreshes the plan's directory for a turn (the checkouts at their base, the
// context, an empty out/) and writes the prompt, the guard config and the settings.
func (r *Runner) preparePlan(ctx context.Context, ru *run) (string, error) {
	os.RemoveAll(ru.dir)
	if err := os.MkdirAll(ru.dir, 0o700); err != nil {
		return "", err
	}
	out := filepath.Join(ru.wt, "out")
	ctxDir := filepath.Join(ru.wt, ".loop", "context")
	for _, d := range []string{out, ru.scratch, ctxDir} {
		removeTree(d)
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}
	info := PlanTurnInfo{Plan: *ru.plan, Turn: *ru.turn, Dir: ru.wt, Resume: ru.resume}
	if err := r.planCheckouts(ctx, ru, &info); err != nil {
		return "", err
	}
	ri := &RunInfo{}
	writeContext(ru, ri, ".loop/context/repos.md", "the configured repos", r.reposTable())
	kinds := []string{pipeline.DocBrief, pipeline.DocBlueprint}
	if ru.turn.Persona == pipeline.PersonaEngineering {
		kinds = append(kinds, pipeline.DocCritique) // Gemini's last critique, and its answers
	}
	for _, kind := range kinds {
		d, err := r.Client.PlanDoc(ctx, ru.plan.ID, kind)
		switch {
		case err != nil:
			ri.ContextErrs = append(ri.ContextErrs, "the plan's "+kind+": "+err.Error())
		case d != nil:
			label := fmt.Sprintf("the latest %s, v%d", kind, d.Version)
			if (kind == pipeline.DocBrief && ru.plan.BriefApproved == d.Version) || (kind == pipeline.DocBlueprint && ru.plan.BlueprintApproved == d.Version) {
				label += ", approved"
			}
			writeContext(ru, ri, ".loop/context/"+kind+".md", label, d.Markdown)
		}
	}
	info.Context, info.ContextErrs = ri.Context, append(info.ContextErrs, ri.ContextErrs...)

	prompt := PlanPrompt(info)
	for _, p := range []string{filepath.Join(ru.wt, ".loop", "brief.md"), filepath.Join(ru.dir, "brief.md")} {
		if err := os.WriteFile(p, []byte(prompt), 0o644); err != nil {
			return "", err
		}
	}
	home := Home()
	// The guard's write roots are out/ and the scratch dir: the checkouts, the context and the
	// rest of the plan's directory are read-only to the agent.
	gc := guard.Config{
		Worktree: out, Scratch: ru.scratch, Home: home,
		Protected: r.cfgOf(ru).Protected(), Secrets: r.cfgOf(ru).Secrets(), Words: []string{r.cfgOf(ru).UserEmulator},
		LoopdAddrs: r.cfgOf(ru).LoopdAddrs(), ReadOnly: true,
		Report: filepath.Join(ru.dir, "refusals.jsonl"), Armed: filepath.Join(ru.dir, "armed"),
	}
	b, _ := json.MarshalIndent(gc, "", "  ")
	if err := os.WriteFile(filepath.Join(ru.dir, "guard.json"), b, 0o600); err != nil {
		return "", err
	}
	settings, err := GenerateSettings(r.planSettingsInput(home))
	if err != nil {
		return "", err
	}
	if err := writeAtomic(ru.settings, settings); err != nil {
		return "", err
	}
	if _, err := os.Stat(r.cfgOf(ru).Guard); err != nil {
		return "", fmt.Errorf("the guard binary %s: %w", r.cfgOf(ru).Guard, err)
	}
	argv, _ := json.Marshal(r.agentCommand(ru).Args[1:])
	if err := os.WriteFile(filepath.Join(ru.dir, "argv.json"), argv, 0o600); err != nil {
		return "", err
	}
	return prompt, nil
}

// planSettingsInput is the settings of every plan turn: read-only (no commits), like triage's,
// and nothing per repo or per plan.
func (r *Runner) planSettingsInput(home string) SettingsInput {
	return SettingsInput{Mode: r.Config().Mode, Guard: r.Config().Guard, Home: home, Protected: r.Config().Protected(), Secrets: r.Config().Secrets(),
		LoopdAddrs: r.Config().LoopdAddrs(), AllowedDomains: r.Config().AllowedDomains, ReadOnly: true}
}

// planCheckouts checks the plan's repos out under repos/<key>/, detached at their base after a
// fetch (a fresh worktree each turn), and removes the checkouts of repos the plan no longer has.
func (r *Runner) planCheckouts(ctx context.Context, ru *run, info *PlanTurnInfo) error {
	want := map[string]bool{}
	for _, slug := range ru.plan.Repos {
		key, rc, ok := r.cfgOf(ru).Repo(slug)
		if !ok {
			info.ContextErrs = append(info.ContextErrs, "the repo "+slug+" (this runner has no such repo configured)")
			continue
		}
		dir := "repos/" + Slug(key)
		if want[dir] {
			continue
		}
		want[dir] = true
		mirror := r.mirrorPath(key)
		unlock := r.lockMirror(mirror)
		err := r.Git.EnsureMirror(ctx, mirror, rc)
		if err == nil {
			err = r.Git.AddDetached(ctx, mirror, filepath.Join(ru.wt, dir), "refs/remotes/origin/"+rc.Base)
		}
		unlock()
		if err != nil {
			return fmt.Errorf("checking out %s: %w", slug, err)
		}
		info.Checkouts = append(info.Checkouts, Checkout{Key: key, Slug: rc.Slug, Base: rc.Base, Dir: dir})
	}
	ents, _ := os.ReadDir(filepath.Join(ru.wt, "repos"))
	for _, e := range ents {
		dir := "repos/" + e.Name()
		if want[dir] {
			continue
		}
		path := filepath.Join(ru.wt, dir)
		for key := range r.cfgOf(ru).Repos {
			if Slug(key) == e.Name() {
				mirror := r.mirrorPath(key)
				unlock := r.lockMirror(mirror)
				r.Git.RemoveWorktree(ctx, mirror, path)
				unlock()
			}
		}
		removeTree(path)
	}
	return nil
}

// reposTable is .loop/context/repos.md for a plan turn: every configured repo.
func (r *Runner) reposTable() string {
	keys := make([]string, 0, len(r.Config().Repos))
	for k := range r.Config().Repos {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# The repos the loop works on\n\nName them by slug (`owner/name`) in `repos` and in the blueprint's pieces.\n\n")
	b.WriteString("| key | slug | group | base | what it is |\n| --- | --- | --- | --- | --- |\n")
	for _, k := range keys {
		rc := r.Config().Repos[k]
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", k, rc.Slug, cell(rc.Group), rc.Base, cell(rc.Description))
	}
	return b.String()
}

// reportPlan turns how a plan turn ended into its result for loopd.
func (r *Runner) reportPlan(ctx context.Context, ru *run, out outcome, log *slog.Logger) {
	r.flushRefusals(ctx, ru)
	switch out.reason {
	case endStopped, endShutdown:
		// loopd ended the lease (Stop puts the turn back in the queue), or it will expire.
		return
	case endTimeCap:
		r.postTurn(ctx, ru, out, out.detail, log)
		return
	case endModeMismatch:
		r.paused.Store(out.detail)
		log.Error("pausing: the agent did not get the permission mode it was started with", "detail", out.detail)
		r.postTurn(ctx, ru, out, "boundary check failed: "+out.detail+"; the runner stopped claiming until it is restarted", log)
		return
	case endNotArmed, endStartFailed:
		r.postTurn(ctx, ru, out, "boundary check failed ("+out.reason+"): "+out.detail, log)
		return
	}
	if hit := out.stats.limit; hit != nil {
		why := "the agent hit the usage limit"
		if hit.Reset != "" {
			why += " (resets " + hit.Reset + ")"
		}
		r.postTurn(ctx, ru, out, why, log)
		return
	}
	raw, err := readResult(filepath.Join(ru.wt, "out", "result.json"))
	if err != nil {
		r.postTurn(ctx, ru, out, fmt.Sprintf("the agent exited (code %d) without a usable out/result.json (%v)", out.exitCode, err), log)
		return
	}
	res, err := ValidatePlanResult(raw, ru.turn.Persona, filepath.Join(ru.wt, "out"), r.repoSlugs())
	if err != nil {
		r.postTurn(ctx, ru, out, err.Error(), log)
		return
	}
	res.SessionID = out.stats.sessionID
	if res, ok := r.critique(ctx, ru, res, log); ok {
		r.sendTurn(ctx, ru, res, log)
	}
}

// postTurn fails the turn with why; the session id is kept, so the next turn resumes it.
func (r *Runner) postTurn(ctx context.Context, ru *run, out outcome, why string, log *slog.Logger) {
	why = clip(strings.TrimSpace(why), 1500)
	res := pipeline.TurnResult{Error: why, Reply: "That turn failed: " + why + "; reply to try again.", SessionID: out.stats.sessionID}
	r.sendTurn(ctx, ru, res, log)
}

func (r *Runner) sendTurn(ctx context.Context, ru *run, res pipeline.TurnResult, log *slog.Logger) {
	if log == nil {
		log = r.Log
	}
	t, err := r.Client.PlanTurnResult(ctx, ru.plan.ID, ru.turn.ID, res)
	if err != nil {
		log.Error("plan turn result", "plan", ru.plan.ID, "turn", ru.turn.ID, "err", err)
		return
	}
	log.Info("reported", "plan", ru.plan.ID, "turn", ru.turn.ID, "status", t.Status, "error", t.Error)
}

// repoSlugs are the configured repos' slugs.
func (r *Runner) repoSlugs() []string {
	var out []string
	for _, rc := range r.Config().Repos {
		out = append(out, rc.Slug)
	}
	sort.Strings(out)
	return out
}

// planResult is out/result.json.
type planResult struct {
	Reply       string   `json:"reply"`
	Brief       string   `json:"brief"`
	Blueprint   string   `json:"blueprint"`
	Ready       bool     `json:"ready"`
	BackToBrief string   `json:"back_to_brief"`
	Repos       []string `json:"repos"`
}

// ValidatePlanResult checks a plan turn's result (out/result.json) for a persona and loads the
// documents it names from outDir: the reply, the persona's own fields, the repos (configured
// slugs), and a blueprint's pieces (they parse, do not loop, name configured repos). loopd checks
// the content again; this fails the turn before it is sent.
func ValidatePlanResult(raw []byte, persona, outDir string, slugs []string) (pipeline.TurnResult, error) {
	var v planResult
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return pipeline.TurnResult{}, fmt.Errorf("out/result.json: %v", err)
	}
	var res pipeline.TurnResult
	res.Reply = strings.TrimSpace(v.Reply)
	switch n := utf8.RuneCountInString(res.Reply); {
	case n == 0:
		return res, fmt.Errorf("the result has no reply")
	case n > pipeline.MaxReply:
		return res, fmt.Errorf("the reply has %d characters (at most %d)", n, pipeline.MaxReply)
	}
	back := strings.TrimSpace(v.BackToBrief)
	switch persona {
	case pipeline.PersonaProduct:
		if v.Blueprint != "" {
			return res, fmt.Errorf("only engineering writes the blueprint")
		}
		if back != "" {
			return res, fmt.Errorf("only engineering sends a plan back to the brief")
		}
	case pipeline.PersonaEngineering:
		if v.Brief != "" {
			return res, fmt.Errorf("only product writes the brief")
		}
		if len(v.Repos) > 0 {
			return res, fmt.Errorf("only product sets the plan's repos")
		}
	default:
		return res, fmt.Errorf("unknown persona %q", persona)
	}
	if back != "" && (v.Ready || v.Blueprint != "") {
		return res, fmt.Errorf("back_to_brief goes alone: not with ready or a blueprint")
	}
	for _, s := range v.Repos {
		if !slices.Contains(slugs, strings.TrimSpace(s)) {
			return res, fmt.Errorf("repos: %q is not a configured repo (%s)", s, strings.Join(slugs, ", "))
		}
		res.Repos = append(res.Repos, strings.TrimSpace(s))
	}
	var err error
	if v.Brief != "" {
		if res.Brief, err = loadPlanDoc(outDir, v.Brief); err != nil {
			return res, fmt.Errorf("brief: %v", err)
		}
	}
	if v.Blueprint != "" {
		if res.Blueprint, err = loadBlueprint(outDir, v.Blueprint, slugs); err != nil {
			return res, err
		}
	}
	res.Ready, res.BackToBrief = v.Ready, back
	return res, nil
}

// loadBlueprint loads a blueprint the agent wrote in out/ and checks its pieces: they parse, do
// not loop, and name configured repos.
func loadBlueprint(outDir, name string, slugs []string) (string, error) {
	md, err := loadPlanDoc(outDir, name)
	if err != nil {
		return "", fmt.Errorf("blueprint: %v", err)
	}
	pieces, err := pipeline.ParseBlueprint(md)
	if err != nil {
		return "", fmt.Errorf("blueprint: %v", err)
	}
	for _, pc := range pieces {
		if pc.Repo != "" && !slices.Contains(slugs, pc.Repo) {
			return "", fmt.Errorf("blueprint: piece %d's repo %q is not a configured repo", pc.N, pc.Repo)
		}
	}
	return md, nil
}

// loadPlanDoc reads a document the agent wrote: a plain file directly in out/ ("brief.md"
// or "out/brief.md"), UTF-8, not empty, at most MaxPlanDocBytes. Names with a directory are
// refused: only the leaf is Lstat'ed, and a symlinked directory could point at a secret.
func loadPlanDoc(outDir, name string) (string, error) {
	rel := filepath.ToSlash(filepath.Clean(strings.TrimSpace(name)))
	rel = strings.TrimPrefix(rel, "out/")
	if !filepath.IsLocal(rel) || strings.Contains(rel, "/") {
		return "", fmt.Errorf("%q is not a file in out/", name)
	}
	p := filepath.Join(outDir, rel)
	fi, err := os.Lstat(p)
	if err != nil {
		return "", fmt.Errorf("%q: %v", name, err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a plain file", name)
	}
	if fi.Size() > MaxPlanDocBytes {
		return "", fmt.Errorf("%q has %d bytes (at most %d)", name, fi.Size(), MaxPlanDocBytes)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("%q is not UTF-8 text", name)
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", fmt.Errorf("%q is empty", name)
	}
	return string(b), nil
}
