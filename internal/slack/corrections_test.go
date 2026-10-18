package slack

import (
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/store"
)

// The user's corrections from the CLI (loop edit, link, unlink, resources) read in the thread.
func TestCorrectionLines(t *testing.T) {
	e := newOrchEnv(t)
	dep := e.add("the dependency")
	id := e.add("x")
	it := e.item(id).Item
	for _, c := range []struct {
		ev   store.Event
		want string
	}{
		{store.Event{Kind: "edited", Data: map[string]any{"title": map[string]any{"from": "y", "to": "x"}, "ask": map[string]any{"from": "", "to": "z"}}},
			"Title: x; the ask changed (you (alex, CLI))."},
		{store.Event{Kind: "edited", Data: map[string]any{"ask": map[string]any{"from": "", "to": "z"}}},
			"The ask changed (you (alex, CLI))."},
		{store.Event{Kind: store.KindLinked, Data: map[string]any{"after": []any{float64(dep), float64(9)}}},
			"Now waits on #1 the dependency, #9 (you (alex, CLI))."},
		{store.Event{Kind: "unlinked", Data: map[string]any{"after": float64(dep)}},
			"No longer waits on #1 the dependency (you (alex, CLI))."},
		{store.Event{Kind: "resources", Data: map[string]any{"from": []any{"macos"}, "to": []any{"gpu", "voice"}}},
			"Resources macos → gpu, voice (you (alex, CLI))."},
		{store.Event{Kind: "resources", Data: map[string]any{"from": []any{"macos"}, "to": []any{}}},
			"Resources macos → none (you (alex, CLI))."},
	} {
		c.ev.ItemID, c.ev.Actor = id, "user:alex"
		m, ok := e.b.describe(e.ctx, c.ev, store.Detail{Item: it})
		if !ok || m.Text != c.want {
			t.Errorf("%s: got %q, want %q", c.ev.Kind, m.Text, c.want)
		}
	}
}
