package api

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

// Principal kinds.
const (
	KindUser   = "user"
	KindRunner = "runner"
	// KindBoard reads the web board and nothing else: the API refuses it on every route
	// (docs/decisions/2026-10-15-board.md).
	KindBoard = "board"
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
		if kind != KindUser && kind != KindRunner && kind != KindBoard {
			return nil, fmt.Errorf("tokens line %d: kind must be user, runner or board", line)
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
