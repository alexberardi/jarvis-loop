package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func eventsEnvelope(envID, eventID, text string) map[string]any {
	return map[string]any{"envelope_id": envID, "type": "events_api", "payload": map[string]any{
		"event_id": eventID, "event": map[string]any{"type": "message", "text": text}}}
}

type socketHarness struct {
	f      *fakeSlack
	got    chan Envelope
	sl     *sleeper
	cancel context.CancelFunc
	done   chan struct{}
}

func startSocket(t *testing.T, handle func(Envelope)) *socketHarness {
	t.Helper()
	f := newFakeSlack(t)
	h := &socketHarness{f: f, got: make(chan Envelope, 16), sl: &sleeper{}, done: make(chan struct{})}
	s := &Socket{
		Open: NewAPI(f.baseURL(), "xapp-test", nil).OpenConnection,
		OnEnvelope: func(ctx context.Context, e Envelope) {
			if handle != nil {
				handle(e)
			}
			h.got <- e
		},
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sleep: h.sl.Sleep,
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { s.Run(ctx); close(h.done) }()
	t.Cleanup(func() { cancel(); <-h.done })
	return h
}

func (h *socketHarness) next(t *testing.T) Envelope {
	t.Helper()
	select {
	case e := <-h.got:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no envelope handled")
		return Envelope{}
	}
}

func TestSocketAcksBeforeHandling(t *testing.T) {
	var mu sync.Mutex
	acked := map[string]bool{}
	var h *socketHarness
	ackSeen := make(chan string, 4)
	h = startSocket(t, func(e Envelope) {
		// The handler runs only once the fake has the ack in hand.
		select {
		case id := <-ackSeen:
			mu.Lock()
			acked[id] = true
			mu.Unlock()
		case <-time.After(5 * time.Second):
		}
	})
	c := h.f.conn()
	h.f.send(c, map[string]any{"type": "hello"})
	h.f.send(c, eventsEnvelope("env-1", "Ev1", "hi"))
	a := h.f.ack()
	if a["envelope_id"] != "env-1" {
		t.Fatalf("ack %v", a)
	}
	ackSeen <- "env-1"
	e := h.next(t)
	if e.EnvelopeID != "env-1" || e.Type != "events_api" {
		t.Fatalf("envelope %+v", e)
	}
	mu.Lock()
	defer mu.Unlock()
	if !acked["env-1"] {
		t.Fatal("handled before the ack reached Slack")
	}
}

func TestSocketDropsDuplicates(t *testing.T) {
	h := startSocket(t, nil)
	c := h.f.conn()
	h.f.send(c, eventsEnvelope("env-1", "Ev1", "first"))
	h.f.send(c, eventsEnvelope("env-1", "Ev1", "same envelope again")) // a retry
	h.f.send(c, eventsEnvelope("env-2", "Ev1", "same event, new envelope"))
	h.f.send(c, eventsEnvelope("env-3", "Ev3", "new"))
	// Every envelope is acked, duplicates included, or Slack keeps retrying.
	for i := 0; i < 4; i++ {
		h.f.ack()
	}
	if e := h.next(t); e.EnvelopeID != "env-1" {
		t.Fatalf("first %+v", e)
	}
	if e := h.next(t); e.EnvelopeID != "env-3" {
		t.Fatalf("second %+v (a duplicate got through)", e)
	}
}

func TestSocketReconnectsOnDisconnectAndDrop(t *testing.T) {
	h := startSocket(t, nil)
	c1 := h.f.conn()
	h.f.send(c1, map[string]any{"type": "hello"})
	h.f.send(c1, map[string]any{"type": "disconnect", "reason": "refresh_requested"})

	// A disconnect envelope: a new connection at once, no backoff.
	c2 := h.f.conn()
	if h.f.openCount() != 2 {
		t.Fatalf("opens %d", h.f.openCount())
	}
	if w := h.sl.Waits(); len(w) != 0 {
		t.Fatalf("waited %v after a disconnect envelope", w)
	}
	h.f.send(c2, eventsEnvelope("env-2", "Ev2", "after reconnect"))
	h.f.ack()
	if e := h.next(t); e.EnvelopeID != "env-2" {
		t.Fatalf("envelope %+v", e)
	}

	// A dropped connection: back off, then reconnect.
	c2.Close(websocket.StatusInternalError, "gone")
	c3 := h.f.conn()
	if w := h.sl.Waits(); len(w) != 1 || w[0] != time.Second {
		t.Fatalf("backoff %v", w)
	}
	h.f.send(c3, eventsEnvelope("env-3", "Ev3", "after drop"))
	h.f.ack()
	if e := h.next(t); e.EnvelopeID != "env-3" {
		t.Fatalf("envelope %+v", e)
	}
}

func TestSocketSlashCommandAckCarriesTheAnswer(t *testing.T) {
	f := newFakeSlack(t)
	s := &Socket{
		Open:       NewAPI(f.baseURL(), "xapp-test", nil).OpenConnection,
		OnEnvelope: func(ctx context.Context, e Envelope) {},
		OnCommand: func(ctx context.Context, e Envelope) map[string]any {
			return map[string]any{"response_type": "ephemeral", "text": "done"}
		},
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sleep: (&sleeper{}).Sleep,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	c := f.conn()
	f.send(c, map[string]any{"envelope_id": "env-c", "type": "slash_commands", "payload": map[string]any{"command": "/loop", "text": "status"}})
	a := f.ack()
	b, _ := json.Marshal(a["payload"])
	if a["envelope_id"] != "env-c" || string(b) != `{"response_type":"ephemeral","text":"done"}` {
		t.Fatalf("ack %v", a)
	}
}

func TestSocketSlowCommandAnswersLate(t *testing.T) {
	f := newFakeSlack(t)
	release := make(chan struct{})
	late := make(chan map[string]any, 1)
	s := &Socket{
		Open:       NewAPI(f.baseURL(), "xapp-test", nil).OpenConnection,
		OnEnvelope: func(ctx context.Context, e Envelope) {},
		OnCommand: func(ctx context.Context, e Envelope) map[string]any {
			<-release
			return map[string]any{"response_type": "ephemeral", "text": "slow answer"}
		},
		OnLate:         func(ctx context.Context, e Envelope, p map[string]any) { late <- p },
		CommandTimeout: time.Millisecond,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sleep:          (&sleeper{}).Sleep,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	c := f.conn()
	f.send(c, map[string]any{"envelope_id": "env-slow", "type": "slash_commands", "payload": map[string]any{"command": "/loop"}})
	a := f.ack()
	if a["envelope_id"] != "env-slow" || !strings.Contains(fmt.Sprint(a["payload"]), "working…") {
		t.Fatalf("ack %v", a)
	}
	close(release)
	select {
	case p := <-late:
		if p["text"] != "slow answer" {
			t.Fatalf("late %v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no late answer")
	}
}
