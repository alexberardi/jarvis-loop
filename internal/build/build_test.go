package build

import (
	"strings"
	"sync"
	"testing"
)

// The nil Build reports no revision and says so, rather than panicking: a handler that keeps no
// build (a test's, or a binary with nothing to report) is served by it.
func TestNilBuildHasNoRevision(t *testing.T) {
	var b *Build
	got := b.Info()
	if got.Revision != "" || got.Why == "" {
		t.Errorf("%+v, want no revision and a reason", got)
	}
}

func TestSetAndInfo(t *testing.T) {
	b := New("", "nothing yet")
	if got := b.Info(); got.Revision != "" || got.Why != "nothing yet" {
		t.Errorf("%+v", got)
	}
	b.Set(strings.Repeat("a", 40), "")
	if got := b.Info(); got.Revision != strings.Repeat("a", 40) || got.Why != "" {
		t.Errorf("%+v", got)
	}
	// Set clears the reason it replaces: a Build with a revision must not also say why it has none.
	b.Set("", "gone again")
	if got := b.Info(); got.Revision != "" || got.Why != "gone again" {
		t.Errorf("%+v", got)
	}
}

// loopd serves Info while the loop's harness calls Set, so the two must not race (the race
// detector is the point of this test).
func TestSetWhileReading(t *testing.T) {
	b := New("", "")
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(2)
		go func() { defer wg.Done(); b.Set(strings.Repeat(string(rune('a'+i)), 40), "") }()
		go func() { defer wg.Done(); b.Info() }()
	}
	wg.Wait()
	if len(b.Info().Revision) != 40 {
		t.Errorf("%+v", b.Info())
	}
}

// Current reads this test binary's own stamps. `go test` builds with vcs info off, so this asserts
// only the shape: either a revision, or none with a reason.
func TestCurrent(t *testing.T) {
	got := Current().Info()
	if got.Revision == "" && got.Why == "" {
		t.Error("no revision and no reason why not")
	}
	if got.Revision != "" && got.Why != "" {
		t.Errorf("%+v: a revision and a reason not to have one", got)
	}
}
