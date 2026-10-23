// Package build is the commit a binary was built from: Go's vcs stamps, and the mutable holder
// loopd serves at GET /version. Since loopd moved to fly.io it is deployed on its own, so the
// runner's build no longer stands for the loop's and it has to ask loopd
// (docs/decisions/2026-10-23-loopd-on-fly.md).
package build

import (
	"runtime/debug"
	"sync"
)

// Info is a binary's build, as GET /version reports it: the commit, or no commit and why not.
type Info struct {
	Revision string `json:"revision"`
	Why      string `json:"why,omitempty"`
}

// Revision is the commit this binary was built from (Go's vcs.revision stamp), or "" and why not:
// no stamp (go run, go install pkg@version, a build outside a checkout) or a build with
// uncommitted changes, which contains no commit for certain.
func Revision() (rev, why string) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "", "no build info"
	}
	modified := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	switch {
	case rev == "":
		return "", "the binary has no vcs.revision stamp"
	case modified:
		return "", "the binary was built from a checkout with uncommitted changes (vcs.modified)"
	}
	return rev, ""
}

// Build holds what a process reports as its build. It is mutable so the loop's harness can stand
// in for a deployed loopd (Stack.Deploy); loopd itself sets it once, from Revision, at startup.
// A nil *Build reports no revision, so a test handler may leave it out.
type Build struct {
	mu   sync.RWMutex
	info Info
}

// New is a Build reporting rev, or no revision and why not.
func New(rev, why string) *Build { return &Build{info: Info{Revision: rev, Why: why}} }

// Current is a Build reporting this binary's own stamps.
func Current() *Build { return New(Revision()) }

// Set replaces what the Build reports.
func (b *Build) Set(rev, why string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.info = Info{Revision: rev, Why: why}
}

// Info is what the Build reports now. The nil Build has no revision.
func (b *Build) Info() Info {
	if b == nil {
		return Info{Why: "this build keeps no revision"}
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.info
}
