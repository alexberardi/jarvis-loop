// Package web is loopd's status page: the board grouped by display stage and the active-agents
// slider, server-rendered with no JavaScript. The browser signs in with HTTP basic auth, the user
// token as the password (docs/decisions/2026-10-14-web-page-auth.md).
package web

import (
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/api"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

type page struct {
	st     *store.Store
	tokens *api.Tokens
	log    *slog.Logger
}

// New returns the handler for GET / and POST /ui/control.
func New(st *store.Store, tokens *api.Tokens, log *slog.Logger) http.Handler {
	p := &page{st: st, tokens: tokens, log: log}
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", p.auth(p.board))
	// Basic-auth credentials are sent by the browser on any request, including a form post from
	// another site, so state changes are guarded against cross-origin requests.
	mux.Handle("POST /ui/control", http.NewCrossOriginProtection().Handler(p.auth(p.control)))
	return mux
}

func (p *page) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, tok, ok := r.BasicAuth()
		pr, found := p.tokens.Lookup(tok)
		if !ok || !found {
			w.Header().Set("WWW-Authenticate", `Basic realm="loopd", charset="UTF-8"`)
			http.Error(w, "sign in with your loopd user token as the password", http.StatusUnauthorized)
			return
		}
		api.SetActor(w, pr)
		if pr.Kind != api.KindUser {
			http.Error(w, "the page is for the user token", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
		next(w, r)
	})
}

// order is the board's column order: what needs the user first, then work in flight, then history.
var order = []string{
	"needs-you", "held", "qa", "land", "land (queued)", "review", "review (queued)",
	"build", "ready", "triage", "intake", "done", "closed",
}

type group struct {
	Name  string
	Items []store.BoardRow
}

func (p *page) board(w http.ResponseWriter, r *http.Request) {
	if err := p.st.Sweep(r.Context()); err != nil {
		p.fail(w, err)
		return
	}
	rows, err := p.st.ListItems(r.Context(), "", "")
	if err != nil {
		p.fail(w, err)
		return
	}
	ctl, err := p.st.Control(r.Context())
	if err != nil {
		p.fail(w, err)
		return
	}
	by := map[string][]store.BoardRow{}
	for _, row := range rows {
		by[row.Display] = append(by[row.Display], row)
	}
	var groups []group
	for _, name := range order {
		if len(by[name]) > 0 {
			groups = append(groups, group{Name: name, Items: by[name]})
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err = tmpl.Execute(w, map[string]any{"Groups": groups, "Control": ctl, "Max": pipeline.MaxActiveAgents, "Total": len(rows)})
	if err != nil {
		p.log.Error("render board", "err", err)
	}
}

func (p *page) control(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var patch store.ControlPatch
	if v := r.PostForm.Get("active_agents"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			http.Error(w, "active_agents must be a number", http.StatusBadRequest)
			return
		}
		patch.ActiveAgents = &n
	}
	switch r.PostForm.Get("action") {
	case "stop":
		t := true
		patch.Stopped = &t
	case "resume":
		f := false
		patch.Stopped = &f
	}
	_, tok, _ := r.BasicAuth()
	pr, _ := p.tokens.Lookup(tok)
	if _, err := p.st.SetControl(r.Context(), pr.Actor(), patch); err != nil {
		if pe := (*pipeline.Error)(nil); errors.As(err, &pe) {
			http.Error(w, pe.Message, http.StatusBadRequest)
			return
		}
		p.fail(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (p *page) fail(w http.ResponseWriter, err error) {
	p.log.Error("web", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func age(secs int64) string {
	d := time.Duration(secs) * time.Second
	switch {
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	}
	return strconv.Itoa(int(d.Hours()/24)) + "d"
}

var tmpl = template.Must(template.New("board").Funcs(template.FuncMap{"age": age}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Jarvis Loop</title>
<style>
:root { --bg: #fafaf9; --fg: #1c1917; --muted: #78716c; --line: #e7e5e4; --card: #fff; --accent: #2563eb; --warn: #b45309; }
@media (prefers-color-scheme: dark) {
  :root { --bg: #1c1917; --fg: #f5f5f4; --muted: #a8a29e; --line: #44403c; --card: #292524; --accent: #60a5fa; --warn: #fbbf24; }
}
* { box-sizing: border-box; }
body { margin: 0; padding: 16px; background: var(--bg); color: var(--fg); font: 15px/1.45 system-ui, sans-serif; }
main { max-width: 960px; margin: 0 auto; }
h1 { font-size: 20px; margin: 0 0 12px; }
h2 { font-size: 14px; text-transform: uppercase; letter-spacing: .04em; color: var(--muted); margin: 20px 0 6px; }
form.control { background: var(--card); border: 1px solid var(--line); border-radius: 8px; padding: 12px; display: flex; flex-wrap: wrap; gap: 12px; align-items: center; }
input[type=range] { width: 200px; max-width: 100%; }
button { font: inherit; padding: 4px 12px; border-radius: 6px; border: 1px solid var(--line); background: var(--bg); color: var(--fg); cursor: pointer; }
.stopped { color: var(--warn); font-weight: 600; }
table { width: 100%; border-collapse: collapse; background: var(--card); border: 1px solid var(--line); border-radius: 8px; overflow: hidden; }
td { padding: 6px 10px; border-top: 1px solid var(--line); vertical-align: top; }
tr:first-child td { border-top: 0; }
td.id, td.pri, td.age { white-space: nowrap; color: var(--muted); width: 1%; }
td.who { color: var(--muted); font-size: 13px; }
.empty { color: var(--muted); }
</style>
</head>
<body>
<main>
<h1>Jarvis Loop</h1>
<form class="control" method="post" action="/ui/control">
  <label for="active_agents">Active agents</label>
  <input type="range" id="active_agents" name="active_agents" min="0" max="{{.Max}}" step="1" value="{{.Control.ActiveAgents}}">
  <output>{{.Control.ActiveAgents}} of {{.Max}}</output>
  <button type="submit" name="action" value="set">Set</button>
  {{if .Control.Stopped}}<span class="stopped">Stopped</span> <button type="submit" name="action" value="resume">Resume</button>
  {{else}}<button type="submit" name="action" value="stop">Stop</button>{{end}}
</form>
{{if not .Groups}}<p class="empty">No items.</p>{{end}}
{{range .Groups}}
<h2>{{.Name}} · {{len .Items}}</h2>
<table>
{{range .Items}}<tr><td class="id">#{{.ID}}</td><td class="pri">{{.Priority}}</td><td>{{.Title}}{{if .ClaimedBy}}<div class="who">{{.ClaimedBy}}</div>{{end}}</td><td class="age">{{age .AgeSeconds}}</td></tr>
{{end}}</table>
{{end}}
</main>
</body>
</html>
`))
