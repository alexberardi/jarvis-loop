package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The facts of the workspace (docs/phases/03-slack.md). They are constants, not flags: changing
// who may approve, or where the bot may post, is a code change and goes through review.
const (
	// ChannelLoop holds one thread per item.
	ChannelLoop = "C0C9G6Y87HA"
	// ChannelDigest holds the active-agents control message and the digest.
	ChannelDigest = "C0C969AA2Q3"
	// DefaultBaseURL is Slack's Web API.
	DefaultBaseURL = "https://slack.com/api/"
)

// ErrChannelNotAllowed is returned, without calling Slack, for a channel the bot may not post in.
var ErrChannelNotAllowed = errors.New("slack: the bot may not post in that channel")

// Error is a failed Web API call: Slack's ok:false error code, or an HTTP status. It never holds
// a token.
type Error struct {
	Method string
	Code   string
	Status int
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("slack %s: %s", e.Method, e.Code)
	}
	return fmt.Sprintf("slack %s: HTTP %d", e.Method, e.Status)
}

// Channels lists the channels a client may post in; empty means none.
type Channels []string

// API is a minimal Slack Web API client: JSON over net/http, only the methods loopd uses.
type API struct {
	base     string
	token    string
	channels Channels
	// HTTP is the client used; it has a timeout by default.
	HTTP *http.Client
	// Sleep waits between 429 retries; tests inject one that moves a fake clock.
	Sleep func(context.Context, time.Duration) error
	// MaxRetries is how many 429s in a row are waited out before giving up.
	MaxRetries int
}

// NewAPI returns a client for baseURL (DefaultBaseURL in production) with a token. The token is
// only ever put in the Authorization header.
func NewAPI(baseURL, token string, channels Channels) *API {
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	return &API{base: baseURL, token: token, channels: channels,
		HTTP: &http.Client{Timeout: 20 * time.Second}, Sleep: realSleep, MaxRetries: 3}
}

func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// maxRetryAfter caps how long one Retry-After is honoured.
const maxRetryAfter = 5 * time.Minute

type response struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	TS      string `json:"ts"`
	URL     string `json:"url"`
	Channel string `json:"channel"`
}

func (a *API) call(ctx context.Context, method string, body any) (response, error) {
	var out response
	payload, err := json.Marshal(body)
	if err != nil {
		return out, err
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+method, bytes.NewReader(payload))
		if err != nil {
			return out, err
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("Authorization", "Bearer "+a.token)
		resp, err := a.HTTP.Do(req)
		if err != nil {
			// The error names the URL (no token: that is in a header).
			return out, fmt.Errorf("slack %s: %w", method, err)
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			if attempt >= a.MaxRetries {
				return out, &Error{Method: method, Code: "rate_limited", Status: resp.StatusCode}
			}
			wait := time.Second
			if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s >= 0 {
				wait = time.Duration(s) * time.Second
			}
			wait = min(wait, maxRetryAfter)
			if err := a.Sleep(ctx, wait); err != nil {
				return out, err
			}
			continue
		}
		if rerr != nil {
			return out, fmt.Errorf("slack %s: %w", method, rerr)
		}
		if resp.StatusCode != http.StatusOK {
			return out, &Error{Method: method, Status: resp.StatusCode}
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return out, fmt.Errorf("slack %s: bad response: %w", method, err)
		}
		if !out.OK {
			return out, &Error{Method: method, Code: out.Error, Status: resp.StatusCode}
		}
		return out, nil
	}
}

func (a *API) allowed(channel string) error {
	if !slices.Contains(a.channels, channel) {
		return ErrChannelNotAllowed
	}
	return nil
}

// Message is a message to post or update.
type Message struct {
	Channel  string
	Text     string           // the fallback text (notifications); clipped to MaxChars
	Blocks   []map[string]any // optional
	ThreadTS string           // reply in this thread
	TS       string           // the message to update
	User     string           // the recipient of an ephemeral message
}

func (m Message) body() map[string]any {
	b := map[string]any{"channel": m.Channel, "text": Clip(m.Text, MaxChars, "")}
	if m.Blocks != nil {
		b["blocks"] = m.Blocks
	}
	if m.ThreadTS != "" {
		b["thread_ts"] = m.ThreadTS
	}
	if m.TS != "" {
		b["ts"] = m.TS
	}
	if m.User != "" {
		b["user"] = m.User
	}
	// Links and mentions are written by the bot itself; user text is escaped before it gets here.
	b["unfurl_links"], b["unfurl_media"] = false, false
	return b
}

// PostMessage posts a message (chat.postMessage) and returns its ts.
func (a *API) PostMessage(ctx context.Context, m Message) (string, error) {
	if err := a.allowed(m.Channel); err != nil {
		return "", err
	}
	m.TS = ""
	r, err := a.call(ctx, "chat.postMessage", m.body())
	return r.TS, err
}

// Update replaces a message (chat.update).
func (a *API) Update(ctx context.Context, m Message) error {
	if err := a.allowed(m.Channel); err != nil {
		return err
	}
	b := m.body()
	delete(b, "unfurl_links")
	delete(b, "unfurl_media")
	delete(b, "thread_ts")
	if m.Blocks == nil {
		// A text-only update still replaces the blocks, so old buttons go away.
		b["blocks"] = []map[string]any{section(b["text"].(string))}
	}
	_, err := a.call(ctx, "chat.update", b)
	return err
}

// PostEphemeral shows a message to one user in a channel (chat.postEphemeral).
func (a *API) PostEphemeral(ctx context.Context, m Message) error {
	if err := a.allowed(m.Channel); err != nil {
		return err
	}
	b := m.body()
	delete(b, "unfurl_links")
	delete(b, "unfurl_media")
	_, err := a.call(ctx, "chat.postEphemeral", b)
	return err
}

// PinsAdd pins a message; a message that is already pinned is not an error.
func (a *API) PinsAdd(ctx context.Context, channel, ts string) error {
	if err := a.allowed(channel); err != nil {
		return err
	}
	_, err := a.call(ctx, "pins.add", map[string]any{"channel": channel, "timestamp": ts})
	var ae *Error
	if errors.As(err, &ae) && ae.Code == "already_pinned" {
		return nil
	}
	return err
}

// OpenConnection asks for a Socket Mode WebSocket URL (apps.connections.open, app token).
func (a *API) OpenConnection(ctx context.Context) (string, error) {
	r, err := a.call(ctx, "apps.connections.open", map[string]any{})
	if err != nil {
		return "", err
	}
	if r.URL == "" {
		return "", &Error{Method: "apps.connections.open", Code: "no_url"}
	}
	return r.URL, nil
}
