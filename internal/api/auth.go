package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"

	"github.com/alexberardi/jarvis-loop/internal/build"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// Principal kinds.
const (
	KindUser   = "user"
	KindRunner = "runner"
	// KindBoard reads the web board and nothing else: the API refuses it on every route
	// (docs/decisions/2026-10-15-board.md).
	KindBoard = "board"
	// KindOrchestrator is the orchestrator's (docs/phases/08-orchestrator.md): it reads, and acts
	// through its own routes within their limits; with the approver's Slack message it acts for the
	// user, except approvals (docs/decisions/2026-10-16-orchestrator-token.md).
	KindOrchestrator = "orchestrator"
)

// MinTokenLen is the shortest token loopd accepts.
const MinTokenLen = 16

// Principal is who a token belongs to.
type Principal struct {
	Kind string
	Name string
}

// Actor is how the principal appears in events: "user:alex", "runner:devbox".
func (p Principal) Actor() string { return p.Kind + ":" + p.Name }

type tokenEntry struct {
	sum [32]byte
	p   Principal
}

// Tokens maps bearer tokens to principals.
type Tokens struct{ entries []tokenEntry }

// LoadTokens reads a tokens file and refuses one that group or others can read or write.
func LoadTokens(path string) (*Tokens, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is mode %04o; it holds tokens, so make it 0600", path, fi.Mode().Perm())
	}
	return ParseTokens(f)
}

// ParseTokens reads "token<TAB>kind<TAB>name" lines; blank lines and lines starting with # are
// skipped. Error messages name the line, never the token.
func ParseTokens(r io.Reader) (*Tokens, error) {
	t := &Tokens{}
	seen := map[[32]byte]bool{}
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		parts := strings.Split(s, "\t")
		if len(parts) != 3 {
			return nil, fmt.Errorf("tokens line %d: want token<TAB>kind<TAB>name", line)
		}
		tok, kind, name := parts[0], strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
		if len(tok) < MinTokenLen {
			return nil, fmt.Errorf("tokens line %d: token shorter than %d characters", line, MinTokenLen)
		}
		if kind != KindUser && kind != KindRunner && kind != KindBoard && kind != KindOrchestrator {
			return nil, fmt.Errorf("tokens line %d: kind must be user, runner, board or orchestrator", line)
		}
		if name == "" {
			return nil, fmt.Errorf("tokens line %d: name is empty", line)
		}
		sum := sha256.Sum256([]byte(tok))
		if seen[sum] {
			return nil, fmt.Errorf("tokens line %d: duplicate token", line)
		}
		seen[sum] = true
		t.entries = append(t.entries, tokenEntry{sum: sum, p: Principal{Kind: kind, Name: name}})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(t.entries) == 0 {
		return nil, fmt.Errorf("tokens file has no tokens")
	}
	return t, nil
}

// Lookup finds a token's principal. It compares fixed-length hashes in constant time and checks
// every entry, so the time taken says nothing about which token, or how much of one, matched.
func (t *Tokens) Lookup(tok string) (Principal, bool) {
	sum := sha256.Sum256([]byte(tok))
	var found Principal
	ok := 0
	for _, e := range t.entries {
		if subtle.ConstantTimeCompare(sum[:], e.sum[:]) == 1 {
			found = e.p
			ok = 1
		}
	}
	return found, ok == 1
}

type ctxKey struct{}

// PrincipalFrom returns the authenticated principal of a request.
func PrincipalFrom(ctx context.Context) Principal {
	p, _ := ctx.Value(ctxKey{}).(Principal)
	return p
}

// CodeRunnersOnly refuses a token that is not a runner's on the remote runners' listener.
const CodeRunnersOnly = "runners_only"

// NewRunners is the API for remote runners only: a listener serving this takes remote runners
// without exposing the status page, the board, or the user's and the orchestrator's routes. A
// token of any other kind (user, board, orchestrator) is refused on every route before routing,
// which also rules out acting for the user; a runner token then meets New's routes and rules.
// loopd itself serves one port since it moved to fly.io, where the per-route token rules are what
// separate a runner from the user; this stays for the loop's harness and for anyone who wants a
// listener of its own (docs/decisions/2026-10-17-mac-runners.md).
func NewRunners(st *store.Store, tokens *Tokens, log *slog.Logger, b *build.Build) http.Handler {
	s := &server{st: st, tokens: tokens, log: log, build: b}
	mux := s.routes()
	return Logged(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := tokens.Lookup(BearerToken(r))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="loopd"`)
			writeErr(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
			return
		}
		if p.Kind != KindRunner {
			SetActor(w, p)
			writeErr(w, http.StatusForbidden, CodeRunnersOnly, "this listener is for runners: "+article(p.Kind)+" "+p.Kind+" token is refused here")
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

// BearerToken extracts the token of an "Authorization: Bearer" header.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func (s *server) auth(pattern string, kinds []string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.tokens.Lookup(BearerToken(r))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="loopd"`)
			writeErr(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
			return
		}
		SetActor(w, p)
		onBehalf := r.Header.Get(OnBehalfHeader)
		if onBehalf == "" && !slices.Contains(kinds, p.Kind) {
			s.fail(w, s.refusedRoute(r, p, pattern, &pipeline.Error{Code: CodeForbidden, Message: article(p.Kind) + " " + p.Kind + " token cannot do this"}))
			return
		}
		// Expiry is evaluated on every request, so a read never shows a dead lease as live.
		if err := s.st.Sweep(r.Context()); err != nil {
			s.fail(w, err)
			return
		}
		ctx := r.Context()
		if onBehalf != "" {
			np, nctx, err := s.actForUser(ctx, r, p, pattern, onBehalf)
			if err != nil {
				s.fail(w, err)
				return
			}
			p, ctx = np, nctx
		}
		next(w, r.WithContext(context.WithValue(ctx, ctxKey{}, p)))
	})
}
