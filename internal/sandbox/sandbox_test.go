package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/guard"
	"github.com/alexberardi/jarvis-loop/internal/slack"
	"github.com/alexberardi/jarvis-loop/internal/slackfake"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// The test binary doubles as the fake claude and loop-guard (LOOP_TEST_AS), as in
// the runner's tests: the fake claude runs the real guard as its PreToolUse hook.
func TestMain(m *testing.M) {
	switch os.Getenv("LOOP_TEST_AS") {
	case "claude":
		os.Exit(fakeclaude.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	case "guard":
		path, err := guard.ConfigPath(os.Args[1:], os.Getenv)
		if err != nil {
			os.Exit(2)
		}
		os.Exit(guard.Hook(path, os.Stdin, os.Stdout, os.Stderr, time.Now))
	}
	// git's detached "maintenance run --auto" after a commit races t.TempDir's RemoveAll
	// ("mirror.git: directory not empty"), so every git the tests start gets it switched off.
	for k, v := range map[string]string{"GIT_CONFIG_COUNT": "2", "GIT_CONFIG_KEY_0": "gc.auto", "GIT_CONFIG_VALUE_0": "0",
		"GIT_CONFIG_KEY_1": "maintenance.auto", "GIT_CONFIG_VALUE_1": "false"} {
		os.Setenv(k, v)
	}
	os.Exit(m.Run())
}

func testBinaries(t *testing.T) Binaries {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Binaries{Claude: self, Guard: self, Env: []string{"LOOP_TEST_AS=claude", raceNoExitSleep}}
}

func tempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// seedDB is a once-migrated loop.db for the whole test binary (as in the runner's tests):
// each stack's loopd boot otherwise runs every migration through race-instrumented SQLite,
// and this package ran within 40 s of CI's 12 m go-test timeout.
var seedDB struct {
	once sync.Once
	data []byte
	err  error
}

// seedStore pre-writes the migrated loop.db where the stack at stackDir opens its store, so
// store.Open finds every migration applied and only re-stamps the cheap version rows. If the
// stack's layout ever moves, the file is never read and the boot migrates as before.
func seedStore(t *testing.T, stackDir string) {
	t.Helper()
	seedDB.once.Do(func() {
		tmp, err := os.MkdirTemp("", "loop-seed-")
		if err != nil {
			seedDB.err = err
			return
		}
		defer os.RemoveAll(tmp)
		st, err := store.Open(tmp, clock.Real{})
		if err != nil {
			seedDB.err = err
			return
		}
		if err := st.Close(); err != nil {
			seedDB.err = err
			return
		}
		// Closing the last connection checkpoints and removes the WAL, so loop.db is whole.
		if _, err := os.Stat(filepath.Join(tmp, "loop.db-wal")); err == nil {
			seedDB.err = fmt.Errorf("seed store left a WAL behind; refusing a partial template")
			return
		}
		seedDB.data, seedDB.err = os.ReadFile(filepath.Join(tmp, "loop.db"))
	})
	if seedDB.err != nil {
		t.Fatalf("seeding the template store: %v", seedDB.err)
	}
	dir := filepath.Join(stackDir, "loopd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "loop.db"), seedDB.data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Every scenario in scenarios/ passes: the same run CI does with scripts/loop-sandbox run.
func TestScenarios(t *testing.T) {
	scs, err := LoadScenarios([]string{filepath.Join("..", "..", "scenarios")})
	if err != nil {
		t.Fatal(err)
	}
	if len(scs) < 9 {
		t.Fatalf("%d scenarios, want the 9 starters at least", len(scs))
	}
	bin := testBinaries(t)
	for _, sc := range scs {
		t.Run(sc.Name, func(t *testing.T) {
			t.Parallel() // each scenario has its own dir and ports; one after another, -race ran past CI's 12 m
			dir := tempDir(t)
			if keep := os.Getenv("SANDBOX_KEEP"); keep != "" { // keep the run's record for a look
				dir = filepath.Join(keep, sc.Name)
				os.RemoveAll(dir)
			}
			seedStore(t, filepath.Join(dir, "stack"))
			res := RunScenario(sc, dir, bin)
			if !res.Pass {
				tr, _ := os.ReadFile(filepath.Join(dir, "transcript.md"))
				logs, _ := os.ReadFile(filepath.Join(dir, "stack.log"))
				t.Fatalf("%s\n\n%s\n\nlog tail:\n%s", strings.Join(res.Failures, "\n"), tr, tail(string(logs), 40))
			}
		})
	}
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// The scenario runner itself: what should fail does, and says why in words.
func TestFailingScenariosFail(t *testing.T) {
	bin := testBinaries(t)
	add := Step{Do: "add", Item: "a", Title: "Shorten the session timeout"}
	expect := func(e Expect) Step { e.Within = "300ms"; return Step{Do: "expect", Expect: e} }
	cases := []struct {
		name  string
		steps []Step
		seed  func(t *testing.T, s *Stack)
		want  []string
	}{
		{"wrong stage", []Step{add, expect(Expect{Stage: map[string]string{"a": "build"}})}, nil,
			[]string{"step 2 (expect)", `a (#1) is triage, want build`}},
		{"missing message", []Step{add, expect(Expect{Messages: []MsgMatch{{Thread: "a", Text: "^Nope$"}}})}, nil,
			[]string{`message 1 {thread="a" text="^Nope$"} not found`, "there:"}},
		{"messages out of order", []Step{add, {Do: "triage", Item: "a"}, expect(Expect{Messages: []MsgMatch{
			{Thread: "a", Text: "^Reproduced"}, {Thread: "a", Text: "^Claimed for triage"}}})}, nil,
			[]string{`message 2 {thread="a" text="^Claimed for triage"} not found after the previous one`}},
		{"forbidden message", []Step{add, expect(Expect{NoMessages: []MsgMatch{{Parent: "a", Text: "Shorten"}}})}, nil,
			[]string{`exists, want none`, "Shorten the session timeout"}},
		{"missing event", []Step{add, expect(Expect{Events: []EventMatch{{Item: "a", Kind: "approved", Actor: "approver"}}})}, nil,
			[]string{"no event {item=a kind=approved actor=approver}", "events: created by user:sandbox"}},
		{"unknown step", []Step{{Do: "dance"}}, nil, []string{`unknown step "dance"`}},
		{"unknown item", []Step{{Do: "triage", Item: "zz"}}, nil, []string{`no item "zz"`}},
		{"a press on a button that is not there", []Step{add, {Do: "press", Item: "a", Button: "approve", Expect: Expect{Within: "300ms"}}}, nil,
			[]string{"no message with approve to press"}},
		{"a message outside the three channels", []Step{add}, func(t *testing.T, s *Stack) {
			api := slack.NewAPI(s.Slack.BaseURL(), s.Secrets[3], slack.Channels{"C0GENERAL"})
			if _, err := api.PostMessage(context.Background(), slack.Message{Channel: "C0GENERAL", Text: "hello"}); err != nil {
				t.Fatal(err)
			}
		}, []string{"must not: chat.postMessage to #C0GENERAL (not an allowed channel), outside #loop, #loop-digest and #plan"}},
		{"a reaction outside the three channels", []Step{add}, func(t *testing.T, s *Stack) {
			api := slack.NewAPI(s.Slack.BaseURL(), s.Secrets[3], slack.Channels{"C0GENERAL"})
			api.ReactionsAdd(context.Background(), "C0GENERAL", "1.1", slack.ReactionDone)
		}, []string{"must not: reactions.add to #C0GENERAL (not an allowed channel), outside #loop, #loop-digest and #plan"}},
		{"a token in a message", []Step{add}, func(t *testing.T, s *Stack) {
			api := slack.NewAPI(s.Slack.BaseURL(), s.Secrets[3], slack.Channels{slack.ChannelLoop})
			api.PostMessage(context.Background(), slack.Message{Channel: slack.ChannelLoop, Text: "debug: " + s.Secrets[1]})
		}, []string{"must not: chat.postMessage carries a token", "must not: a message in #loop shows a token"}},
		{"an unexpected actor", []Step{add}, func(t *testing.T, s *Stack) {
			s.Store.Record(context.Background(), store.Event{ItemID: 0, Actor: "slack:U0STRANGER", Kind: "control"})
		}, []string{`must not: event control (#0) by unexpected actor "slack:U0STRANGER"`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := tempDir(t)
			seedStore(t, dir)
			s, err := StartStack(Options{Dir: dir, Bin: bin})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if c.seed != nil {
				c.seed(t, s)
			}
			res := Play(&Scenario{Name: "bad", Steps: c.steps}, s)
			if res.Pass {
				t.Fatal("a scenario that should fail passed")
			}
			all := strings.Join(res.Failures, "\n")
			for _, w := range c.want {
				if !strings.Contains(all, w) {
					t.Errorf("failure lacks %q:\n%s", w, all)
				}
			}
		})
	}
}

func TestLoadScenarioRefusesUnknownFields(t *testing.T) {
	p := filepath.Join(tempDir(t), "typo.json")
	os.WriteFile(p, []byte(`{"name": "typo", "steps": [{"do": "expect", "stagee": {"a": "done"}}]}`), 0o644)
	if _, err := LoadScenario(p); err == nil || !strings.Contains(err.Error(), "stagee") {
		t.Fatalf("a typo was accepted: %v", err)
	}
}

func TestTranscript(t *testing.T) {
	msgs := []slackfake.Message{
		{Seq: 1, Channel: slack.ChannelLoop, TS: "1.1", User: slackfake.BotUser, Kind: "message", Text: "#1 x", Edits: 2, Blocks: []map[string]any{
			{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "*#1 x*\nStage *held*"}, "accessory": map[string]any{
				"type": "static_select", "action_id": "priority", "initial_option": map[string]any{"text": map[string]any{"text": "P3 medium"}}}}}},
		{Seq: 2, Channel: slack.ChannelLoop, TS: "1.2", ThreadTS: "1.1", User: slackfake.BotUser, Kind: "message", Text: "held", Blocks: []map[string]any{
			{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "#1 is held"}},
			{"type": "actions", "elements": []any{
				map[string]any{"type": "button", "action_id": "approve", "text": map[string]any{"text": "Approve merge"}},
				map[string]any{"type": "button", "action_id": "keep_holding", "text": map[string]any{"text": "Keep holding"}}}}}},
		{Seq: 3, Channel: slack.ChannelLoop, TS: "1.3", ThreadTS: "1.1", User: slack.Approver, Kind: "message", Text: "lgtm"},
		{Seq: 4, Channel: slack.ChannelLoop, TS: "1.4", User: slackfake.BotUser, To: Other, Kind: "ephemeral", Text: "Sorry"},
		{Seq: 5, Channel: slack.ChannelDigest, TS: "1.5", User: slackfake.BotUser, Kind: "message", Text: "Active agents: 4 of 6", Pinned: true},
		{Seq: 6, Channel: slack.ChannelLoop, TS: "1.6", ThreadTS: "9.9", User: slackfake.BotUser, Kind: "message", Text: "another thread"},
	}
	want := "#loop\n" +
		"  jarvis-loop (edited 2×): *#1 x*\n" +
		"    Stage *held*  [P3 medium ▾]\n" +
		"    ↳ jarvis-loop: #1 is held\n" +
		"      [Approve merge] [Keep holding]\n" +
		"    ↳ you: lgtm\n" +
		"  jarvis-loop (only someone else sees this): Sorry\n" +
		"\n" +
		"#loop-digest\n" +
		"  jarvis-loop (pinned): Active agents: 4 of 6\n"
	if got := Render(msgs, ""); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	one := Render(msgs, "1.1")
	if !strings.Contains(one, "you: lgtm") || strings.Contains(one, "Sorry") || strings.Contains(one, "Active agents") {
		t.Fatalf("one thread:\n%s", one)
	}
}

// raceNoExitSleep: a -race binary sleeps 1 s at exit by default (atexit_sleep_ms), and each
// agent run starts the test binary several times (the fake claude, a guard per tool call).
const raceNoExitSleep = "GORACE=atexit_sleep_ms=0"
