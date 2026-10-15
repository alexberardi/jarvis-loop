package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/api"
	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/store"
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
	srv := httptest.NewServer(handler(st, toks, slog.New(slog.DiscardHandler)))
	defer srv.Close()
	for path, want := range map[string]string{"/": "Basic", "/items": "Bearer", "/control": "Bearer"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), want) {
			t.Errorf("%s: %d %q, want 401 %s", path, resp.StatusCode, resp.Header.Get("WWW-Authenticate"), want)
		}
	}
}
