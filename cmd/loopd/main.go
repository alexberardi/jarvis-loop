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
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/api"
	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/loopd"
	"github.com/alexberardi/jarvis-loop/internal/slack"
	"github.com/alexberardi/jarvis-loop/internal/store"
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
	reposFlag := fs.String("repos", "", "the repos the loop works on, as owner/name slugs, comma-separated: the only repos triage may give an item that has none, and the ones `/loop add` recognises by name")
	reposFile := fs.String("repos-file", "", "a file of repos, one owner/name slug per line, optionally followed by ':' and comma-separated aliases (`alexberardi/jarvis-node-mobile: mobile, node app`); '#' starts a comment. Added to --repos")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkAddr(*addr, *lan); err != nil {
		return err
	}
	repos, err := parseRepos(*reposFlag)
	if err != nil {
		return fmt.Errorf("--repos: %w", err)
	}
	var aliases map[string]string
	if *reposFile != "" {
		b, err := os.ReadFile(expand(*reposFile))
		if err != nil {
			return fmt.Errorf("--repos-file: %w", err)
		}
		more, al, err := parseReposFile(string(b))
		if err != nil {
			return fmt.Errorf("--repos-file %s: %w", *reposFile, err)
		}
		for _, r := range more {
			if !slices.Contains(repos, r) {
				repos = append(repos, r)
			}
		}
		aliases = al
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
	st.SetRepos(repos)

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
		if _, err := loopd.StartSlack(sctx, &bg, st, loopd.Slack{BaseURL: slack.DefaultBaseURL, Tokens: slackToks,
			LinkBase: *slackLink, Repos: repos, Aliases: aliases, Clock: clock.Real{}, Log: log}); err != nil {
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

// handler serves the API, plus the status page at / and its form at /ui/control.
func handler(st *store.Store, toks *api.Tokens, log *slog.Logger) http.Handler {
	return loopd.Handler(st, toks, log)
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

// parseReposFile reads --repos-file: one owner/name slug per line, optionally followed by ':' and
// comma-separated aliases; blank lines and '#' comments are skipped. An alias may name one repo
// only, and never another repo's slug or name.
func parseReposFile(text string) ([]string, map[string]string, error) {
	var repos []string
	aliases := map[string]string{}
	for n, line := range strings.Split(text, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		slug, rest, _ := strings.Cut(line, ":")
		slug = strings.TrimSpace(slug)
		if !slugRe.MatchString(slug) {
			return nil, nil, fmt.Errorf("line %d: %q is not an owner/name slug", n+1, slug)
		}
		if !slices.Contains(repos, slug) {
			repos = append(repos, slug)
		}
		for _, a := range strings.Split(rest, ",") {
			a = strings.Join(strings.Fields(strings.ToLower(a)), " ")
			if a == "" {
				continue
			}
			if strings.ContainsAny(a, "[]:/") {
				return nil, nil, fmt.Errorf("line %d: alias %q may not contain [ ] : or /", n+1, a)
			}
			if prev, ok := aliases[a]; ok && prev != slug {
				return nil, nil, fmt.Errorf("line %d: alias %q names both %s and %s", n+1, a, prev, slug)
			}
			aliases[a] = slug
		}
	}
	for a := range aliases {
		for _, r := range repos {
			if _, name, _ := strings.Cut(strings.ToLower(r), "/"); a == name {
				return nil, nil, fmt.Errorf("alias %q is the name of %s", a, r)
			}
		}
	}
	return repos, aliases, nil
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
