// Package loopd is loopd's wiring: the HTTP handler (the API and the status page) and the Slack
// layer (the bot and its Socket Mode connection). cmd/loopd runs it against real Slack; the
// loop's harness (internal/sandbox) runs the same wiring against internal/slackfake.
package loopd

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/api"
	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/slack"
	"github.com/alexberardi/jarvis-loop/internal/store"
	"github.com/alexberardi/jarvis-loop/internal/web"
)

// Handler serves the API, plus the status page at / and its form at /ui/control.
func Handler(st *store.Store, toks *api.Tokens, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", api.New(st, toks, log))
	page := api.Logged(log, web.New(st, toks, log))
	mux.Handle("GET /{$}", page)
	mux.Handle("POST /ui/control", page)
	return mux
}

// Slack configures the Slack layer.
type Slack struct {
	// BaseURL is the Web API (slack.DefaultBaseURL; the harness's fake otherwise).
	BaseURL string
	Tokens  slack.Tokens
	// LinkBase is loopd's address as the Slack reader reaches it, for links to long text.
	LinkBase string
	// Repos are the configured repo slugs, which /loop add recognises by name; Aliases other
	// words for them (alias -> slug).
	Repos   []string
	Aliases map[string]string
	Clock   clock.Clock
	Log     *slog.Logger
	// TickEvery is the bot's timer interval (default 5 s).
	TickEvery time.Duration
	// Sleep waits between Slack retries and reconnects (default: real sleeps).
	Sleep func(context.Context, time.Duration) error
}

// StartSlack runs the Slack layer until ctx ends: the bot (event fan-out, timers, control
// message) and the Socket Mode connection, each in a goroutine counted in bg. Slack failures are
// logged; they never stop loopd or a pipeline action.
func StartSlack(ctx context.Context, bg *sync.WaitGroup, st *store.Store, c Slack) (*slack.Bot, error) {
	if c.BaseURL == "" || c.Clock == nil || c.Log == nil {
		return nil, errors.New("loopd: Slack needs BaseURL, Clock and Log")
	}
	slg := c.Log.With("component", "slack")
	botAPI := slack.NewAPI(c.BaseURL, c.Tokens.Bot, slack.Channels{slack.ChannelLoop, slack.ChannelDigest})
	appAPI := slack.NewAPI(c.BaseURL, c.Tokens.App, nil)
	if c.Sleep != nil {
		botAPI.Sleep, appAPI.Sleep = c.Sleep, c.Sleep
	}
	bot, err := slack.New(slack.Config{Store: st, API: botAPI, Clock: c.Clock, Log: slg, LinkBase: c.LinkBase,
		Repos: c.Repos, Aliases: c.Aliases, TickEvery: c.TickEvery, Sleep: c.Sleep})
	if err != nil {
		return nil, err
	}
	bot.Attach()
	sock := &slack.Socket{Open: appAPI.OpenConnection, OnEnvelope: bot.HandleEnvelope, OnCommand: bot.HandleCommand,
		OnLate: bot.Late, Log: slg, PingEvery: 30 * time.Second, Sleep: c.Sleep}
	bg.Add(2)
	go func() { defer bg.Done(); bot.Run(ctx) }()
	go func() { defer bg.Done(); sock.Run(ctx) }()
	slg.Info("slack on", "channels", "#loop, #loop-digest")
	return bot, nil
}
