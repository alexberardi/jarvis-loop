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
	withRepos := func(r RunnerSeen, repos ...string) RunnerSeen {
		r.Repos = repos
		return r
	}
	mbp := func(repos ...string) RunnerSeen { return withRepos(seen("mbp", time.Minute, "macos"), repos...) }
	cases := []struct {
		name      string
		resources []string
		repo      string
		runners   []RunnerSeen
		want      []string
	}{
		{"no resources, no runners", nil, "", nil, nil},
		{"no resources", nil, "", []RunnerSeen{devbox}, nil},
		{"no runners", []string{"macos"}, "", nil, []string{"macos"}},
		{"devbox has no macos", []string{"macos"}, "", []RunnerSeen{devbox}, []string{"macos"}},
		{"devbox has emulator and gpu", []string{"emulator", "gpu"}, "", []RunnerSeen{devbox}, nil},
		{"only the missing one is named", []string{"gpu", "macos"}, "", []RunnerSeen{devbox}, []string{"macos"}},
		{"macos runner seen 2d ago", []string{"macos"}, "", []RunnerSeen{devbox, seen("mini", 48*time.Hour, "macos")}, []string{"macos"}},
		{"macos runner seen within the window", []string{"macos"}, "", []RunnerSeen{devbox, seen("mini", RunnerSeenWindow, "macos")}, nil},
		{"one runner per resource, none with both", []string{"gpu", "macos"}, "",
			[]RunnerSeen{devbox, seen("mini", time.Minute, "macos")}, []string{"gpu", "macos"}},
		// The runner's repos (#467): a runner serves an item only with its repo checked out (a
		// runner that sent no repos predates them: any repo).
		{"macos runner without the repo", []string{"macos"}, "o/mobile", []RunnerSeen{devbox, mbp("o/server")}, []string{"macos", "o/mobile"}},
		{"macos runner with the repo", []string{"macos"}, "o/mobile", []RunnerSeen{devbox, mbp("o/server", "o/mobile")}, nil},
		{"macos runner that sent no repos", []string{"macos"}, "o/mobile", []RunnerSeen{devbox, mbp()}, nil},
		{"the repo is on another runner, without macos", []string{"macos"}, "o/mobile", []RunnerSeen{withRepos(devbox, "o/mobile"), mbp("o/server")}, []string{"macos", "o/mobile"}},
		{"no resources, no runner has the repo", nil, "o/mobile", []RunnerSeen{withRepos(devbox, "o/server")}, []string{"o/mobile"}},
		{"no resources, a runner has the repo", nil, "o/mobile", []RunnerSeen{withRepos(devbox, "o/server"), mbp("o/mobile")}, nil},
		{"no resources and no runners: not judged", nil, "o/mobile", nil, nil},
		{"macos missing, the repo is there", []string{"macos"}, "o/server", []RunnerSeen{withRepos(devbox, "o/server")}, []string{"macos"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Unservable(c.resources, c.repo, c.runners, now); !slices.Equal(got, c.want) {
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
