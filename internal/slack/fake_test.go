package slack

import (
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/alexberardi/jarvis-loop/internal/slackfake"
)

// fakeSlack is internal/slackfake with test-friendly helpers that fail the test instead of
// returning errors.
type fakeSlack struct {
	*slackfake.Server
	t *testing.T
}

type call = slackfake.Call

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{Server: slackfake.New(), t: t}
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSlack) baseURL() string { return f.BaseURL() }

func (f *fakeSlack) failNext(method string, codes ...string) { f.FailNext(method, codes...) }

func (f *fakeSlack) take() []call { return f.Take() }

func (f *fakeSlack) openCount() int { return f.OpenCount() }

// conn waits for the client's next WebSocket connection.
func (f *fakeSlack) conn() *websocket.Conn {
	f.t.Helper()
	c, err := f.NextConn(5 * time.Second)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fakeSlack) send(c *websocket.Conn, v any) {
	f.t.Helper()
	if err := f.Send(c, v); err != nil {
		f.t.Fatalf("send: %v", err)
	}
}

// ack waits for the client's next ack.
func (f *fakeSlack) ack() map[string]any {
	f.t.Helper()
	a, err := f.NextAck(5 * time.Second)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}
