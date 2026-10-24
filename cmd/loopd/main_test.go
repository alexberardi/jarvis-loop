package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/api"
	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/loopd"
	"github.com/alexberardi/jarvis-loop/internal/slack"
	"github.com/alexberardi/jarvis-loop/internal/store"
	"github.com/alexberardi/jarvis-loop/internal/web"
)

func TestCheckAddr(t *testing.T) {
	cases := []struct {
		addr string
		lan  bool
		ok   bool
	}{
		{"127.0.0.1:7780", false, true},
		{"localhost:7780", false, true},
		{"[::1]:7780", false, true},
		{":7780", false, false},
		{"0.0.0.0:7780", false, false},
		{"192.168.1.10:7780", false, false},
		{"192.168.1.10:7780", true, true},
		// What loopd listens on, on fly.io: all interfaces inside the machine, which the one TLS
		// port in front of it reaches (docs/decisions/2026-10-23-loopd-on-fly.md).
		{"0.0.0.0:7780", true, true},
		{":7780", true, true},
		{"nonsense", true, false},
	}
	for _, c := range cases {
		if err := checkAddr(c.addr, c.lan); (err == nil) != c.ok {
			t.Errorf("checkAddr(%q, %v) = %v", c.addr, c.lan, err)
		}
	}
}

// The page answers basic auth at /, the API bearer auth everywhere else.
func TestHandlerRouting(t *testing.T) {
	st, err := store.Open(t.TempDir(), clock.NewFake(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	toks, _ := api.ParseTokens(strings.NewReader("user-token-0123456789abcdef\tuser\talex\n"))
	srv := httptest.NewServer(handler(st, toks, slog.New(slog.DiscardHandler), web.BoardConfig{}, nil))
	defer srv.Close()
	for path, want := range map[string]string{"/": "Basic", "/items": "Bearer", "/control": "Bearer", "/board": "", "/board/items/1": ""} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), want) {
			t.Errorf("%s: %d %q, want 401 %s", path, resp.StatusCode, resp.Header.Get("WWW-Authenticate"), want)
		}
	}
	// /healthz is the only route with no token, and tells a reader nothing else
	// (docs/decisions/2026-10-23-loopd-on-fly.md). Anything but GET is 405 with no body.
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "ok\n" {
		t.Errorf("GET /healthz: %d %q", resp.StatusCode, body)
	}
	if ct, cc := resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"); !strings.HasPrefix(ct, "text/plain") || cc != "no-store" {
		t.Errorf("GET /healthz: Content-Type %q, Cache-Control %q", ct, cc)
	}
	resp, err = http.Post(srv.URL+"/healthz", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 405 || len(body) != 0 {
		t.Errorf("POST /healthz: %d %q", resp.StatusCode, body)
	}
}

// The board listener serves the board and nothing else, whatever the token.
func TestBoardHandlerRouting(t *testing.T) {
	st, err := store.Open(t.TempDir(), clock.NewFake(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const userTok, boardTok = "user-token-0123456789abcdef", "board-token-0123456789abcdef"
	toks, _ := api.ParseTokens(strings.NewReader(userTok + "\tuser\talex\n" + boardTok + "\tboard\tphone\n"))
	srv := httptest.NewServer(loopd.BoardHandler(st, toks, slog.New(slog.DiscardHandler), web.BoardConfig{}))
	defer srv.Close()
	do := func(method, path string) int {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+userTok)
		req.SetBasicAuth("alex", userTok)
		req.AddCookie(&http.Cookie{Name: web.BoardCookie, Value: boardTok})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, path := range []string{"/", "/items", "/items/1", "/control", "/claim", "/ui/control", "/healthz"} {
		for _, method := range []string{"GET", "POST", "PUT"} {
			if got := do(method, path); got != 404 && got != 405 {
				t.Errorf("%s %s on the board listener: %d", method, path, got)
			}
		}
	}
	if got := do("GET", "/board"); got != 200 {
		t.Errorf("GET /board: %d", got)
	}
	if got := do("POST", "/board"); got != 405 {
		t.Errorf("POST /board: %d", got)
	}
}

// loopd serves one listener (docs/decisions/2026-10-23-loopd-on-fly.md): the health check with no
// token, every other route behind one, and the two dropped flags refused. This replaces
// TestRunWithBoardAddr and TestRunWithRunnerAddr, which covered the --board-addr and --runner-addr
// listeners this change dropped.
func TestRunServesOneListener(t *testing.T) {
	dir := t.TempDir()
	tokens := filepath.Join(dir, "tokens")
	os.WriteFile(tokens, []byte("user-token-0123456789abcdef\tuser\talex\n"), 0o600)
	// A port the test can reach while run serves it.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()
	args := []string{"--addr", addr, "--data", filepath.Join(dir, "data"), "--tokens", tokens,
		"--slack-workspace", "https://jarvis.slack.com"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr syncWriter
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, &stderr) }()

	base := "http://" + addr
	var status int
	var body string
	for range 200 {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			status, body = resp.StatusCode, string(b)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The health check is the only route with no token, and says nothing but that loopd serves.
	if status != 200 || body != "ok\n" {
		t.Fatalf("GET /healthz: %d %q; log:\n%s", status, body, stderr.String())
	}
	// Everything else on the same listener still needs one: the API, the board and the page.
	for path, want := range map[string]string{"/items": "Bearer", "/board": "", "/": "Basic"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), want) {
			t.Errorf("%s: %d %q, want 401 %s", path, resp.StatusCode, resp.Header.Get("WWW-Authenticate"), want)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v\n%s", err, stderr.String())
	}

	// The dropped flags are gone, and a bad --slack-workspace is still refused.
	stopped, stop := context.WithCancel(context.Background())
	stop()
	for _, bad := range [][]string{{"--board-addr", "127.0.0.1:0"}, {"--runner-addr", "127.0.0.1:0"},
		{"--slack-workspace", "jarvis.slack.com"}, {"--slack-workspace", "http://jarvis.slack.com"}} {
		if err := run(stopped, append(args[:len(args):len(args)], bad...), io.Discard); err == nil {
			t.Errorf("run accepted %v", bad)
		}
	}
}

// syncWriter is run's log where a test reads it while run writes it.
type syncWriter struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// loopd.RunnerHandler serves the API and nothing else: no status page, no board. loopd itself no
// longer puts it on a listener of its own, but the loop's harness still wires it (scenario 63).
func TestRunnerHandlerRouting(t *testing.T) {
	st, err := store.Open(t.TempDir(), clock.NewFake(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const runnerTok = "runner-token-0123456789abcdef"
	toks, _ := api.ParseTokens(strings.NewReader(runnerTok + "\trunner\tmbp\n"))
	srv := httptest.NewServer(loopd.RunnerHandler(st, toks, slog.New(slog.DiscardHandler), nil))
	defer srv.Close()
	for path, want := range map[string]int{"/": 404, "/board": 404, "/ui/control": 404, "/items": 200, "/control": 200} {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+runnerTok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s on the runner listener: %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func TestSlackEnvMustBePrivate(t *testing.T) {
	dir := t.TempDir()
	tokens := filepath.Join(dir, "tokens")
	os.WriteFile(tokens, []byte("user-token-0123456789abcdef\tuser\talex\n"), 0o600)
	env := filepath.Join(dir, "slack.env")
	os.WriteFile(env, []byte("SLACK_BOT_TOKEN=xoxb-secret-bot\nSLACK_APP_TOKEN=xapp-secret-app\n"), 0o600)
	os.Chmod(env, 0o644)
	var stderr strings.Builder
	err := run(context.Background(), []string{"--addr", "127.0.0.1:0", "--data", filepath.Join(dir, "data"), "--tokens", tokens, "--slack-env", env}, &stderr)
	if err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error()+stderr.String(), "secret") {
		t.Fatal("a token was printed")
	}
}

// --repos are the slugs triage may give an item and the Slack add words may name.
func TestParseRepos(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		ok   bool
	}{
		{"", nil, true},
		{"alexberardi/jarvis-loop", []string{"alexberardi/jarvis-loop"}, true},
		{" alexberardi/jarvis-loop , alexberardi/jarvis-server,alexberardi/jarvis-loop ", []string{"alexberardi/jarvis-loop", "alexberardi/jarvis-server"}, true},
		{"jarvis-loop", nil, false},
		{"a/b/c", nil, false},
		{"alexberardi/jarvis loop", nil, false},
		{"alexberardi/", nil, false},
		{"/jarvis-loop", nil, false},
		{"alexberardi/jarvis-loop,", []string{"alexberardi/jarvis-loop"}, true},
	}
	for _, c := range cases {
		got, err := parseRepos(c.in)
		if (err == nil) != c.ok || strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("parseRepos(%q) = %v, %v; want %v (ok %v)", c.in, got, err, c.want, c.ok)
		}
	}
}

// The @orchestrator app's tokens: orchestrator-slack.env next to --slack-env's file by default,
// off when that file is absent; an explicit file must exist, be 0600 and hold another app's tokens.
func TestOrchestratorSlackEnv(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o600)
		os.Chmod(p, mode)
		return p
	}
	loopEnv := write("slack.env", "SLACK_BOT_TOKEN=xoxb-loop\nSLACK_APP_TOKEN=xapp-loop\n", 0o600)
	if tok, err := orchestratorTokens(loopEnv, "", slack.Tokens{Bot: "xoxb-loop", App: "xapp-loop"}); err != nil || tok != nil {
		t.Fatalf("absent default: %v %v", tok, err)
	}
	if tok, err := orchestratorTokens("", "", slack.Tokens{}); err != nil || tok != nil {
		t.Fatalf("no Slack: %v %v", tok, err)
	}
	write("orchestrator-slack.env", "SLACK_BOT_TOKEN=xoxb-orch\nSLACK_APP_TOKEN=xapp-orch\n", 0o600)
	loop := slack.Tokens{Bot: "xoxb-loop", App: "xapp-loop"}
	tok, err := orchestratorTokens(loopEnv, "", loop)
	if err != nil || tok == nil || tok.Bot != "xoxb-orch" || tok.App != "xapp-orch" {
		t.Fatalf("default next to slack.env: %v", err)
	}
	if tok, err := orchestratorTokens(loopEnv, "off", loop); err != nil || tok != nil {
		t.Fatalf("off: %v %v", tok, err)
	}
	if _, err := orchestratorTokens(loopEnv, filepath.Join(dir, "missing.env"), loop); err == nil {
		t.Fatal("an explicit missing file was accepted")
	}
	if _, err := orchestratorTokens("", filepath.Join(dir, "orchestrator-slack.env"), slack.Tokens{}); err == nil {
		t.Fatal("accepted without --slack-env")
	}
	same := write("same.env", "SLACK_BOT_TOKEN=xoxb-loop\nSLACK_APP_TOKEN=xapp-other\n", 0o600)
	if _, err := orchestratorTokens(loopEnv, same, loop); err == nil || strings.Contains(err.Error(), "xoxb-loop") {
		t.Fatalf("the loop bot's token twice: %v", err)
	}
	open := write("open.env", "SLACK_BOT_TOKEN=xoxb-secret\nSLACK_APP_TOKEN=xapp-secret\n", 0o644)
	if _, err := orchestratorTokens(loopEnv, open, loop); err == nil || !strings.Contains(err.Error(), "0600") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("mode 0644: %v", err)
	}
}
