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

// The swim-lane board (docs/decisions/2026-10-15-board.md): read-only, server-rendered, no
// JavaScript. It signs in with a board (or user) token passed once as ?t=, kept in a cookie.

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

// NewBoard returns the handler for GET /board, GET /board/items/{id} and
// GET /board/items/{id}/evidence/{n}, and nothing else.
func NewBoard(st *store.Store, tokens *api.Tokens, log *slog.Logger, cfg BoardConfig) http.Handler {
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	p := &boardPage{st: st, tokens: tokens, log: log, cfg: cfg}
	mux := http.NewServeMux()
	mux.Handle("GET /board", p.auth(p.board))
	mux.Handle("GET /board/items/{id}", p.auth(p.item))
	mux.Handle("GET /board/items/{id}/evidence/{n}", p.auth(p.evidence))
	return mux
}

// boardKinds may read the board: its own read-only kind, and the user, who may do more anyway.
var boardKinds = []string{api.KindBoard, api.KindUser}

func (p *boardPage) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; form-action 'none'; frame-ancestors 'none'")
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
	v := boardView(items, p.cfg.Clock.Now(), r.URL.Query().Get("closed") == "1", p.cfg.SlackWorkspace)
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := itemTmpl.Execute(w, itemView(d, ch, ts, p.cfg.Clock.Now(), p.cfg.SlackWorkspace)); err != nil {
		p.log.Error("render item", "err", err)
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

type column struct {
	Name  string
	Note  string
	Cards []card
}

type boardData struct {
	Columns    []column
	ShowClosed bool
	Now        time.Time
	Total      int
}

// lanes maps the board's columns to display stages (pipeline.Display). Planning and QA have no
// stage yet.
var lanes = []struct {
	name, note string
	displays   []string
}{
	{"Planning", "No planning stages yet.", nil},
	{"Triage", "", []string{"intake", "triage"}},
	{"Build", "", []string{"ready", "build"}},
	{"Review", "", []string{"review (queued)", "review", "land (queued)", "land"}},
	{"Needs you", "", []string{"needs-you"}},
	{"Approval", "", []string{"held"}},
	{"QA", "No QA stage yet.", nil},
	{"Done", "", []string{"done", "closed"}},
}

func boardView(items []store.BoardItem, now time.Time, showClosed bool, slackBase string) boardData {
	v := boardData{ShowClosed: showClosed, Now: now}
	for _, l := range lanes {
		col := column{Name: l.name, Note: l.note, Cards: []card{}}
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
			col.Cards = append(col.Cards, c)
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

var funcs = template.FuncMap{"age": age, "size": size, "stamp": stamp}

var (
	boardTmpl = template.Must(template.New("board.tmpl").Funcs(funcs).ParseFS(templateFS, "templates/board.tmpl", "templates/style.tmpl"))
	itemTmpl  = template.Must(template.New("item.tmpl").Funcs(funcs).ParseFS(templateFS, "templates/item.tmpl", "templates/style.tmpl"))
)
