package slack

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Envelope is one Socket Mode message.
type Envelope struct {
	EnvelopeID   string          `json:"envelope_id"`
	Type         string          `json:"type"`
	Payload      json.RawMessage `json:"payload"`
	Reason       string          `json:"reason"`
	RetryAttempt int             `json:"retry_attempt"`
}

// Socket is a Socket Mode client: it opens a connection with the app token, acks every envelope
// at once, drops Slack's retries, and reconnects (at once after a disconnect envelope, with
// backoff after a dropped connection).
type Socket struct {
	// Open returns a WebSocket URL (API.OpenConnection with the app token). The URL carries a
	// ticket, so it is never logged.
	Open func(context.Context) (string, error)
	// OnEnvelope handles events_api and interactive envelopes, one at a time, after the ack.
	OnEnvelope func(context.Context, Envelope)
	// OnCommand answers a slash command; its result rides on the ack. If it takes longer than
	// CommandTimeout the ack says "working…" and OnLate gets the answer instead.
	OnCommand      func(context.Context, Envelope) map[string]any
	OnLate         func(context.Context, Envelope, map[string]any)
	CommandTimeout time.Duration
	Log            *slog.Logger
	// Sleep waits between reconnects; tests inject one that does not sleep.
	Sleep func(context.Context, time.Duration) error
	// PingEvery checks a quiet connection is alive; 0 turns it off.
	PingEvery time.Duration

	seen     map[string]bool
	seenList []string
}

const (
	minBackoff = time.Second
	maxBackoff = time.Minute
	seenCap    = 2000
)

// Run keeps a connection open until ctx ends.
func (s *Socket) Run(ctx context.Context) {
	if s.Sleep == nil {
		s.Sleep = realSleep
	}
	if s.CommandTimeout == 0 {
		s.CommandTimeout = 2 * time.Second
	}
	s.seen = map[string]bool{}
	work := make(chan Envelope, 256)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case e := <-work:
				s.OnEnvelope(ctx, e)
			}
		}
	}()
	defer wg.Wait()

	backoff := minBackoff
	for ctx.Err() == nil {
		hello, refresh, err := s.session(ctx, work)
		if ctx.Err() != nil {
			return
		}
		if hello {
			backoff = minBackoff
		}
		if refresh {
			s.Log.Info("slack socket: reconnecting", "reason", "disconnect requested")
			continue
		}
		s.Log.Warn("slack socket: connection lost; reconnecting", "err", errText(err), "in", backoff.String())
		if s.Sleep(ctx, backoff) != nil {
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// session runs one connection. hello: Slack greeted it; refresh: Slack asked to reconnect.
func (s *Socket) session(ctx context.Context, work chan<- Envelope) (hello, refresh bool, err error) {
	url, err := s.Open(ctx)
	if err != nil {
		return false, false, err
	}
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		// coder/websocket's dial errors can include the URL; report only that it failed.
		return false, false, errors.New("websocket dial failed")
	}
	conn.SetReadLimit(4 << 20)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.CloseNow()
	if s.PingEvery > 0 {
		go s.ping(cctx, conn)
	}
	for {
		_, data, err := conn.Read(cctx)
		if err != nil {
			return hello, false, err
		}
		var e Envelope
		if err := json.Unmarshal(data, &e); err != nil {
			s.Log.Warn("slack socket: unreadable message")
			continue
		}
		switch e.Type {
		case "hello":
			hello = true
			continue
		case "disconnect":
			conn.Close(websocket.StatusNormalClosure, "")
			return hello, true, nil
		}
		if e.EnvelopeID == "" {
			continue
		}
		if e.Type == "slash_commands" && s.OnCommand != nil {
			if s.duplicate(e) {
				s.ack(cctx, conn, e.EnvelopeID, nil)
				continue
			}
			go s.command(cctx, conn, e)
			continue
		}
		// Ack first: Slack retries anything not acked within 3 seconds.
		s.ack(cctx, conn, e.EnvelopeID, nil)
		if s.duplicate(e) {
			continue
		}
		select {
		case work <- e:
		default:
			s.Log.Error("slack socket: handler queue full; envelope dropped", "type", e.Type)
		}
	}
}

func (s *Socket) ack(ctx context.Context, conn *websocket.Conn, id string, payload map[string]any) {
	msg := map[string]any{"envelope_id": id}
	if payload != nil {
		msg["payload"] = payload
	}
	b, _ := json.Marshal(msg)
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil && ctx.Err() == nil {
		s.Log.Warn("slack socket: ack failed", "err", err.Error())
	}
}

// command answers a slash command on its ack, or acks "working…" and answers late.
func (s *Socket) command(ctx context.Context, conn *websocket.Conn, e Envelope) {
	res := make(chan map[string]any, 1)
	go func() { res <- s.OnCommand(ctx, e) }()
	t := time.NewTimer(s.CommandTimeout)
	defer t.Stop()
	select {
	case p := <-res:
		s.ack(ctx, conn, e.EnvelopeID, p)
	case <-t.C:
		s.ack(ctx, conn, e.EnvelopeID, map[string]any{"response_type": "ephemeral", "text": "working…"})
		p := <-res
		if s.OnLate != nil && p != nil {
			s.OnLate(ctx, e, p)
		}
	case <-ctx.Done():
	}
}

// duplicate reports whether the envelope, or the event inside it, was seen before. Only the
// session goroutine calls it.
func (s *Socket) duplicate(e Envelope) bool {
	keys := []string{"env:" + e.EnvelopeID}
	var p struct {
		EventID string `json:"event_id"`
	}
	if json.Unmarshal(e.Payload, &p) == nil && p.EventID != "" {
		keys = append(keys, "event:"+p.EventID)
	}
	dup := false
	for _, k := range keys {
		if s.seen[k] {
			dup = true
		}
	}
	for _, k := range keys {
		if !s.seen[k] {
			s.seen[k] = true
			s.seenList = append(s.seenList, k)
		}
	}
	for len(s.seenList) > seenCap {
		delete(s.seen, s.seenList[0])
		s.seenList = s.seenList[1:]
	}
	return dup
}

func (s *Socket) ping(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(s.PingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := conn.Ping(pctx)
			cancel()
			if err != nil && ctx.Err() == nil {
				s.Log.Warn("slack socket: ping failed; dropping the connection")
				conn.CloseNow()
				return
			}
		}
	}
}
