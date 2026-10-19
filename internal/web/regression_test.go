package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

func postForm(t *testing.T, u string, cookie *http.Cookie, form url.Values) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.AddCookie(cookie)
	return send(t, req)
}

func TestBoardRunRegression(t *testing.T) {
	srv, st := boardSetup(t)
	ctx := context.Background()
	user := &http.Cookie{Name: "loop_board", Value: userTok}
	board := &http.Cookie{Name: "loop_board", Value: boardTok}

	// The board token sees no panel and may not start a run.
	if _, page := get(t, srv.URL+"/board", board); strings.Contains(page, "Run regression") {
		t.Fatalf("the board token got the button")
	}
	if resp, _ := postForm(t, srv.URL+"/board/regressions", board, url.Values{"suite": {"web"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("board token post: %d", resp.StatusCode)
	}
	// The user's token gets the form (and a CSP that lets it post).
	resp, page := get(t, srv.URL+"/board", user)
	if !strings.Contains(page, `<form method="post" action="/board/regressions">`) || !strings.Contains(page, `<option value="all" selected>all</option>`) ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "form-action 'self'") {
		t.Fatalf("no form for the user (CSP %q):\n%s", resp.Header.Get("Content-Security-Policy"), page)
	}
	// A cross-site post is refused.
	req, _ := http.NewRequest("POST", srv.URL+"/board/regressions", strings.NewReader("suite=web"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(user)
	if resp, _ := send(t, req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site post: %d", resp.StatusCode)
	}
	if resp, _ := postForm(t, srv.URL+"/board/regressions", user, url.Values{"suite": {"web"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("user post: %d", resp.StatusCode)
	}
	rs, _ := st.Regressions(ctx, 5)
	if len(rs) != 1 || rs[0].Suite != "web" || rs[0].Via != "board" || rs[0].RequestedBy != "user:alex" {
		t.Fatalf("runs %+v", rs)
	}
	if resp, body := postForm(t, srv.URL+"/board/regressions", user, url.Values{"suite": {"all"}}); resp.StatusCode != http.StatusConflict || !strings.Contains(body, "one runs at a time") {
		t.Fatalf("second post: %d %s", resp.StatusCode, body)
	}
	if _, page := get(t, srv.URL+"/board", user); !strings.Contains(page, "One run at a time: R1 is queued.") || strings.Contains(page, `action="/board/regressions">`) {
		t.Fatalf("the open run:\n%s", page)
	}

	// The run ends: the panel links its report, which shows the failure and its screenshot.
	c, err := st.Claim(ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "a", Kinds: []string{pipeline.WorkRegression}})
	if err != nil || c == nil {
		t.Fatal(c, err)
	}
	if _, err := st.AddRegressionFile(ctx, "devbox", c.Reg.ID, "2-shot.png", strings.NewReader("\x89PNG\r\n\x1a\nimg")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegressionResult(ctx, "devbox", c.Reg.ID, store.RegressionReport{Tests: []store.RegressionReportTest{
		{Suite: "web", Title: "inbox › deletes", Status: "passed", MS: 5},
		{Suite: "web", Title: "login › <b>bad</b> password", File: "tests/login.spec.ts", Line: 9, Status: "failed", Error: "expected Wrong password", Screenshot: "2-shot.png"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, page := get(t, srv.URL+"/board", board); !strings.Contains(page, `<a href="/board/regressions/1">R1</a> web <span class="label">failed</span> 1 passed, 1 failed, 0 skipped`) {
		t.Fatalf("the panel:\n%s", page)
	}
	_, page = get(t, srv.URL+"/board/regressions/1", board)
	for _, want := range []string{"Regression R1 (web): failed", "<h3>web: login › &lt;b&gt;bad&lt;/b&gt; password</h3>", "tests/login.spec.ts:9, failed",
		"<pre>expected Wrong password</pre>", `<img src="/board/regressions/1/files/1"`, "<td>inbox › deletes</td>"} {
		if !strings.Contains(page, want) {
			t.Errorf("report lacks %q:\n%s", want, page)
		}
	}
	resp, body := get(t, srv.URL+"/board/regressions/1/files/1", board)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || !strings.HasSuffix(body, "img") {
		t.Fatalf("file: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp, _ := get(t, srv.URL+"/board/regressions/9", board); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing run: %d", resp.StatusCode)
	}
}

func TestBoardTonight(t *testing.T) {
	srv, st := boardSetup(t)
	ctx := context.Background()
	user := &http.Cookie{Name: "loop_board", Value: userTok}
	board := &http.Cookie{Name: "loop_board", Value: boardTok}
	if resp, _ := postForm(t, srv.URL+"/board/regressions/tonight", board, url.Values{"on": {"1"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("board token: %d", resp.StatusCode)
	}
	if _, page := get(t, srv.URL+"/board", user); !strings.Contains(page, `<input type="hidden" name="on" value="1"><button type="submit">Run overnight tonight</button>`) {
		t.Fatalf("no toggle:\n%s", page)
	}
	if resp, _ := postForm(t, srv.URL+"/board/regressions/tonight", user, url.Values{"on": {"1"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("schedule: %d", resp.StatusCode)
	}
	r, _ := st.ScheduledRegression(ctx)
	if r == nil || r.Via != "board" || r.RequestedBy != "user:alex" {
		t.Fatalf("scheduled %+v", r)
	}
	if resp, _ := postForm(t, srv.URL+"/board/regressions/tonight", user, url.Values{"on": {"1"}}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("second schedule: %d", resp.StatusCode)
	}
	_, page := get(t, srv.URL+"/board", user)
	if !strings.Contains(page, "Cancel tonight's run") || !strings.Contains(page, "Overnight R1 scheduled: from 01:00 ") {
		t.Fatalf("scheduled panel:\n%s", page)
	}
	if _, page := get(t, srv.URL+"/board", board); !strings.Contains(page, "Overnight R1 scheduled") || strings.Contains(page, "Cancel tonight") {
		t.Fatalf("the board token's panel:\n%s", page)
	}
	if resp, _ := postForm(t, srv.URL+"/board/regressions/tonight", user, url.Values{"on": {"0"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("cancel: %d", resp.StatusCode)
	}
	if r, _ := st.ScheduledRegression(ctx); r != nil {
		t.Fatalf("still scheduled: %+v", r)
	}
	if resp, _ := postForm(t, srv.URL+"/board/regressions/tonight", user, url.Values{"on": {"0"}}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cancel with none: %d", resp.StatusCode)
	}
}
