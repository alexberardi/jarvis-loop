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
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/api"
	"github.com/alexberardi/jarvis-loop/internal/build"
	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/loopd"
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
	boardAddr := fs.String("board-addr", "", "a second listener serving only the read-only board at /board (e.g. 0.0.0.0:7790, the LAN); off when empty")
	runnerAddr := fs.String("runner-addr", "", "a listener for remote runners (e.g. 0.0.0.0:7781, the LAN): the API for runner tokens only, every other token refused; off when empty")
	slackWorkspace := fs.String("slack-workspace", "", "the Slack workspace's address (https://<team>.slack.com), for the board's links to item threads; none without it")
	slackEnv := fs.String("slack-env", "", "Slack env file, 0600, with SLACK_BOT_TOKEN and SLACK_APP_TOKEN; Slack is off without it")
	orchEnv := fs.String("orchestrator-slack-env", "", "the @orchestrator Slack app's env file (docs/slack/orchestrator-manifest.yml), 0600, like --slack-env's; default: "+orchestratorEnvName+" next to --slack-env's file, the app off when it is absent; \"off\" turns it off")
	slackLink := fs.String("slack-link", "", "loopd's address as the Slack reader reaches it (LAN), for links to long text; default: point at `loop show`")
	reposFlag := fs.String("repos", "", "the repos the loop works on, as owner/name slugs, comma-separated: the only repos triage may give an item that has none, and the ones `/loop add` recognises by name")
	reposFile := fs.String("repos-file", "", "a file of repos, one owner/name slug per line, optionally followed by ':' and comma-separated aliases (`alexberardi/jarvis-node-mobile: mobile, node app`); '#' starts a comment. Added to --repos")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkAddr(*addr, *lan); err != nil {
		return err
	}
	if *boardAddr != "" {
		if _, _, err := net.SplitHostPort(*boardAddr); err != nil {
			return fmt.Errorf("--board-addr: %w", err)
		}
	}
	if *runnerAddr != "" {
		if _, _, err := net.SplitHostPort(*runnerAddr); err != nil {
			return fmt.Errorf("--runner-addr: %w", err)
		}
	}
	if *slackWorkspace != "" {
		if u, err := url.Parse(*slackWorkspace); err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("--slack-workspace %q: want https://<team>.slack.com", *slackWorkspace)
		}
	}
	repos, err := parseRepos(*reposFlag)
	if err != nil {
		return fmt.Errorf("--repos: %w", err)
	}
	// --repos-file is read again on SIGHUP (reloadReposOnHUP).
	rsrc := loopd.RepoSource{Flag: repos}
	if *reposFile != "" {
		rsrc.File = expand(*reposFile)
	}
	repos, aliases, err := rsrc.Load()
	if err != nil {
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
	orchToks, err := orchestratorTokens(*slackEnv, *orchEnv, slackToks)
	if err != nil {
		return fmt.Errorf("orchestrator slack: %w", err)
	}
	st, err := store.Open(expand(*data), clock.Real{})
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer st.Close()
	st.SetRepos(repos)
	st.SetAliases(aliases)

	// The commit this loopd runs, which it serves at GET /version: since loopd is deployed on its
	// own (fly.io) the runner asks for this rather than taking its own build for the loop's
	// (docs/decisions/2026-10-23-loopd-on-fly.md).
	bld := build.Current()
	boardCfg := web.BoardConfig{SlackWorkspace: *slackWorkspace, Clock: clock.Real{}}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler(st, toks, log, boardCfg, bld),
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
	bi := bld.Info()
	log.Info("loopd listening", "addr", ln.Addr().String(), "data", expand(*data), "revision", bi.Revision)
	if bi.Revision == "" {
		log.Warn("build revision unknown: no deployed reports for qa_after_deploy repos (use /loop verified)", "why", bi.Why)
	}
	var boardSrv *http.Server
	var boardLn net.Listener
	if *boardAddr != "" {
		// The board's own listener, for the LAN: read-only routes only, so it needs no --lan.
		boardSrv = &http.Server{
			Addr:              *boardAddr,
			Handler:           loopd.BoardHandler(st, toks, log, boardCfg),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       time.Minute,
			WriteTimeout:      5 * time.Minute, // evidence downloads
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    64 << 10,
		}
		if boardLn, err = net.Listen("tcp", *boardAddr); err != nil {
			ln.Close()
			return fmt.Errorf("--board-addr: %w", err)
		}
		log.Info("board listening", "addr", boardLn.Addr().String())
	}
	var runnerSrv *http.Server
	var runnerLn net.Listener
	if *runnerAddr != "" {
		// The remote runners' listener, for the LAN: runner tokens only, so it needs no --lan.
		runnerSrv = &http.Server{
			Addr:              *runnerAddr,
			Handler:           loopd.RunnerHandler(st, toks, log, bld),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       5 * time.Minute, // evidence uploads
			WriteTimeout:      5 * time.Minute,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    64 << 10,
		}
		if runnerLn, err = net.Listen("tcp", *runnerAddr); err != nil {
			ln.Close()
			if boardLn != nil {
				boardLn.Close()
			}
			return fmt.Errorf("--runner-addr: %w", err)
		}
		log.Info("runner listener listening", "addr", runnerLn.Addr().String())
	}

	var bg sync.WaitGroup
	defer bg.Wait()
	sctx, stopSlack := context.WithCancel(ctx) // ends the Slack goroutines however run returns
	defer stopSlack()
	var bot *slack.Bot
	if *slackEnv != "" {
		if bot, err = loopd.StartSlack(sctx, &bg, st, loopd.Slack{BaseURL: slack.DefaultBaseURL, Tokens: slackToks,
			LinkBase: *slackLink, Repos: repos, Aliases: aliases, Clock: clock.Real{}, Log: log, Orchestrator: orchToks}); err != nil {
			ln.Close()
			if boardLn != nil {
				boardLn.Close()
			}
			if runnerLn != nil {
				runnerLn.Close()
			}
			return err
		}
	}

	// SIGHUP reads the repos again: a repo added to --repos-file needs no restart.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				loopd.ReloadRepos(rsrc, st, bot, log)
			}
		}
	}()

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

	// Any server failing stops them all: loopd is up with all its listeners or not at all.
	servers := []*http.Server{srv}
	errc := make(chan error, 3)
	go func() { errc <- srv.Serve(ln) }()
	if boardSrv != nil {
		servers = append(servers, boardSrv)
		go func() { errc <- boardSrv.Serve(boardLn) }()
	}
	if runnerSrv != nil {
		servers = append(servers, runnerSrv)
		go func() { errc <- runnerSrv.Serve(runnerLn) }()
	}
	var serveErr error
	select {
	case serveErr = <-errc:
	case <-ctx.Done():
	}
	log.Info("loopd shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var errs []error
	for _, s := range servers {
		if err := s.Shutdown(shutCtx); err != nil {
			errs = append(errs, err)
		}
	}
	pending := len(servers)
	if serveErr != nil {
		pending--
		if !errors.Is(serveErr, http.ErrServerClosed) {
			errs = append(errs, serveErr)
		}
	}
	for range pending {
		if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// handler serves the API, plus the status page at / and its form at /ui/control, and the board.
func handler(st *store.Store, toks *api.Tokens, log *slog.Logger, board web.BoardConfig, b *build.Build) http.Handler {
	return loopd.Handler(st, toks, log, board, b)
}

// orchestratorEnvName is the @orchestrator app's env file, next to the loop bot's.
const orchestratorEnvName = "orchestrator-slack.env"

// orchestratorTokens reads the @orchestrator app's tokens (--orchestrator-slack-env): by default
// from orchestratorEnvName next to slackEnv's file, the app off (nil) when that file is absent or
// Slack is off; an explicit file must exist. Its tokens must be another app's than the loop bot's.
func orchestratorTokens(slackEnv, orchEnv string, loop slack.Tokens) (*slack.Tokens, error) {
	if orchEnv == "off" {
		return nil, nil
	}
	if slackEnv == "" {
		if orchEnv != "" {
			return nil, errors.New("--orchestrator-slack-env needs --slack-env (the loop bot)")
		}
		return nil, nil
	}
	path := expand(orchEnv)
	if orchEnv == "" {
		path = filepath.Join(filepath.Dir(expand(slackEnv)), orchestratorEnvName)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
	}
	t, err := slack.LoadEnv(path)
	if err != nil {
		return nil, err
	}
	if t.Bot == loop.Bot || t.App == loop.App {
		return nil, fmt.Errorf("%s holds the loop bot's own token: the orchestrator is a second Slack app, with its own", path)
	}
	return &t, nil
}

var slugRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// parseRepos reads --repos: owner/name slugs, comma-separated, deduplicated.
func parseRepos(s string) ([]string, error) {
	var out []string
	for _, r := range strings.Split(s, ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !slugRe.MatchString(r) {
			return nil, fmt.Errorf("%q is not an owner/name slug", r)
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out, nil
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
