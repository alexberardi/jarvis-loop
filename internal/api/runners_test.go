package api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

const boardTok = "board-token-0123456789abcdef"

// newRunnersEnv is newEnv on NewRunners, the API loopd serves to remote runners on the LAN.
func newRunnersEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	st, err := store.Open(t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	toks, err := ParseTokens(strings.NewReader(fmt.Sprintf("%s\tuser\talex\n%s\trunner\tdevbox\n%s\trunner\tmac\n%s\torchestrator\tloop\n%s\tboard\twall\n",
		userTok, runnerTok, otherTok, orchTok, boardTok)))
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	srv := httptest.NewServer(NewRunners(st, toks, slog.New(slog.NewJSONHandler(logs, nil))))
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, clock: clk, store: st, logs: logs}
}

// The remote runners' listener takes runner tokens only: a user, board or orchestrator token is
// refused on every route, its own included, and so are no token and an unknown one. A runner
// works there as on the main listener.
func TestRunnersListenerTakesRunnerTokensOnly(t *testing.T) {
	e := newRunnersEnv(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/items"}, {"POST", "/items"}, {"POST", "/claim"}, {"PUT", "/control"}, {"POST", "/items/1/approve"},
		{"POST", "/orchestrator/items"}, {"GET", "/orchestrator/snapshot"}, {"GET", "/"}, {"GET", "/board"}, {"GET", "/nowhere"},
	} {
		for _, tok := range []string{userTok, orchTok, boardTok} {
			e.refused(tok, c.method, c.path, map[string]any{}, 403, "runners_only")
		}
		e.refused("", c.method, c.path, map[string]any{}, 401, "unauthorized")
		e.refused("not-a-token-0123456789abc", c.method, c.path, map[string]any{}, 401, "unauthorized")
	}
	// Acting for the user is the orchestrator's, and refused here before anything else.
	e.refused(userTok, "POST", "/items/1/answer", map[string]any{"answer": "x"}, 403, "runners_only")

	e.do(runnerTok, "GET", "/items", nil, 200, nil)
	e.do(otherTok, "POST", "/claim", map[string]any{"agent": "mac-1", "capabilities": []string{"macos"}}, 204, nil)
	// only_resources (the Mac's claim scope) must be among the capabilities offered.
	e.do(otherTok, "POST", "/claim", map[string]any{"agent": "mac-1", "capabilities": []string{"macos"}, "only_resources": []string{"macos"}, "poll_seconds": 15}, 204, nil)
	// loopd keeps what the runner claims with, for the held slots (#192).
	if p, err := e.store.Runners(context.Background()); err != nil || len(p.Runners) != 1 || !slices.Equal(p.Runners[0].OnlyResources, []string{"macos"}) || p.Runners[0].PollSeconds != 15 {
		t.Fatalf("runners %+v %v", p, err)
	}
	e.refused(otherTok, "POST", "/claim", map[string]any{"agent": "mac-1", "capabilities": []string{"gpu"}, "only_resources": []string{"macos"}}, 400, "invalid_field")
	// A runner's own rules hold as on the main listener.
	e.refused(runnerTok, "POST", "/claim", map[string]any{"runner": "mac", "agent": "a"}, 403, "runner_mismatch")
	e.refused(runnerTok, "POST", "/items/1/approve", map[string]any{}, 403, "forbidden")
	if !strings.Contains(e.logs.String(), `"actor":"user:alex"`) || !strings.Contains(e.logs.String(), `"status":403`) {
		t.Fatalf("refusals are not logged with the actor:\n%s", e.logs.String())
	}
	for _, tok := range []string{userTok, runnerTok, otherTok, orchTok, boardTok} {
		if strings.Contains(e.logs.String(), tok) {
			t.Fatal("a token in the log")
		}
	}
}
