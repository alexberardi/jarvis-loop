// Package slackfake is a fake Slack for loopd's tests and for the loop's own harness
// (scripts/loop-sandbox). It speaks the Web API methods the bot uses and Socket Mode, records
// every call and keeps a model of the channels (messages, thread replies, edits, ephemerals), and
// can inject what a person would do: a thread reply, a mention, a button press, a menu choice, a
// slash command, from the approver or from anyone else.
//
// It never talks to real Slack, and it does not import internal/slack (whose tests use it).
package slackfake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// BotUser is the user id messages posted with the bot token are recorded under.
const BotUser = "BOT"

// Call is one Web API call as the fake received it.
type Call struct {
	Seq    int            `json:"seq"`
	Method string         `json:"method"`
	Body   map[string]any `json:"body"`
	// Auth is the Authorization header (kept so a harness can check which token was used; never
	// rendered).
	Auth string `json:"-"`
}

// Message is a message as it stands in the fake workspace.
type Message struct {
	Seq      int              `json:"seq"` // order of posting (shared with calls and injections)
	Channel  string           `json:"channel"`
	TS       string           `json:"ts"`
	ThreadTS string           `json:"thread_ts,omitempty"`
	User     string           `json:"user"`         // BotUser or the person's id
	To       string           `json:"to,omitempty"` // an ephemeral's only reader
	Kind     string           `json:"kind"`         // "message", "ephemeral", "command" (a slash command typed), "response" (its answer)
	Text     string           `json:"text"`
	Blocks   []map[string]any `json:"blocks,omitempty"`
	Edits    int              `json:"edits,omitempty"`  // chat.update calls on it
	Pinned   bool             `json:"pinned,omitempty"` // pins.add
	History  []string         `json:"history,omitempty"`
}

// Server is the fake. Start it with New; Close it when done.
type Server struct {
	srv *httptest.Server

	mu       sync.Mutex
	seq      int
	calls    []Call
	msgs     []*Message
	nextTS   int
	fail     map[string][]string // method -> error codes to answer next, in order
	opens    int
	live     *websocket.Conn // the latest client connection (injections go there)
	liveCh   chan struct{}   // closed when a connection is live
	waiting  map[string]chan map[string]any
	eventSeq int

	conns chan *websocket.Conn
	acks  chan map[string]any
}

// New starts a fake Slack on a loopback port.
func New() *Server {
	return NewAt("127.0.0.1:0")
}

// NewAt starts a fake Slack listening on addr (loopback).
func NewAt(addr string) *Server {
	f := &Server{fail: map[string][]string{}, conns: make(chan *websocket.Conn, 8), acks: make(chan map[string]any, 256),
		waiting: map[string]chan map[string]any{}, liveCh: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/{method}", f.api)
	mux.HandleFunc("/ws", f.ws)
	f.srv = httptest.NewUnstartedServer(mux)
	if addr != "127.0.0.1:0" {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			f.srv.Listener.Close()
			f.srv.Listener = ln
		}
	}
	f.srv.Start()
	return f
}

// Close stops the server and drops the connections.
func (f *Server) Close() {
	f.mu.Lock()
	if f.live != nil {
		f.live.CloseNow()
	}
	f.mu.Unlock()
	f.srv.CloseClientConnections()
	f.srv.Close()
}

// URL is the server's base URL.
func (f *Server) URL() string { return f.srv.URL }

// BaseURL is the Web API base, for slack.NewAPI.
func (f *Server) BaseURL() string { return f.srv.URL + "/api/" }

func (f *Server) nextSeq() int {
	f.seq++
	return f.seq
}

func (f *Server) newTS() string {
	f.nextTS++
	return fmt.Sprintf("1700000000.%06d", f.nextTS)
}

func (f *Server) find(channel, ts string) *Message {
	for _, m := range f.msgs {
		if m.Channel == channel && m.TS == ts {
			return m
		}
	}
	return nil
}

func blocksFrom(v any) []map[string]any {
	if v == nil {
		return nil
	}
	raw, _ := json.Marshal(v)
	var out []map[string]any
	json.Unmarshal(raw, &out)
	return out
}

func (f *Server) api(w http.ResponseWriter, r *http.Request) {
	method := r.PathValue("method")
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	seq := f.nextSeq()
	f.calls = append(f.calls, Call{Seq: seq, Method: method, Body: body, Auth: r.Header.Get("Authorization")})
	var code string
	if q := f.fail[method]; len(q) > 0 {
		code, f.fail[method] = q[0], q[1:]
	}
	ts := f.newTS()
	if method == "apps.connections.open" {
		f.opens++
	}
	str := func(k string) string { s, _ := body[k].(string); return s }
	if code == "" {
		switch method {
		case "chat.postMessage":
			f.msgs = append(f.msgs, &Message{Seq: seq, Channel: str("channel"), TS: ts, ThreadTS: str("thread_ts"), User: BotUser,
				Kind: "message", Text: str("text"), Blocks: blocksFrom(body["blocks"])})
		case "chat.postEphemeral":
			f.msgs = append(f.msgs, &Message{Seq: seq, Channel: str("channel"), TS: ts, ThreadTS: str("thread_ts"), User: BotUser,
				To: str("user"), Kind: "ephemeral", Text: str("text"), Blocks: blocksFrom(body["blocks"])})
		case "chat.update":
			m := f.find(str("channel"), str("ts"))
			if m == nil {
				code = "message_not_found"
				break
			}
			m.History = append(m.History, m.Text)
			m.Text, m.Edits = str("text"), m.Edits+1
			if b, ok := body["blocks"]; ok {
				m.Blocks = blocksFrom(b)
			}
		case "pins.add":
			if m := f.find(str("channel"), str("timestamp")); m != nil {
				if m.Pinned {
					code = "already_pinned"
				}
				m.Pinned = true
			}
		}
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

func (f *Server) ws(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(4 << 20)
	f.mu.Lock()
	f.live = c
	select {
	case <-f.liveCh:
	default:
		close(f.liveCh)
	}
	f.mu.Unlock()
	select {
	case f.conns <- c:
	default:
	}
	for {
		_, data, err := c.Read(context.Background())
		if err != nil {
			f.mu.Lock()
			if f.live == c {
				f.live = nil
				f.liveCh = make(chan struct{})
			}
			f.mu.Unlock()
			return
		}
		var ack map[string]any
		json.Unmarshal(data, &ack)
		id, _ := ack["envelope_id"].(string)
		f.mu.Lock()
		if ch := f.waiting[id]; ch != nil {
			delete(f.waiting, id)
			ch <- ack
		}
		f.mu.Unlock()
		select {
		case f.acks <- ack:
		default: // nobody reads the raw acks (the harness); injections got theirs above
		}
	}
}

// FailNext makes the next calls of method answer ok:false with these codes ("http500" answers
// HTTP 500).
func (f *Server) FailNext(method string, codes ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[method] = append(f.fail[method], codes...)
}

// Take returns the calls so far and forgets them.
func (f *Server) Take() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

// Calls returns every call kept so far (Take forgets them).
func (f *Server) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Messages returns a copy of every message in the workspace, in posting order.
func (f *Server) Messages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Message, 0, len(f.msgs))
	for _, m := range f.msgs {
		c := *m
		c.History = append([]string(nil), m.History...)
		out = append(out, c)
	}
	return out
}

// OpenCount is how many times apps.connections.open was called.
func (f *Server) OpenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
}

// NextConn waits for the client's next WebSocket connection.
func (f *Server) NextConn(timeout time.Duration) (*websocket.Conn, error) {
	select {
	case c := <-f.conns:
		return c, nil
	case <-time.After(timeout):
		return nil, errors.New("slackfake: the client never connected")
	}
}

// Send writes one Socket Mode message to a connection.
func (f *Server) Send(c *websocket.Conn, v any) error {
	b, _ := json.Marshal(v)
	return c.Write(context.Background(), websocket.MessageText, b)
}

// NextAck waits for the client's next ack (any envelope).
func (f *Server) NextAck(timeout time.Duration) (map[string]any, error) {
	select {
	case a := <-f.acks:
		return a, nil
	case <-time.After(timeout):
		return nil, errors.New("slackfake: no ack")
	}
}

// WaitConnected waits until a client holds a live Socket Mode connection, and greets it.
func (f *Server) WaitConnected(timeout time.Duration) error {
	f.mu.Lock()
	ch := f.liveCh
	f.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-time.After(timeout):
		return errors.New("slackfake: no Socket Mode connection")
	}
}

// deliver sends an envelope over the live connection and waits for its ack.
func (f *Server) deliver(typ string, payload any, timeout time.Duration) (map[string]any, error) {
	if err := f.WaitConnected(timeout); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.eventSeq++
	id := fmt.Sprintf("env-%d", f.eventSeq)
	ch := make(chan map[string]any, 1)
	f.waiting[id] = ch
	c := f.live
	f.mu.Unlock()
	if c == nil {
		return nil, errors.New("slackfake: no Socket Mode connection")
	}
	if err := f.Send(c, map[string]any{"envelope_id": id, "type": typ, "payload": payload}); err != nil {
		return nil, err
	}
	select {
	case a := <-ch:
		return a, nil
	case <-time.After(timeout):
		f.mu.Lock()
		delete(f.waiting, id)
		f.mu.Unlock()
		return nil, fmt.Errorf("slackfake: envelope %s (%s) was not acked", id, typ)
	}
}

// InjectTimeout bounds how long an injection waits for the bot's ack.
var InjectTimeout = 10 * time.Second

// Reply is a person's reply in a thread (a message event). It is recorded in the workspace and
// delivered to the bot; the result is the reply's ts.
func (f *Server) Reply(user, channel, threadTS, text string) (string, error) {
	return f.message(user, channel, threadTS, text, "message")
}

// Mention is a person's message mentioning the bot (an app_mention event), outside a thread
// when threadTS is empty.
func (f *Server) Mention(user, channel, threadTS, text string) (string, error) {
	return f.message(user, channel, threadTS, text, "app_mention")
}

func (f *Server) message(user, channel, threadTS, text, typ string) (string, error) {
	f.mu.Lock()
	seq := f.nextSeq()
	ts := f.newTS()
	f.msgs = append(f.msgs, &Message{Seq: seq, Channel: channel, TS: ts, ThreadTS: threadTS, User: user, Kind: "message", Text: text})
	evID := fmt.Sprintf("Ev%06d", seq)
	f.mu.Unlock()
	ev := map[string]any{"type": typ, "channel": channel, "user": user, "text": text, "ts": ts}
	if threadTS != "" {
		ev["thread_ts"] = threadTS
	}
	_, err := f.deliver("events_api", map[string]any{"event_id": evID, "event": ev}, InjectTimeout)
	return ts, err
}

// ErrNoElement is returned when a press names an element the message does not have.
var ErrNoElement = errors.New("slackfake: the message has no such element")

// element finds an interactive element on a message by action_id: its block_id and the element.
func element(m *Message, actionID string) (string, map[string]any) {
	for _, b := range m.Blocks {
		bid, _ := b["block_id"].(string)
		if acc, ok := b["accessory"].(map[string]any); ok && acc["action_id"] == actionID {
			return bid, acc
		}
		if els, ok := b["elements"].([]any); ok {
			for _, el := range els {
				if e, ok := el.(map[string]any); ok && e["action_id"] == actionID {
					return bid, e
				}
			}
		}
	}
	return "", nil
}

// Press is a person pressing a button (actionID) on the message at channel/ts, as Slack sends it:
// the button's own value and block_id. A message without that button is an error, as a person
// cannot press what is not there.
func (f *Server) Press(user, channel, ts, actionID string) error {
	return f.action(user, channel, ts, actionID, "")
}

// Choose is a person choosing value in a select menu (actionID) on a message.
func (f *Server) Choose(user, channel, ts, actionID, value string) error {
	return f.action(user, channel, ts, actionID, value)
}

func (f *Server) action(user, channel, ts, actionID, value string) error {
	f.mu.Lock()
	m := f.find(channel, ts)
	if m == nil {
		f.mu.Unlock()
		return fmt.Errorf("slackfake: no message %s in %s", ts, channel)
	}
	bid, el := element(m, actionID)
	if el == nil {
		f.mu.Unlock()
		return fmt.Errorf("%w: %s on %s", ErrNoElement, actionID, ts)
	}
	act := map[string]any{"action_id": actionID, "block_id": bid, "type": el["type"]}
	if el["type"] == "static_select" {
		found := false
		if opts, ok := el["options"].([]any); ok {
			for _, o := range opts {
				if om, ok := o.(map[string]any); ok && om["value"] == value {
					act["selected_option"] = om
					found = true
				}
			}
		}
		if !found {
			f.mu.Unlock()
			return fmt.Errorf("%w: option %q of %s", ErrNoElement, value, actionID)
		}
	} else {
		act["value"] = el["value"]
	}
	thread := m.ThreadTS
	f.nextSeq()
	f.mu.Unlock()
	payload := map[string]any{
		"type": "block_actions", "user": map[string]any{"id": user}, "channel": map[string]any{"id": channel},
		"container": map[string]any{"type": "message", "channel_id": channel, "message_ts": ts, "thread_ts": thread},
		"message":   map[string]any{"ts": ts, "thread_ts": thread}, "actions": []any{act},
	}
	_, err := f.deliver("interactive", payload, InjectTimeout)
	return err
}

// Command is a person typing a slash command in channel. The typed command and the answer that
// rides on the ack (ephemeral, to that person) are recorded; the answer's text is returned.
func (f *Server) Command(user, channel, command, text string) (string, error) {
	f.mu.Lock()
	seq := f.nextSeq()
	f.msgs = append(f.msgs, &Message{Seq: seq, Channel: channel, TS: f.newTS(), User: user, To: user, Kind: "command", Text: strings.TrimSpace(command + " " + text)})
	f.mu.Unlock()
	ack, err := f.deliver("slash_commands", map[string]any{"command": command, "text": text, "user_id": user, "channel_id": channel,
		"response_url": f.srv.URL + "/response"}, InjectTimeout)
	if err != nil {
		return "", err
	}
	p, _ := ack["payload"].(map[string]any)
	answer, _ := p["text"].(string)
	f.mu.Lock()
	f.msgs = append(f.msgs, &Message{Seq: f.nextSeq(), Channel: channel, TS: f.newTS(), User: BotUser, To: user, Kind: "response", Text: answer})
	f.mu.Unlock()
	return answer, nil
}
