package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/api"
	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

const (
	userTok   = "user-token-0123456789abcdef"
	runnerTok = "runner-token-0123456789abcdef"
)

func setup(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir(), clock.NewFake(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	toks, err := api.ParseTokens(strings.NewReader(userTok + "\tuser\talex\n" + runnerTok + "\trunner\tdevbox\n" + boardTok + "\tboard\tphone\n"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st, toks, slog.New(slog.DiscardHandler)))
	t.Cleanup(srv.Close)
	return srv, st
}

func send(t *testing.T, req *http.Request) (*http.Response, string) {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestBoardPage(t *testing.T) {
	srv, st := setup(t)
	ctx := context.Background()
	if _, err := st.AddItem(ctx, "user:alex", store.NewItem{Title: "wake word <misses>", Priority: "P1"}); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	resp, _ := send(t, req)
	if resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("no credentials: %d %v", resp.StatusCode, resp.Header)
	}
	req.SetBasicAuth("x", runnerTok)
	if resp, _ := send(t, req); resp.StatusCode != 403 {
		t.Fatalf("runner token on the page: %d", resp.StatusCode)
	}
	req.SetBasicAuth("x", boardTok)
	if resp, _ := send(t, req); resp.StatusCode != 403 {
		t.Fatalf("board token on the status page: %d", resp.StatusCode)
	}
	req.SetBasicAuth("alex", userTok)
	resp, body := send(t, req)
	if resp.StatusCode != 200 {
		t.Fatalf("board: %d %s", resp.StatusCode, body)
	}
	for _, want := range []string{"intake", "wake word &lt;misses&gt;", "P1", `type="range"`, `min="0"`, `max="10"`, `value="4"`} {
		if !strings.Contains(body, want) {
			t.Errorf("board lacks %q", want)
		}
	}
	if strings.Contains(body, "<script") {
		t.Error("the page should need no JavaScript")
	}
}

func TestSliderPost(t *testing.T) {
	srv, st := setup(t)
	post := func(form url.Values, hdr map[string]string) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+"/ui/control", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth("alex", userTok)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, _ := send(t, req)
		return resp
	}

	resp := post(url.Values{"active_agents": {"2"}}, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("slider: %d %v", resp.StatusCode, resp.Header)
	}
	if c, _ := st.Control(context.Background()); c.ActiveAgents != 2 {
		t.Fatalf("active agents %d, want 2", c.ActiveAgents)
	}
	post(url.Values{"active_agents": {"2"}, "action": {"stop"}}, nil)
	if c, _ := st.Control(context.Background()); !c.Stopped {
		t.Fatal("stop button did not stop")
	}
	post(url.Values{"active_agents": {"3"}, "action": {"resume"}}, nil)
	if c, _ := st.Control(context.Background()); c.Stopped || c.ActiveAgents != 3 {
		t.Fatalf("resume: %+v", c)
	}
	if resp := post(url.Values{"active_agents": {"11"}}, nil); resp.StatusCode != 400 {
		t.Fatalf("out of range: %d", resp.StatusCode)
	}
	// A cross-site form post is refused even with the browser's cached credentials.
	if resp := post(url.Values{"active_agents": {"6"}}, map[string]string{"Sec-Fetch-Site": "cross-site"}); resp.StatusCode != 403 {
		t.Fatalf("cross-site post: %d", resp.StatusCode)
	}
	if c, _ := st.Control(context.Background()); c.ActiveAgents != 3 {
		t.Fatalf("cross-site post changed control: %+v", c)
	}
}

// Every stage's board name has a column, so no item is left off the page (qa was missing once).
func TestOrderHasEveryStage(t *testing.T) {
	for _, s := range pipeline.AllStages {
		for _, claimed := range []bool{false, true} {
			name := pipeline.Display(s, claimed)
			if !slices.Contains(order, name) {
				t.Errorf("no column for %q", name)
			}
		}
	}
}
