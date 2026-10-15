package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeSlack is Slack for tests: the Web API methods record their calls and answer ok (or a
// planned error), apps.connections.open hands out a WebSocket on the same server, and the test
// pushes envelopes over it and reads the client's acks.
type fakeSlack struct {
	t   *testing.T
	srv *httptest.Server

	mu     sync.Mutex
	calls  []call
	nextTS int
	fail   map[string][]string // method -> error codes to answer next, in order
	opens  int

	conns chan *websocket.Conn
	acks  chan map[string]any
}

type call struct {
	Method string
	Body   map[string]any
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{t: t, fail: map[string][]string{}, conns: make(chan *websocket.Conn, 8), acks: make(chan map[string]any, 64)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/{method}", f.api)
	mux.HandleFunc("/ws", f.ws)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) baseURL() string { return f.srv.URL + "/api/" }

func (f *fakeSlack) api(w http.ResponseWriter, r *http.Request) {
	method := r.PathValue("method")
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.calls = append(f.calls, call{method, body})
	var code string
	if q := f.fail[method]; len(q) > 0 {
		code, f.fail[method] = q[0], q[1:]
	}
	f.nextTS++
	ts := fmt.Sprintf("1700000000.%06d", f.nextTS)
	if method == "apps.connections.open" {
		f.opens++
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if code == "http500" {
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	if code != "" {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": code})
		return
	}
	switch method {
	case "apps.connections.open":
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws"})
	case "chat.postMessage":
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": ts, "channel": body["channel"]})
	default:
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

func (f *fakeSlack) ws(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	f.conns <- c
	for {
		_, data, err := c.Read(context.Background())
		if err != nil {
			return
		}
		var ack map[string]any
		json.Unmarshal(data, &ack)
		f.acks <- ack
	}
}

// failNext makes the next calls of method answer ok:false with these codes.
func (f *fakeSlack) failNext(method string, codes ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[method] = append(f.fail[method], codes...)
}

// take returns the calls so far and forgets them.
func (f *fakeSlack) take() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

func (f *fakeSlack) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
}

// conn waits for the client's next WebSocket connection.
func (f *fakeSlack) conn() *websocket.Conn {
	f.t.Helper()
	select {
	case c := <-f.conns:
		return c
	case <-time.After(5 * time.Second):
		f.t.Fatal("the client never connected")
		return nil
	}
}

func (f *fakeSlack) send(c *websocket.Conn, v any) {
	f.t.Helper()
	b, _ := json.Marshal(v)
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		f.t.Fatalf("send: %v", err)
	}
}

// ack waits for the client's next ack.
func (f *fakeSlack) ack() map[string]any {
	f.t.Helper()
	select {
	case a := <-f.acks:
		return a
	case <-time.After(5 * time.Second):
		f.t.Fatal("no ack")
		return nil
	}
}
