package web

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/api"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// Regression runs on the board (docs/decisions/2026-10-18-regression-trigger.md): the panel with
// the latest runs and, for the user's token only, the "Run regression" button (a form post, the
// board's only write); each run's report page and its files.

// regLimit is how many runs the panel lists.
const regLimit = 5

type regressionSection struct {
	Runs []regRow
	// CanRun: the board was opened with the user's token, which may start a run.
	CanRun bool
	Suites []string
	// Open is the queued or running run (no second one starts meanwhile).
	Open *regRow
}

// Show says the panel has something to show.
func (s regressionSection) Show() bool { return s.CanRun || len(s.Runs) > 0 }

type regRow struct {
	store.Regression
	Ago int64
}

func regressionView(rs []store.Regression, canRun bool, now time.Time) regressionSection {
	v := regressionSection{CanRun: canRun, Suites: pipeline.Suites}
	for _, r := range rs {
		row := regRow{Regression: r, Ago: max(int64(now.Sub(r.CreatedAt)/time.Second), 0)}
		v.Runs = append(v.Runs, row)
		if r.Status == store.RegQueued || r.Status == store.RegRunning {
			v.Open = &row
		}
	}
	return v
}

// principal is who the board's cookie signs in (auth has checked it).
func (p *boardPage) principal(r *http.Request) api.Principal {
	c, err := r.Cookie(BoardCookie)
	if err != nil {
		return api.Principal{}
	}
	pr, _ := p.tokens.Lookup(c.Value)
	return pr
}

// runRegression is the "Run regression" button: the user's token only (the approver's, as on
// Slack); a board token is refused.
func (p *boardPage) runRegression(w http.ResponseWriter, r *http.Request) {
	pr := p.principal(r)
	if pr.Kind != api.KindUser {
		http.Error(w, "only the user's token may start a regression run", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if _, err := p.st.QueueRegression(r.Context(), pr.Actor(), r.PostForm.Get("suite"), "board"); err != nil {
		if pe := (*pipeline.Error)(nil); errors.As(err, &pe) {
			status := http.StatusBadRequest
			if pe.Code == store.CodeRegressionOpen {
				status = http.StatusConflict
			}
			http.Error(w, pe.Message, status)
			return
		}
		p.fail(w, err)
		return
	}
	http.Redirect(w, r, "/board", http.StatusSeeOther)
}

type regressionPage struct {
	Run      store.Regression
	Failures []store.RegressionTest
	Files    []store.RegressionFile
	Now      time.Time
}

func (p *boardPage) regression(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	reg, err := p.st.GetRegression(r.Context(), id)
	if err != nil {
		p.fail(w, err)
		return
	}
	v := regressionPage{Run: reg, Files: reg.Files, Now: p.cfg.Clock.Now()}
	for _, t := range reg.Tests {
		if t.Failed() {
			v.Failures = append(v.Failures, t)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := regressionTmpl.Execute(w, v); err != nil {
		p.log.Error("render regression", "err", err)
	}
}

func (p *boardPage) regressionFile(w http.ResponseWriter, r *http.Request) {
	id, err1 := strconv.ParseInt(r.PathValue("id"), 10, 64)
	n, err2 := strconv.Atoi(r.PathValue("n"))
	if err1 != nil || err2 != nil || id <= 0 || n <= 0 {
		http.NotFound(w, r)
		return
	}
	f, file, err := p.st.OpenRegressionFile(r.Context(), id, n)
	if err != nil {
		p.fail(w, err)
		return
	}
	defer file.Close()
	h := w.Header()
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	if slices.Contains(imageTypes, f.ContentType) {
		h.Set("Content-Type", f.ContentType)
		h.Set("Content-Disposition", `inline; filename="`+f.Name+`"`)
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", `attachment; filename="`+f.Name+`"`)
	}
	http.ServeContent(w, r, "", f.At, file)
}
