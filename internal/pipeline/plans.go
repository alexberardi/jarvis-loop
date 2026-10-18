package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Plans (docs/phases/07-planning.md, "Plans"): an idea is talked into a brief (product), then a
// blueprint (engineering), each approved by the user against the document's sha256; approving the
// blueprint splits it into items, one per piece.

// PlanStage is where a plan is.
type PlanStage string

// The plan stages.
const (
	PlanBrief     PlanStage = "brief"
	PlanBlueprint PlanStage = "blueprint"
	PlanSplit     PlanStage = "split"
	PlanDone      PlanStage = "done"
	PlanDropped   PlanStage = "dropped"
)

// PlanStages lists every plan stage, in order.
var PlanStages = []PlanStage{PlanBrief, PlanBlueprint, PlanSplit, PlanDone, PlanDropped}

// Valid reports whether s is a known plan stage.
func (s PlanStage) Valid() bool { return slices.Contains(PlanStages, s) }

// Open reports whether the plan can still change: talked about, or split and not finished.
func (s PlanStage) Open() bool { return s == PlanBrief || s == PlanBlueprint || s == PlanSplit }

// Talking reports whether the plan is in a conversation: its turns run.
func (s PlanStage) Talking() bool { return s == PlanBrief || s == PlanBlueprint }

// The documents a plan has.
const (
	DocBrief     = "brief"
	DocBlueprint = "blueprint"
)

// The personas that run a plan's turns.
const (
	PersonaProduct     = "product"
	PersonaEngineering = "engineering"
)

// PersonaFor is who talks with the user at a stage: product writes the brief, engineering the
// blueprint; "" when the plan is not in a conversation.
func PersonaFor(s PlanStage) string {
	switch s {
	case PlanBrief:
		return PersonaProduct
	case PlanBlueprint:
		return PersonaEngineering
	}
	return ""
}

// SourcePlan is the source of an item filed by a plan's hand-off. It is not one of Sources: only
// the hand-off files such items.
const SourcePlan = "plan"

// TurnStatus is where a plan turn is.
type TurnStatus string

// The turn statuses.
const (
	TurnQueued  TurnStatus = "queued"
	TurnRunning TurnStatus = "running"
	TurnDone    TurnStatus = "done"
	TurnFailed  TurnStatus = "failed"
)

// Plan is one idea on its way to items.
type Plan struct {
	ID           int64     `json:"id"`
	Title        string    `json:"title"`
	Stage        PlanStage `json:"stage"`
	Priority     string    `json:"priority"`
	Repos        []string  `json:"repos"`
	SlackChannel string    `json:"slack_channel,omitempty"`
	SlackTS      string    `json:"slack_ts,omitempty"`
	// SlackHeader is the bot's message heading the thread (stage, Drop button).
	SlackHeader        string `json:"slack_header,omitempty"`
	ProductSession     string `json:"product_session,omitempty"`
	EngineeringSession string `json:"engineering_session,omitempty"`
	// BriefApproved and BlueprintApproved are the approved versions (0: none yet).
	BriefApproved     int       `json:"brief_approved,omitempty"`
	BlueprintApproved int       `json:"blueprint_approved,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// PlanDoc is one version of a plan's brief or blueprint.
type PlanDoc struct {
	PlanID    int64     `json:"plan_id"`
	Kind      string    `json:"kind"`
	Version   int       `json:"version"`
	Markdown  string    `json:"markdown,omitempty"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
}

// DocSHA256 is the hex sha256 of a document's markdown: what an approval binds to.
func DocSHA256(md string) string {
	sum := sha256.Sum256([]byte(md))
	return hex.EncodeToString(sum[:])
}

// PlanTurn is one agent turn of a plan's conversation.
type PlanTurn struct {
	ID      int64  `json:"id"`
	PlanID  int64  `json:"plan_id"`
	Persona string `json:"persona,omitempty"`
	// Messages are the user's messages batched into the turn, verbatim.
	Messages []string `json:"messages"`
	// SlackTS are the Slack ts of the messages that came from the plan's thread (#plan).
	SlackTS []string `json:"slack_ts,omitempty"`
	// BackToBrief is why engineering sent the plan back: the product turn's prompt starts with it.
	BackToBrief string     `json:"back_to_brief,omitempty"`
	Status      TurnStatus `json:"status"`
	Runner      string     `json:"runner,omitempty"`
	Agent       string     `json:"agent,omitempty"`
	Reply       string     `json:"reply,omitempty"`
	Ready       bool       `json:"ready,omitempty"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// NewPlan builds a new plan at brief (default P3). repos must be configured slugs.
func NewPlan(title, priority string, repos, configured []string, now time.Time) (Plan, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Plan{}, errf(CodeMissingField, "title is required")
	}
	if priority == "" {
		priority = "P3"
	}
	if !ValidPriority(priority) {
		return Plan{}, errf(CodeInvalidField, "priority must be P1 to P5")
	}
	repos, err := planRepos(repos, configured)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Title: title, Stage: PlanBrief, Priority: priority, Repos: repos, CreatedAt: now, UpdatedAt: now}, nil
}

// planRepos checks a plan's repos against the configured slugs, dropping repeats.
func planRepos(repos, configured []string) ([]string, error) {
	out := []string{}
	for _, r := range repos {
		r = strings.TrimSpace(r)
		if r == "" || slices.Contains(out, r) {
			continue
		}
		if !slices.Contains(configured, r) {
			return nil, errf(CodeInvalidField, "repo must be a configured repo (%s), not %q", strings.Join(configured, ", "), r)
		}
		out = append(out, r)
	}
	return out, nil
}

// PlanApproval is an approval that applies: the plan as it should be stored, the document
// approved, and, for a blueprint, its pieces.
type PlanApproval struct {
	Plan   Plan
	Doc    PlanDoc
	Pieces []Piece
}

// ApplyPlanApprove is the user approving a plan's document, bound to the sha256 of the version
// they saw: brief → blueprint, blueprint → split. docs are every version of that kind, oldest
// first. A newer version makes the approval stale: refused, naming the version to approve.
func ApplyPlanApprove(p Plan, kind, sha string, docs []PlanDoc, configured []string) (PlanApproval, error) {
	if kind != DocBrief && kind != DocBlueprint {
		return PlanApproval{}, errf(CodeInvalidField, "kind must be brief or blueprint, not %q", kind)
	}
	if p.Stage != PlanStage(kind) {
		return PlanApproval{}, errf(CodeWrongStage, "plan P%d is at %s, not %s", p.ID, p.Stage, kind)
	}
	if len(docs) == 0 {
		return PlanApproval{}, errf(CodeWrongStage, "plan P%d has no %s yet", p.ID, kind)
	}
	sha = strings.ToLower(strings.TrimSpace(sha))
	if sha == "" {
		return PlanApproval{}, errf(CodeMissingField, "sha256 is required (the version you approve)")
	}
	latest := docs[len(docs)-1]
	if sha != latest.SHA256 {
		for _, d := range docs {
			if d.SHA256 == sha {
				return PlanApproval{}, errf(CodeSHAMismatch, "the %s changed; approve v%d instead (that was v%d)", kind, latest.Version, d.Version)
			}
		}
		return PlanApproval{}, errf(CodeSHAMismatch, "no %s of plan P%d has sha256 %s; approve v%d instead", kind, p.ID, sha, latest.Version)
	}
	a := PlanApproval{Doc: latest}
	switch kind {
	case DocBrief:
		p.Stage, p.BriefApproved = PlanBlueprint, latest.Version
	case DocBlueprint:
		pieces, err := ParseBlueprint(latest.Markdown)
		if err != nil {
			return PlanApproval{}, errf(CodeInvalidField, "blueprint v%d does not split: %s", latest.Version, err)
		}
		for _, pc := range pieces {
			if pc.Repo != "" && !slices.Contains(configured, pc.Repo) {
				return PlanApproval{}, errf(CodeInvalidField, "blueprint v%d: piece %d's repo %q is not a configured repo", latest.Version, pc.N, pc.Repo)
			}
		}
		p.Stage, p.BlueprintApproved = PlanSplit, latest.Version
		a.Pieces = pieces
	}
	a.Plan = p
	return a, nil
}

// ApplyPlanDrop drops an open plan. Items it already filed are untouched.
func ApplyPlanDrop(p Plan) (Plan, error) {
	if !p.Stage.Open() {
		return p, errf(CodeWrongStage, "plan P%d is %s", p.ID, p.Stage)
	}
	p.Stage = PlanDropped
	return p, nil
}

// PlanSettled reports whether a split plan is done, given its items' stages: none is open and at
// least one is done (dropped and closed items don't count).
func PlanSettled(stages []Stage) bool {
	done := false
	for _, st := range stages {
		switch st {
		case Done:
			done = true
		case Closed:
		default:
			return false
		}
	}
	return done
}

// ---- turn results ----

// MaxReply is the longest reply a turn may post (Slack's limit on a message's text).
const MaxReply = 3000

// TurnResult is what a runner reports at the end of a plan turn.
type TurnResult struct {
	// Reply is the Slack message.
	Reply string `json:"reply"`
	// Brief and Blueprint are the document's new markdown, when the turn wrote one.
	Brief     string `json:"brief,omitempty"`
	Blueprint string `json:"blueprint,omitempty"`
	// Ready: the agent thinks the document is ready to approve (post the Approve button).
	Ready bool `json:"ready,omitempty"`
	// BackToBrief (engineering only) sends the plan back to the brief, with why.
	BackToBrief string `json:"back_to_brief,omitempty"`
	// Repos (product only) sets the plan's repos.
	Repos []string `json:"repos,omitempty"`
	// SessionID is the persona's claude session, resumed by its next turn.
	SessionID string `json:"session_id,omitempty"`
	// Error: the turn failed (the runner's own checks, or the agent); the reply says so.
	Error string `json:"error,omitempty"`
}

// TurnOutcome is what a turn's result does.
type TurnOutcome struct {
	// Plan is the plan as it should be stored (session id, repos, stage).
	Plan Plan
	// Failed is why the turn failed; nothing else changes then but the session id.
	Failed string
	// Docs are the new document versions to add.
	Docs  []PlanDoc
	Ready bool
	// BackToBrief: the plan went back to brief; queue a product turn with the reason.
	BackToBrief bool
	// Ignored names what the result carried that no longer applies (the plan moved on mid-turn).
	Ignored []string
}

// ApplyTurnResult judges a turn's result. latest holds the plan's latest document of each kind.
// Content an agent got wrong (a blueprint that does not split, an unknown repo, a missing reply)
// fails the turn rather than refusing the call: the runner's lease is over either way, and the
// user is told and replies to try again.
func ApplyTurnResult(p Plan, t PlanTurn, r TurnResult, latest map[string]PlanDoc, configured []string, now time.Time) TurnOutcome {
	if sid := strings.TrimSpace(r.SessionID); sid != "" {
		switch t.Persona {
		case PersonaProduct:
			p.ProductSession = sid
		case PersonaEngineering:
			p.EngineeringSession = sid
		}
	}
	fail := func(why string) TurnOutcome { return TurnOutcome{Plan: p, Failed: why} }
	if e := strings.TrimSpace(r.Error); e != "" {
		return fail(e)
	}
	reply := strings.TrimSpace(r.Reply)
	if reply == "" {
		return fail("the result has no reply")
	}
	if n := utf8.RuneCountInString(reply); n > MaxReply {
		return fail(fmt.Sprintf("the reply has %d characters (at most %d)", n, MaxReply))
	}
	back := strings.TrimSpace(r.BackToBrief)
	switch t.Persona {
	case PersonaProduct:
		if r.Blueprint != "" {
			return fail("only engineering writes the blueprint")
		}
		if back != "" {
			return fail("only engineering sends a plan back to the brief")
		}
	case PersonaEngineering:
		if r.Brief != "" {
			return fail("only product writes the brief")
		}
		if len(r.Repos) > 0 {
			return fail("only product sets the plan's repos")
		}
	}
	if back != "" && r.Ready {
		return fail("the result is both ready and back_to_brief")
	}
	var repos []string
	if len(r.Repos) > 0 {
		var err error
		if repos, err = planRepos(r.Repos, configured); err != nil {
			return fail(err.(*Error).Message)
		}
	}
	if r.Blueprint != "" {
		pieces, err := ParseBlueprint(r.Blueprint)
		if err != nil {
			return fail("blueprint: " + err.Error())
		}
		for _, pc := range pieces {
			if pc.Repo != "" && !slices.Contains(configured, pc.Repo) {
				return fail(fmt.Sprintf("blueprint: piece %d's repo %q is not a configured repo", pc.N, pc.Repo))
			}
		}
	}
	o := TurnOutcome{Plan: p}
	// The plan moved on while the turn ran (approved, sent back, dropped): the reply still
	// goes to the user, but the turn no longer speaks for the plan's stage.
	if PersonaFor(p.Stage) != t.Persona {
		for _, x := range []struct {
			name string
			set  bool
		}{{"brief", r.Brief != ""}, {"blueprint", r.Blueprint != ""}, {"ready", r.Ready}, {"back_to_brief", back != ""}, {"repos", len(r.Repos) > 0}} {
			if x.set {
				o.Ignored = append(o.Ignored, x.name)
			}
		}
		return o
	}
	if repos != nil {
		o.Plan.Repos = repos
	}
	kind, md := DocBrief, r.Brief
	if t.Persona == PersonaEngineering {
		kind, md = DocBlueprint, r.Blueprint
	}
	have, ok := latest[kind]
	if md != "" && (!ok || have.SHA256 != DocSHA256(md)) {
		d := PlanDoc{PlanID: p.ID, Kind: kind, Version: have.Version + 1, Markdown: md, SHA256: DocSHA256(md), CreatedAt: now}
		o.Docs = append(o.Docs, d)
		ok = true
	}
	if r.Ready && !ok {
		return fail("ready, but there is no " + kind + " yet")
	}
	o.Ready = r.Ready
	if back != "" {
		o.Plan.Stage = PlanBrief
		o.BackToBrief = true
	}
	return o
}

// ---- blueprint pieces ----

// Piece is one loop-sized part of a blueprint: it becomes one item.
type Piece struct {
	N     int    `json:"n"`
	Title string `json:"title"`
	Repo  string `json:"repo,omitempty"`
	Size  string `json:"size,omitempty"`
	// Builder is who should build it: BuilderQwen or BuilderClaude ("": not said).
	Builder   string `json:"builder,omitempty"`
	DependsOn []int  `json:"depends_on,omitempty"`
	// After is the existing loop items ("#N" in "depends on:") the piece waits for: the hand-off
	// adds them to the item's depends_on next to its pieces' items.
	After []int64 `json:"after,omitempty"`
	// Section is the piece's part of the blueprint, heading included: the item's ask ends with it.
	Section string `json:"section"`
}

var (
	headingRe = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	pieceRe   = regexp.MustCompile(`(?i)^(?:piece\s+)?(\d+)\s*[.):]?\s*(.*)$`)
	fieldRe   = regexp.MustCompile(`(?i)^\s*(?:[-*]\s+)?\**\s*(repo|size|builder|depends on)\s*\**\s*:\s*\**\s*(.*?)\s*$`)
	numRe     = regexp.MustCompile(`\d+`)
	itemRefRe = regexp.MustCompile(`#\d+`)
)

// The builders a piece names (docs/decisions/2026-10-16-qwen-builder-plans.md): qwen only when
// every step is mechanical, claude otherwise and when the piece does not say.
const (
	BuilderQwen   = "qwen"
	BuilderClaude = "claude"
)

// ParseBlueprint finds a blueprint's pieces: under a "Pieces" heading, one sub-heading per piece,
// "N. Title", numbered from 1 in order, with optional "repo:", "size:", "builder:" and "depends on:" lines
// (piece numbers, "#N" for an existing loop item the piece waits for; "none"). Piece dependencies
// must name other pieces and must not loop; the store checks the items when it hands off.
func ParseBlueprint(md string) ([]Piece, error) {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	level := 0
	start := -1
	for i, l := range lines {
		if m := headingRe.FindStringSubmatch(l); m != nil && strings.EqualFold(m[2], "pieces") {
			level, start = len(m[1]), i+1
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("no Pieces section (a heading named Pieces, one sub-heading per piece)")
	}
	var pieces []Piece
	var body []string
	flush := func() {
		if len(pieces) > 0 {
			pieces[len(pieces)-1].Section = strings.TrimSpace(strings.Join(body, "\n"))
		}
		body = nil
	}
	for _, l := range lines[start:] {
		m := headingRe.FindStringSubmatch(l)
		if m != nil && len(m[1]) <= level {
			break
		}
		if m != nil && len(m[1]) == level+1 {
			flush()
			pm := pieceRe.FindStringSubmatch(m[2])
			if pm == nil {
				return nil, fmt.Errorf("piece heading %q must start with the piece number (\"1. Title\")", m[2])
			}
			n, _ := strconv.Atoi(pm[1])
			if n != len(pieces)+1 {
				if len(pieces) == 0 {
					return nil, fmt.Errorf("pieces are numbered from 1: the first is piece %d", n)
				}
				return nil, fmt.Errorf("piece %d comes after piece %d: number them 1, 2, 3 in order", n, len(pieces))
			}
			title := strings.TrimSpace(pm[2])
			if title == "" {
				return nil, fmt.Errorf("piece %d has no title", n)
			}
			pieces = append(pieces, Piece{N: n, Title: title})
			body = []string{l}
			continue
		}
		if len(pieces) == 0 {
			continue
		}
		body = append(body, l)
		fm := fieldRe.FindStringSubmatch(l)
		if fm == nil {
			continue
		}
		pc := &pieces[len(pieces)-1]
		val := strings.Trim(fm[2], "`* ")
		switch strings.ToLower(fm[1]) {
		case "repo":
			pc.Repo = val
		case "size":
			if !slices.Contains(Sizes, strings.ToUpper(val)) {
				return nil, fmt.Errorf("piece %d: size must be S, M or L, not %q", pc.N, val)
			}
			pc.Size = strings.ToUpper(val)
		case "builder":
			b := strings.ToLower(val)
			if b != BuilderQwen && b != BuilderClaude {
				return nil, fmt.Errorf("piece %d: builder must be qwen or claude, not %q", pc.N, val)
			}
			pc.Builder = b
		case "depends on":
			for _, d := range itemRefRe.FindAllString(val, -1) {
				id, _ := strconv.ParseInt(d[1:], 10, 64)
				if !slices.Contains(pc.After, id) {
					pc.After = append(pc.After, id)
				}
			}
			for _, d := range numRe.FindAllString(itemRefRe.ReplaceAllString(val, ""), -1) {
				n, _ := strconv.Atoi(d)
				if !slices.Contains(pc.DependsOn, n) {
					pc.DependsOn = append(pc.DependsOn, n)
				}
			}
		}
	}
	flush()
	if len(pieces) == 0 {
		return nil, fmt.Errorf("the Pieces section has no pieces (sub-headings \"1. Title\")")
	}
	graph := map[int64][]int64{}
	for _, pc := range pieces {
		graph[int64(pc.N)] = []int64{}
		for _, d := range pc.DependsOn {
			switch {
			case d == pc.N:
				return nil, fmt.Errorf("piece %d depends on itself", pc.N)
			case d < 1 || d > len(pieces):
				return nil, fmt.Errorf("piece %d depends on piece %d, which does not exist", pc.N, d)
			}
			graph[int64(pc.N)] = append(graph[int64(pc.N)], int64(d))
		}
	}
	if c := DependencyCycle(graph); c != nil {
		path := make([]string, len(c))
		for i, n := range c {
			path[i] = strconv.FormatInt(n, 10)
		}
		return nil, fmt.Errorf("the pieces' dependencies make a cycle: %s", strings.Join(path, " → "))
	}
	return pieces, nil
}

// PlanItems are the items a split plan files, one per piece, in piece order: source plan, the
// plan's priority, linked to the plan and piece, the piece's section as the ask's tail. A piece
// without a repo takes the plan's, when it has exactly one; one without a builder gets claude.
// Each carries the plan's title as its feature (FeatureSlug). Dependencies are piece numbers here
// (Piece.DependsOn); the store maps them to the new items' ids and adds the existing items
// (Piece.After).
func PlanItems(p Plan, pieces []Piece, configured []string, now time.Time) ([]Item, error) {
	out := make([]Item, 0, len(pieces))
	for _, pc := range pieces {
		repo := pc.Repo
		if repo == "" && len(p.Repos) == 1 {
			repo = p.Repos[0]
		}
		if repo != "" && !slices.Contains(configured, repo) {
			return nil, errf(CodeInvalidField, "piece %d's repo %q is not a configured repo", pc.N, repo)
		}
		ask := fmt.Sprintf("Piece %d of plan P%d (%s).\n\n%s", pc.N, p.ID, p.Title, pc.Section)
		it, err := NewItem(pc.Title, ask, repo, p.Priority, "user", now)
		if err != nil {
			return nil, err
		}
		it.Source, it.PlanID, it.PlanPiece, it.Builder = SourcePlan, p.ID, pc.N, pc.Builder
		it.Feature = FeatureSlug(p.Title)
		if it.Builder == "" {
			it.Builder = BuilderClaude
		}
		out = append(out, it)
	}
	return out, nil
}
