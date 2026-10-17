package pipeline

import (
	"slices"
	"testing"
	"time"
)

func TestUnservable(t *testing.T) {
	now := t0
	seen := func(name string, ago time.Duration, caps ...string) RunnerSeen {
		return RunnerSeen{Name: name, Capabilities: caps, LastSeen: now.Add(-ago)}
	}
	devbox := seen("devbox", time.Minute, "emulator", "gpu", "voice")
	cases := []struct {
		name      string
		resources []string
		runners   []RunnerSeen
		want      []string
	}{
		{"no resources, no runners", nil, nil, nil},
		{"no resources", nil, []RunnerSeen{devbox}, nil},
		{"no runners", []string{"macos"}, nil, []string{"macos"}},
		{"devbox has no macos", []string{"macos"}, []RunnerSeen{devbox}, []string{"macos"}},
		{"devbox has emulator and gpu", []string{"emulator", "gpu"}, []RunnerSeen{devbox}, nil},
		{"only the missing one is named", []string{"gpu", "macos"}, []RunnerSeen{devbox}, []string{"macos"}},
		{"macos runner seen 2d ago", []string{"macos"}, []RunnerSeen{devbox, seen("mini", 48*time.Hour, "macos")}, []string{"macos"}},
		{"macos runner seen within the window", []string{"macos"}, []RunnerSeen{devbox, seen("mini", RunnerSeenWindow, "macos")}, nil},
		{"one runner per resource, none with both", []string{"gpu", "macos"},
			[]RunnerSeen{devbox, seen("mini", time.Minute, "macos")}, []string{"gpu", "macos"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Unservable(c.resources, c.runners, now); !slices.Equal(got, c.want) {
				t.Fatalf("Unservable(%v) = %v, want %v", c.resources, got, c.want)
			}
		})
	}
}

func TestUnservableRoot(t *testing.T) {
	if got := UnservableRoot([]string{"macos"}); got != "no runner offers macos" {
		t.Fatalf("got %q", got)
	}
	if got := UnservableRoot([]string{"gpu", "macos"}); got != "no runner offers gpu + macos" {
		t.Fatalf("got %q", got)
	}
}
