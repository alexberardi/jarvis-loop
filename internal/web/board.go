package web

import (
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/api"
	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// The swim-lane board (docs/decisions/2026-10-15-board.md): read-only, server-rendered. Its only
// JavaScript is live.js, which refreshes the "Working now" section from GET /board/working
// (docs/decisions/2026-10-16-working-now.md). It signs in with a board (or user) token passed
// once as ?t=, kept in a cookie.

// BoardCookie holds the token the board was opened with.
const BoardCookie = "loop_board"

// DoneWindow is how long a done item stays on the board.
const DoneWindow = 14 * 24 * time.Hour

const cookieMaxAge = 30 * 24 * 60 * 60

// BoardConfig configures the board.
type BoardConfig struct {
	// SlackWorkspace is the workspace's address (https://<team>.slack.com), for links to item
	// threads; the cards have none without it.
	SlackWorkspace string
	// Clock decides which done items are recent (default: the real clock).
	Clock clock.Clock
}

type boardPage struct {
	st     *store.Store
	tokens *api.Tokens
	log    *slog.Logger
	cfg    BoardConfig
}

// NewBoard returns the handler for GET /board, GET /board/working, GET /board/live.js,
// GET /board/items/{id}, GET /board/items/{id}/evidence/{n}, GET /board/plans/{id},
// GET /board/plans/{id}/{kind}/{v}, GET /board/regressions/{id}, its files, and the user's
// POST /board/regressions, and nothing else.
func NewBoard(st *store.Store, tokens *api.Tokens, log *slog.Logger, cfg BoardConfig) http.Handler {
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	p := &boardPage{st: st, tokens: tokens, log: log, cfg: cfg}
	mux := http.NewServeMux()
	mux.Handle("GET /board", p.auth(p.board))
	mux.Handle("GET /board/working", p.auth(p.working))
	mux.Handle("GET /board/live.js", p.auth(p.liveJS))
	mux.Handle("GET /board/items/{id}", p.auth(p.item))
	mux.Handle("GET /board/items/{id}/evidence/{n}", p.auth(p.evidence))
	mux.Handle("GET /board/plans/{id}", p.auth(p.plan))
	mux.Handle("GET /board/plans/{id}/{kind}/{v}", p.auth(p.planDoc))
	mux.Handle("GET /board/regressions/{id}", p.auth(p.regression))
	mux.Handle("GET /board/regressions/{id}/files/{n}", p.auth(p.regressionFile))
	// The board's one write: the user's "Run regression" button. The cookie is SameSite=Strict,
	// and the post is guarded against cross-origin requests too.
	mux.Handle("POST /board/regressions", http.NewCrossOriginProtection().Handler(p.auth(p.runRegression)))
	return mux
}

// boardCSP is every board page's policy; liveCSP adds what live.js needs (the board and the item
// page): the script from /board/live.js and fetches of /board/working, nothing inline.
const (
	boardCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; form-action 'none'; frame-ancestors 'none'"
	liveCSP  = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; script-src 'self'; connect-src 'self'; form-action 'none'; frame-ancestors 'none'"
	// boardFormCSP is the board's own for the user's token: its "Run regression" form posts to it.
	boardFormCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; script-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'"
)

// boardKinds may read the board: its own read-only kind, and the user, who may do more anyway.
var boardKinds = []string{api.KindBoard, api.KindUser}

func (p *boardPage) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", boardCSP)
		q := r.URL.Query()
		if q.Has("t") {
			// The link with the token is opened once: keep the token in a cookie and drop it
			// from the address bar, so it stays out of history and bookmarks.
			tok := q.Get("t")
			pr, ok := p.tokens.Lookup(tok)
			if !ok {
				http.Error(w, "that token is not valid", http.StatusUnauthorized)
				return
			}
			api.SetActor(w, pr)
			if !slices.Contains(boardKinds, pr.Kind) {
				http.Error(w, "the board takes a board or user token", http.StatusForbidden)
				return
			}
			c := &http.Cookie{Name: BoardCookie, Value: tok, Path: "/board", MaxAge: cookieMaxAge,
				HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode}
			if c.Valid() != nil {
				http.Error(w, "that token cannot be kept in a cookie", http.StatusBadRequest)
				return
			}
			http.SetCookie(w, c)
			q.Del("t")
			to := r.URL.Path
			if len(q) > 0 {
				to += "?" + q.Encode()
			}
			http.Redirect(w, r, to, http.StatusSeeOther)
			return
		}
		c, err := r.Cookie(BoardCookie)
		if err != nil {
			http.Error(w, "open the board's link with ?t=<board token> once", http.StatusUnauthorized)
			return
		}
		pr, ok := p.tokens.Lookup(c.Value)
		if !ok {
			http.Error(w, "the board's cookie holds a token loopd no longer knows; open the link with ?t= again", http.StatusUnauthorized)
			return
		}
		api.SetActor(w, pr)
		if !slices.Contains(boardKinds, pr.Kind) {
			http.Error(w, "the board takes a board or user token", http.StatusForbidden)
			return
		}
		// Expiry, as on the API: a read never shows a dead lease as live.
		if err := p.st.Sweep(r.Context()); err != nil {
			p.fail(w, err)
			return
		}
		next(w, r)
	})
}

func (p *boardPage) fail(w http.ResponseWriter, err error) {
	if pe := (*pipeline.Error)(nil); errors.As(err, &pe) && pe.Code == store.CodeNotFound {
		http.Error(w, pe.Message, http.StatusNotFound)
		return
	}
	p.log.Error("board", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (p *boardPage) board(w http.ResponseWriter, r *http.Request) {
	items, err := p.st.BoardItems(r.Context())
	if err != nil {
		p.fail(w, err)
		return
	}
	plans, err := p.st.ListPlans(r.Context(), true)
	if err != nil {
		p.fail(w, err)
		return
	}
	work, err := p.st.Working(r.Context(), 0)
	if err != nil {
		p.fail(w, err)
		return
	}
	orch, err := p.st.OrchestratorBoard(r.Context(), orchLimit)
	if err != nil {
		p.fail(w, err)
		return
	}
	draining, err := p.st.RunnerStates(r.Context())
	if err != nil {
		p.fail(w, err)
		return
	}
	fz, err := p.st.Freezes(r.Context())
	if err != nil {
		p.fail(w, err)
		return
	}
	runners, err := p.st.Runners(r.Context())
	if err != nil {
		p.fail(w, err)
		return
	}
	regs, err := p.st.Regressions(r.Context(), regLimit)
	if err != nil {
		p.fail(w, err)
		return
	}
	rw, err := p.st.Runway(r.Context())
	if err != nil {
		p.fail(w, err)
		return
	}
	now := p.cfg.Clock.Now()
	v := boardView(items, plans, now, r.URL.Query().Get("closed") == "1", p.cfg.SlackWorkspace)
	v.Draining = draining
	v.Freezes = fz
	v.Working = workingView(work, now, 0)
	v.Orch = orchView(orch, items)
	v.Runners = runnerView(runners, now)
	v.Regressions = regressionView(regs, p.principal(r).Kind == api.KindUser, now)
	v.Runway = runwayView(rw)
	w.Header().Set("Content-Security-Policy", liveCSP)
	if v.Regressions.CanRun {
		w.Header().Set("Content-Security-Policy", boardFormCSP)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := boardTmpl.Execute(w, v); err != nil {
		p.log.Error("render board", "err", err)
	}
}

func (p *boardPage) item(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	d, err := p.st.GetItem(r.Context(), id)
	if err != nil {
		p.fail(w, err)
		return
	}
	ch, ts, err := p.st.SlackThread(r.Context(), id)
	if err != nil {
		p.fail(w, err)
		return
	}
	now := p.cfg.Clock.Now()
	v := itemView(d, ch, ts, now, p.cfg.SlackWorkspace)
	if d.Lease != nil {
		work, err := p.st.Working(r.Context(), id)
		if err != nil {
			p.fail(w, err)
			return
		}
		ws := workingView(work, now, id)
		v.Working = &ws
		w.Header().Set("Content-Security-Policy", liveCSP)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := itemTmpl.Execute(w, v); err != nil {
		p.log.Error("render item", "err", err)
	}
}

// working is the "Working now" section's inside, for live.js: every live lease, or one item's
// (?item=N).
func (p *boardPage) working(w http.ResponseWriter, r *http.Request) {
	var id int64
	if q := r.URL.Query().Get("item"); q != "" {
		n, err := strconv.ParseInt(q, 10, 64)
		if err != nil || n <= 0 {
			http.NotFound(w, r)
			return
		}
		id = n
	}
	work, err := p.st.Working(r.Context(), id)
	if err != nil {
		p.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := workingTmpl.ExecuteTemplate(w, "working", workingView(work, p.cfg.Clock.Now(), id)); err != nil {
		p.log.Error("render working", "err", err)
	}
}

func (p *boardPage) liveJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Write(liveJS)
}

func (p *boardPage) plan(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	d, err := p.st.GetPlan(r.Context(), id)
	if err != nil {
		p.fail(w, err)
		return
	}
	// GetPlan lists the versions without their markdown: fetch the latest of each kind.
	latest := map[string]pipeline.PlanDoc{}
	for _, kind := range docKinds {
		if !slices.ContainsFunc(d.Docs, func(x pipeline.PlanDoc) bool { return x.Kind == kind }) {
			continue
		}
		if latest[kind], err = p.st.GetPlanDoc(r.Context(), id, kind, 0); err != nil {
			p.fail(w, err)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := planTmpl.Execute(w, planView(d, latest, p.cfg.Clock.Now(), p.cfg.SlackWorkspace)); err != nil {
		p.log.Error("render plan", "err", err)
	}
}

func (p *boardPage) planDoc(w http.ResponseWriter, r *http.Request) {
	id, err1 := strconv.ParseInt(r.PathValue("id"), 10, 64)
	v, err2 := strconv.Atoi(r.PathValue("v"))
	kind := r.PathValue("kind")
	if err1 != nil || err2 != nil || id <= 0 || v <= 0 || !slices.Contains(docKinds, kind) {
		http.NotFound(w, r)
		return
	}
	d, err := p.st.GetPlan(r.Context(), id)
	if err != nil {
		p.fail(w, err)
		return
	}
	doc, err := p.st.GetPlanDoc(r.Context(), id, kind, v)
	if err != nil {
		p.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := docTmpl.Execute(w, docView(d, doc)); err != nil {
		p.log.Error("render plan doc", "err", err)
	}
}

// imageTypes are shown inline (screenshots); every other file is a download. SVG is not among
// them: it can carry script.
var imageTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

func (p *boardPage) evidence(w http.ResponseWriter, r *http.Request) {
	id, err1 := strconv.ParseInt(r.PathValue("id"), 10, 64)
	n, err2 := strconv.Atoi(r.PathValue("n"))
	if err1 != nil || err2 != nil || id <= 0 || n <= 0 {
		http.NotFound(w, r)
		return
	}
	ev, f, err := p.st.OpenEvidence(r.Context(), id, n)
	if err != nil {
		p.fail(w, err)
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	if slices.Contains(imageTypes, ev.ContentType) {
		h.Set("Content-Type", ev.ContentType)
		h.Set("Content-Disposition", `inline; filename="`+ev.Name+`"`)
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", `attachment; filename="`+ev.Name+`"`)
	}
	http.ServeContent(w, r, "", ev.At, f)
}

// ---- views ----

type card struct {
	store.BoardItem
	RepoName string
	// Label is the display stage, when the column holds more than one ("ready", "land (queued)").
	Label    string
	PRURL    string
	SlackURL string
	// Reason is why it waits on the user: the question or the hold reason.
	Reason string
}

// planCard is an open plan in the Planning column.
type planCard struct {
	store.PlanRow
	RepoNames []string
	// Idle is the seconds since the plan's last activity.
	Idle int64
}

type column struct {
	Name  string
	Note  string
	Cards []card
	Plans []planCard
}

// Count is the column's cards, plans included.
func (c column) Count() int { return len(c.Cards) + len(c.Plans) }

// workingCard is a live lease's "Working now" card.
type workingCard struct {
	store.Working
	// Elapsed is the seconds since the claim.
	Elapsed int64
}

// workingSection is the "Working now" section: its cards, and where live.js refreshes it from.
type workingSection struct {
	Src   string
	Cards []workingCard
}

func workingView(work []store.Working, now time.Time, item int64) workingSection {
	v := workingSection{Src: "/board/working", Cards: []workingCard{}}
	if item > 0 {
		v.Src += "?item=" + strconv.FormatInt(item, 10)
	}
	for _, w := range work {
		el := int64(now.Sub(w.ClaimedAt) / time.Second)
		if el < 0 {
			el = 0
		}
		v.Cards = append(v.Cards, workingCard{Working: w, Elapsed: el})
	}
	return v
}

type boardData struct {
	// Draining are the runners that claim nothing new before a restart (loop-runner drain).
	Draining []store.RunnerState
	// Freezes are the repos frozen while their main is red: a red banner each.
	Freezes []store.Freeze
	Working workingSection
	Orch    orchSection
	Runners runnerSection
	// Regressions are the latest regression runs, and the user's button to start one.
	Regressions regressionSection
	// Runway is how much the loop can do without the user (#205); nil: not shown.
	Runway     *runwaySection
	Columns    []column
	ShowClosed bool
	Now        time.Time
	Total      int
	PlanCount  int
}

// runnerSection is the runner panel: the runners seen within a day and the slots held for their
// work (pipeline.SlotHolds).
type runnerSection struct {
	Runners []runnerRow
	Holds   []pipeline.SlotHold
}

type runnerRow struct {
	pipeline.RunnerSeen
	Ago    int64 // seconds since loopd last saw it
	Around bool  // seen within two of its polls
}

func runnerView(p store.RunnerPanel, now time.Time) runnerSection {
	v := runnerSection{Holds: p.Holds}
	for _, r := range p.Runners {
		v.Runners = append(v.Runners, runnerRow{RunnerSeen: r, Ago: max(int64(now.Sub(r.LastSeen)/time.Second), 0), Around: r.Around(now)})
	}
	return v
}

// orchLimit is how many of each list the Orchestrator panel shows.
const orchLimit = 15

// orchRow is an orchestrator event on its panel.
type orchRow struct {
	store.Event
	Title string
	// Action is what it did: the event's kind, its stage change, or the refused action.
	Action string
	// For is who it acted for, when the user acted through it ("": itself, or loopd).
	For string
	// Text is an escalation's question or a refusal's message; Recommendation an escalation's.
	Text, Recommendation, Reason string
}

type orchTicket struct {
	ID       int64
	Title    string
	Priority string
	Display  string
}

type orchNote struct {
	At   time.Time
	Text template.HTML
}

// orchSection is the board's Orchestrator panel (docs/phases/08-orchestrator.md, "Talking to it");
// Show is false until the orchestrator has done something.
type orchSection struct {
	Show bool
	// Last is its newest action or note. There is no record of its turns yet (piece 3).
	Last        time.Time
	Actions     []orchRow
	Escalations []orchRow
	Tickets     []orchTicket
	Notes       []orchNote
}

func orchView(o store.OrchBoard, items []store.BoardItem) orchSection {
	byID := map[int64]store.BoardItem{}
	for _, it := range items {
		byID[it.ID] = it
	}
	row := func(e store.Event) orchRow {
		r := orchRow{Event: e, Title: byID[e.ItemID].Title, Action: strings.TrimPrefix(e.Kind, "orchestrator."), Reason: str(e.Data, "reason"),
			Text: str(e.Data, "text"), Recommendation: str(e.Data, "recommendation")}
		if e.To != "" && e.To != e.From {
			r.Action += " (" + e.From + " → " + e.To + ")"
		}
		if e.Kind == "refused" {
			r.Action, r.Text = "refused "+str(e.Data, "action"), str(e.Data, "message")
		}
		if via := str(e.Data, "via"); via != "" && via != e.Actor {
			r.For = e.Actor
		}
		return r
	}
	var v orchSection
	for _, e := range o.Actions {
		v.Actions = append(v.Actions, row(e))
		if e.At.After(v.Last) {
			v.Last = e.At
		}
	}
	for _, e := range o.Escalations {
		v.Escalations = append(v.Escalations, row(e))
	}
	// Open ones first, then the rest, each newest first.
	var done []orchTicket
	for _, id := range o.Tickets {
		it, ok := byID[id]
		if !ok {
			continue
		}
		t := orchTicket{ID: id, Title: it.Title, Priority: it.Priority, Display: it.Display}
		if it.Stage == string(pipeline.Done) || it.Stage == string(pipeline.Closed) {
			done = append(done, t)
		} else {
			v.Tickets = append(v.Tickets, t)
		}
	}
	v.Tickets = append(v.Tickets, done...)
	for _, e := range o.Notes {
		// Model-written: through the allow-list renderer, never as HTML.
		v.Notes = append(v.Notes, orchNote{At: e.At, Text: markdown(str(e.Data, "text"))})
		if e.At.After(v.Last) {
			v.Last = e.At
		}
	}
	v.Show = len(v.Actions)+len(v.Escalations)+len(v.Tickets)+len(v.Notes) > 0
	return v
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// lanes maps the board's columns to display stages (pipeline.Display). Planning holds the open
// plans instead (planning is not an item stage). QA groups its cards (pipeline.QAGroup).
var lanes = []struct {
	name, note string
	displays   []string
}{
	{"Planning", "No open plans.", nil},
	{"Triage", "", []string{"intake", "triage"}},
	{"Build", "", []string{"ready", "build"}},
	{"Review", "", []string{"review (queued)", "review", "land (queued)", "land"}},
	{"Needs you", "", []string{"needs-you"}},
	{"Approval", "", []string{"held"}},
	{"QA", "Nothing waits for a check.", []string{"qa"}},
	{"Done", "", []string{"done", "closed"}},
}

func boardView(items []store.BoardItem, plans []store.PlanRow, now time.Time, showClosed bool, slackBase string) boardData {
	v := boardData{ShowClosed: showClosed, Now: now, PlanCount: len(plans), Working: workingView(nil, now, 0)}
	for _, l := range lanes {
		col := column{Name: l.name, Note: l.note, Cards: []card{}}
		if l.name == "Planning" {
			for _, pl := range plans {
				col.Plans = append(col.Plans, planCard{PlanRow: pl, RepoNames: repoNames(pl.Repos), Idle: int64(now.Sub(pl.UpdatedAt) / time.Second)})
			}
			if len(col.Plans) > 0 {
				col.Note = ""
			}
		}
		for _, it := range items {
			if !slices.Contains(l.displays, it.Display) {
				continue
			}
			switch it.Display {
			case "done":
				if now.Sub(it.UpdatedAt) > DoneWindow {
					continue
				}
			case "closed":
				if !showClosed {
					continue
				}
			}
			c := card{BoardItem: it, RepoName: repoName(it.Repo), PRURL: prURL(it.Repo, it.PR),
				SlackURL: slackURL(slackBase, it.SlackChannel, it.SlackTS), Reason: it.Question}
			if it.Display == "held" {
				c.Reason = it.HoldReason
			}
			if len(l.displays) > 1 {
				c.Label = it.Display
			}
			if it.Display == "qa" {
				c.Label, c.Reason = pipeline.QAGroup(it.Proof, it.AwaitingDeploy)
			}
			col.Cards = append(col.Cards, c)
		}
		if l.name == "QA" && len(col.Cards) > 0 {
			// In the column's groups: waiting for a proof, needs you, failed.
			slices.SortStableFunc(col.Cards, func(a, b card) int {
				return slices.Index(pipeline.QAGroups, a.Label) - slices.Index(pipeline.QAGroups, b.Label)
			})
			col.Note = ""
		}
		v.Total += len(col.Cards)
		v.Columns = append(v.Columns, col)
	}
	return v
}

func repoName(slug string) string {
	if _, name, ok := strings.Cut(slug, "/"); ok {
		return name
	}
	return slug
}

func repoNames(slugs []string) []string {
	var out []string
	for _, s := range slugs {
		out = append(out, repoName(s))
	}
	return out
}

func prURL(repo string, pr int) string {
	if repo == "" || pr <= 0 {
		return ""
	}
	return "https://github.com/" + repo + "/pull/" + strconv.Itoa(pr)
}

// slackURL is a thread's permalink: <workspace>/archives/<channel>/p<ts without the dot>.
func slackURL(base, channel, ts string) string {
	if base == "" || channel == "" || ts == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/archives/" + channel + "/p" + strings.ReplaceAll(ts, ".", "")
}

type evidenceRow struct {
	store.Evidence
	Image bool
}

type eventRow struct {
	store.Event
	DataText string
}

type itemData struct {
	Item      pipeline.Item
	Display   string
	ClaimedBy string
	RepoName  string
	PRURL     string
	SlackURL  string
	Reason    string
	Age       int64
	Events    []eventRow
	Evidence  []evidenceRow
	// Working is the item's "Working now" card while a lease is live (nil otherwise).
	Working *workingSection
}

// maxDataText caps an event's data on the page; the full record is in `loop show`.
const maxDataText = 400

func itemView(d store.Detail, channel, ts string, now time.Time, slackBase string) itemData {
	it := d.Item
	v := itemData{Item: it, Display: d.Display, RepoName: repoName(it.Repo), PRURL: prURL(it.Repo, it.PR),
		SlackURL: slackURL(slackBase, channel, ts), Reason: it.Question, Age: int64(now.Sub(it.CreatedAt) / time.Second)}
	if it.Stage == pipeline.Held {
		v.Reason = it.HoldReason
	}
	if it.Stage == pipeline.QA {
		g, why := pipeline.QAGroup(it.Proof, it.AwaitingDeploy)
		v.Reason = g + ": " + why
	}
	if d.Lease != nil {
		v.ClaimedBy = d.Lease.Runner + "/" + d.Lease.Agent
	}
	for _, e := range d.Events {
		row := eventRow{Event: e}
		if len(e.Data) > 0 {
			b, _ := json.Marshal(e.Data)
			row.DataText = string(b)
			if r := []rune(row.DataText); len(r) > maxDataText {
				row.DataText = string(r[:maxDataText]) + "…"
			}
		}
		v.Events = append(v.Events, row)
	}
	for _, ev := range d.Evidence {
		v.Evidence = append(v.Evidence, evidenceRow{Evidence: ev, Image: slices.Contains(imageTypes, ev.ContentType)})
	}
	return v
}

func size(n int64) string {
	switch {
	case n < 1<<10:
		return strconv.FormatInt(n, 10) + " B"
	case n < 1<<20:
		return strconv.FormatInt(n>>10, 10) + " KB"
	}
	return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

//go:embed templates/*.tmpl
var templateFS embed.FS

//go:embed templates/live.js
var liveJS []byte

var funcs = template.FuncMap{"join": strings.Join, "age": age, "size": size, "stamp": stamp, "turnLabel": turnLabel, "unservable": pipeline.UnservableRoot}

var (
	boardTmpl      = template.Must(template.New("board.tmpl").Funcs(funcs).ParseFS(templateFS, "templates/board.tmpl", "templates/style.tmpl", "templates/working.tmpl"))
	itemTmpl       = template.Must(template.New("item.tmpl").Funcs(funcs).ParseFS(templateFS, "templates/item.tmpl", "templates/style.tmpl", "templates/working.tmpl"))
	workingTmpl    = template.Must(template.New("working.tmpl").Funcs(funcs).ParseFS(templateFS, "templates/working.tmpl"))
	planTmpl       = template.Must(template.New("plan.tmpl").Funcs(funcs).ParseFS(templateFS, "templates/plan.tmpl", "templates/style.tmpl", "templates/doc.tmpl"))
	docTmpl        = template.Must(template.New("plandoc.tmpl").Funcs(funcs).ParseFS(templateFS, "templates/plandoc.tmpl", "templates/style.tmpl", "templates/doc.tmpl"))
	regressionTmpl = template.Must(template.New("regression.tmpl").Funcs(funcs).ParseFS(templateFS, "templates/regression.tmpl", "templates/style.tmpl"))
)

// runwaySection is the board's runway line: the hours, and the user's waiting items that free the
// most (the first boardRunwayTop).
type runwaySection struct {
	store.RunwayReport
	Low bool
}

const boardRunwayTop = 3

func runwayView(r store.RunwayReport) *runwaySection {
	r.Unblockers = r.Unblockers[:min(len(r.Unblockers), boardRunwayTop)]
	return &runwaySection{RunwayReport: r, Low: r.Threshold > 0 && r.Hours < float64(r.Threshold)}
}
