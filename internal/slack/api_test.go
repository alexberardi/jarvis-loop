package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alexberardi/jarvis-loop/internal/clock"
)

const testBotToken = "xoxb-test-not-a-real-token"

// sleeper is an injected Sleep that records the waits and moves a fake clock instead of sleeping.
type sleeper struct {
	mu    sync.Mutex
	clk   *clock.Fake
	waits []time.Duration
}

func (s *sleeper) Sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	s.mu.Unlock()
	if s.clk != nil {
		s.clk.Advance(d)
	}
	return ctx.Err()
}

func (s *sleeper) Waits() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration{}, s.waits...)
}

func TestAPIPostsJSONWithBearerAndHandlesOKFalse(t *testing.T) {
	var gotAuth, gotType string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&gotBody)
		switch r.URL.Path {
		case "/api/chat.postMessage":
			io.WriteString(w, `{"ok":true,"ts":"123.456","channel":"C0C9G6Y87HA"}`)
		case "/api/chat.update":
			io.WriteString(w, `{"ok":false,"error":"message_not_found"}`)
		case "/api/pins.add":
			io.WriteString(w, `{"ok":false,"error":"already_pinned"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	api := NewAPI(srv.URL+"/api/", testBotToken, Channels{ChannelLoop, ChannelDigest})

	ts, err := api.PostMessage(context.Background(), Message{Channel: ChannelLoop, Text: strings.Repeat("x", 2500), ThreadTS: "1.0"})
	if err != nil || ts != "123.456" {
		t.Fatalf("post: %q %v", ts, err)
	}
	if gotAuth != "Bearer "+testBotToken || !strings.HasPrefix(gotType, "application/json") {
		t.Fatalf("headers %q %q", gotAuth, gotType)
	}
	if gotBody["channel"] != ChannelLoop || gotBody["thread_ts"] != "1.0" || utf8.RuneCountInString(gotBody["text"].(string)) > MaxChars {
		t.Fatalf("body %v", gotBody)
	}

	err = api.Update(context.Background(), Message{Channel: ChannelLoop, TS: "123.456", Text: "x"})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "message_not_found" || ae.Method != "chat.update" {
		t.Fatalf("update: %v", err)
	}
	if strings.Contains(err.Error(), testBotToken) {
		t.Fatal("an error message contains the token")
	}
	// Pinning a pinned message is not an error.
	if err := api.PinsAdd(context.Background(), ChannelDigest, "123.456"); err != nil {
		t.Fatalf("already pinned: %v", err)
	}
}

func TestAPIRefusesOtherChannels(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `{"ok":true,"ts":"1.1"}`)
	}))
	defer srv.Close()
	api := NewAPI(srv.URL+"/api/", testBotToken, Channels{ChannelLoop, ChannelDigest})
	ctx := context.Background()
	if _, err := api.PostMessage(ctx, Message{Channel: "CGENERAL", Text: "hi"}); !errors.Is(err, ErrChannelNotAllowed) {
		t.Fatalf("post elsewhere: %v", err)
	}
	if err := api.PostEphemeral(ctx, Message{Channel: "D123", User: "U1", Text: "hi"}); !errors.Is(err, ErrChannelNotAllowed) {
		t.Fatalf("ephemeral elsewhere: %v", err)
	}
	if err := api.PinsAdd(ctx, "CGENERAL", "1.1"); !errors.Is(err, ErrChannelNotAllowed) {
		t.Fatalf("pin elsewhere: %v", err)
	}
	if calls != 0 {
		t.Fatalf("%d requests reached Slack", calls)
	}
}

func TestAPIHonoursRetryAfter(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		if k <= 2 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, `{"ok":true,"ts":"9.9"}`)
	}))
	defer srv.Close()
	sl := &sleeper{clk: clk}
	api := NewAPI(srv.URL+"/api/", testBotToken, Channels{ChannelLoop})
	api.Sleep = sl.Sleep
	ts, err := api.PostMessage(context.Background(), Message{Channel: ChannelLoop, Text: "x"})
	if err != nil || ts != "9.9" {
		t.Fatalf("post: %q %v", ts, err)
	}
	if w := sl.Waits(); len(w) != 2 || w[0] != 7*time.Second || w[1] != 7*time.Second {
		t.Fatalf("waits %v", w)
	}
	if got := clk.Now().Sub(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)); got != 14*time.Second {
		t.Fatalf("clock moved %v", got)
	}
}

func TestAPIGivesUpAfterBoundedRetries(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	sl := &sleeper{}
	api := NewAPI(srv.URL+"/api/", testBotToken, Channels{ChannelLoop})
	api.Sleep = sl.Sleep
	_, err := api.PostMessage(context.Background(), Message{Channel: ChannelLoop, Text: "x"})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "rate_limited" {
		t.Fatalf("err %v", err)
	}
	if n != api.MaxRetries+1 || len(sl.Waits()) != api.MaxRetries {
		t.Fatalf("%d requests, %d waits", n, len(sl.Waits()))
	}
}

func TestAPIHTTPErrorIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()
	api := NewAPI(srv.URL+"/api/", testBotToken, Channels{ChannelLoop})
	_, err := api.PostMessage(context.Background(), Message{Channel: ChannelLoop, Text: "x"})
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != http.StatusBadGateway {
		t.Fatalf("err %v", err)
	}
}
