// Command loopd is the Jarvis Loop control plane: the pipeline's state, its rules, the API and
// the status page (docs/DESIGN.md, docs/phases/02-loopd-core.md).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/api"
	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/slack"
	"github.com/alexberardi/jarvis-loop/internal/store"
	"github.com/alexberardi/jarvis-loop/internal/web"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "loopd:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("loopd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:7780", "listen address")
	data := fs.String("data", "~/.local/share/jarvis-loop", "data directory (database and evidence)")
	tokensPath := fs.String("tokens", "~/.config/jarvis-loop/tokens", "tokens file, 0600: token<TAB>kind<TAB>name per line")
	lan := fs.Bool("lan", false, "allow listening on a non-loopback address")
	slackEnv := fs.String("slack-env", "", "Slack env file, 0600, with SLACK_BOT_TOKEN and SLACK_APP_TOKEN; Slack is off without it")
	slackLink := fs.String("slack-link", "", "loopd's address as the Slack reader reaches it (LAN), for links to long text; default: point at `loop show`")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkAddr(*addr, *lan); err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))

	toks, err := api.LoadTokens(expand(*tokensPath))
	if err != nil {
		return fmt.Errorf("tokens: %w", err)
	}
	var slackToks slack.Tokens
	if *slackEnv != "" {
		if slackToks, err = slack.LoadEnv(expand(*slackEnv)); err != nil {
			return fmt.Errorf("slack: %w", err)
		}
	}
	st, err := store.Open(expand(*data), clock.Real{})
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer st.Close()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler(st, toks, log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute, // evidence uploads
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	log.Info("loopd listening", "addr", ln.Addr().String(), "data", expand(*data))

	var bg sync.WaitGroup
	defer bg.Wait()
	sctx, stopSlack := context.WithCancel(ctx) // ends the Slack goroutines however run returns
	defer stopSlack()
	if *slackEnv != "" {
		if err := startSlack(sctx, &bg, st, slackToks, *slackLink, log); err != nil {
			ln.Close()
			return err
		}
	}

	// Expiry also runs on every request; the ticker covers a quiet server.
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := st.Sweep(ctx); err != nil && ctx.Err() == nil {
					log.Error("sweep", "err", err)
				}
			}
		}
	}()

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("loopd shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// startSlack runs the Slack layer: the bot (event fan-out, timers, control message) and the
// Socket Mode connection. Slack failures are logged; they never stop loopd or a pipeline action.
func startSlack(ctx context.Context, bg *sync.WaitGroup, st *store.Store, toks slack.Tokens, link string, log *slog.Logger) error {
	slg := log.With("component", "slack")
	botAPI := slack.NewAPI(slack.DefaultBaseURL, toks.Bot, slack.Channels{slack.ChannelLoop, slack.ChannelDigest})
	appAPI := slack.NewAPI(slack.DefaultBaseURL, toks.App, nil)
	bot, err := slack.New(slack.Config{Store: st, API: botAPI, Clock: clock.Real{}, Log: slg, LinkBase: link})
	if err != nil {
		return err
	}
	bot.Attach()
	sock := &slack.Socket{Open: appAPI.OpenConnection, OnEnvelope: bot.HandleEnvelope, OnCommand: bot.HandleCommand,
		OnLate: bot.Late, Log: slg, PingEvery: 30 * time.Second}
	bg.Add(2)
	go func() { defer bg.Done(); bot.Run(ctx) }()
	go func() { defer bg.Done(); sock.Run(ctx) }()
	slg.Info("slack on", "channels", "#loop, #loop-digest")
	return nil
}

// handler serves the API, plus the status page at / and its form at /ui/control.
func handler(st *store.Store, toks *api.Tokens, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", api.New(st, toks, log))
	page := api.Logged(log, web.New(st, toks, log))
	mux.Handle("GET /{$}", page)
	mux.Handle("POST /ui/control", page)
	return mux
}

// checkAddr refuses a non-loopback listen address unless --lan says it is deliberate.
func checkAddr(addr string, lan bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--addr: %w", err)
	}
	if lan {
		return nil
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("--addr %s is not loopback; pass --lan to listen beyond this machine", addr)
}

func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}
