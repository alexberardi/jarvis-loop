package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/build"
	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// testBuild is what the test API reports at GET /version.
var testBuild = build.New(strings.Repeat("a", 40), "")

// GET /version is loopd's own build, which the runner reads instead of taking its own build for
// the loop's (docs/decisions/2026-10-23-loopd-on-fly.md). It is a read: the user, a runner and the
// orchestrator may ask, the board may not, and no token is unauthorized.
func TestVersion(t *testing.T) {
	e := newEnv(t)
	var got build.Info
	e.do(userTok, "GET", "/version", nil, 200, &got)
	if got.Revision != strings.Repeat("a", 40) || got.Why != "" {
		t.Errorf("user: %+v", got)
	}
	e.do(runnerTok, "GET", "/version", nil, 200, &got)
	if got.Revision != strings.Repeat("a", 40) {
		t.Errorf("runner: %+v", got)
	}
	e.do(orchTok, "GET", "/version", nil, 200, &got)
	if got.Revision != strings.Repeat("a", 40) {
		t.Errorf("orchestrator: %+v", got)
	}
	// No token is unauthorized; a board token is refused here as everywhere
	// (TestBoardTokenRefusedByAPI covers it).
	e.do("", "GET", "/version", nil, 401, nil)
}

// A loopd with no vcs stamp says so rather than reporting a revision it does not have: the runner
// then reports nothing and the user falls back to /loop verified.
func TestVersionWithoutAStamp(t *testing.T) {
	clk := clock.NewFake(t0)
	st, err := store.Open(t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	toks, err := ParseTokens(strings.NewReader(userTok + "\tuser\talex\n"))
	if err != nil {
		t.Fatal(err)
	}
	for name, h := range map[string]*build.Build{
		"no stamp": build.New("", "the binary has no vcs.revision stamp"),
		"no build": nil,
	} {
		srv := httptest.NewServer(New(st, toks, slog.New(slog.DiscardHandler), h))
		req, _ := http.NewRequest("GET", srv.URL+"/version", nil)
		req.Header.Set("Authorization", "Bearer "+userTok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var got build.Info
		err = json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		srv.Close()
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if got.Revision != "" || got.Why == "" {
			t.Errorf("%s: %+v, want no revision and a reason", name, got)
		}
	}
}
