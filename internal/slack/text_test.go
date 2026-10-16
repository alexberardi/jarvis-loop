package slack

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClassifyReply(t *testing.T) {
	cases := []struct {
		reply   string
		approve bool
	}{
		// The spec's table.
		{"yea, removing that old test is fine", true},
		{"looks fine?", false},
		{"don't merge yet", false},
		{"ok wait", false},
		{"LGTM", true},
		{"fine by me", true},
		{"no", false},
		// Approval words and phrases.
		{"approve", true},
		{"Approved.", true},
		{"lgtm!", true},
		{"ship it", true},
		{"go ahead", true},
		{"yes, merge it", true},
		{"okay", true},
		{"ok", true},
		{"Yep", true},
		{"sure", true},
		{"do it", true},
		{"<@U0BOT> lgtm", true},
		{"yea that's fine", true},
		// Not consent.
		{"", false},
		{"thanks", false},
		{"merge", false}, // a bare verb is not one of the phrases
		{"what does this change", false},
		{"is this fine", false}, // a question without the mark
		{"can you merge it", false},
		{"not yet", false},
		{"I do not approve", false},
		{"hold off", false},
		{"holding for now", false},
		{"wait for CI", false},
		{"stop", false},
		{"nope", false},
		{"yes, no problem", false}, // "no" anywhere: the safe side
		{"don’t merge, it's fine otherwise", false},
		{"dont", false},
		{"fine, but it shouldn't merge before Friday", false},
		{"sure?", false},
		{"approve? not sure", false},
		{"okay?!", false},
		{"cancel that, ok", false},
		{"never mind, fine", false},
		{"fine-tune it first", false}, // "fine" only as its own word
		{"yesterday's run", false},
	}
	for _, c := range cases {
		if got := ClassifyReply(c.reply); got != c.approve {
			t.Errorf("ClassifyReply(%q) = %v, want %v", c.reply, got, c.approve)
		}
	}
}

// A held reply that is not an approval is a note for the builder unless it asks, waits or says
// nothing to do.
func TestIsHeldNote(t *testing.T) {
	cases := []struct {
		reply string
		note  bool
	}{
		{"rebuild", true},
		{"rebase and fix the typo in the heading", true},
		{"Rebuild, and fix the typo", true},
		{"<@U0BOT> rename the flag to --dry", true},
		{"fine-tune it first", true},
		{"merge it after adding a test for the empty case", true},
		{"merge main and rebuild", true},
		{"please update the docs", true},
		{"Use the store's clock", true},
		{"actually, rebuild with the new name", true},
		{"merge after removing the flag", true},
		{"merge once dropping the old route", true},
		// Consent words before a change request: still a note (the bot checks this before approval).
		{"ok, rebuild", true},
		{"okay fix the typo", true},
		{"actually ok, rebase", true},
		{"yes, but rebase first", true},
		{"sure, use the store's clock", true},
		// Stays held.
		{"", false},
		{"looks fine?", false},
		{"is the test still there", false},
		{"what does this change", false},
		{"don't merge yet", false},
		{"don’t merge yet", false},
		{"ok wait", false},
		{"no", false},
		{"not yet", false},
		{"hold off", false},
		{"stop", false},
		{"fix it but don't touch the store", false}, // a negation anywhere: the safe side
		{"thanks", false},
		{"Thank you!", false},
		{"merge", false},
		{"merge it please", false},
		{"merge it", false},
		{"ok", false},
		{"yes", false},
		{"sure, go ahead", false},
		{"lgtm", false},
		{"ship it", false},
		{"approve", false},
		// Praise and stalling ask for no change: a note must start with a change verb.
		{"looks good", false},
		{"sounds good", false},
		{"good", false},
		{"perfect", false},
		{"awesome", false},
		{"nice work", false},
		{"great job", false},
		{"nice fix", false},
		{":+1:", false},
		{"go", false},
		{"ship", false},
		{"later", false},
		{"tomorrow", false},
		{"I'll look tonight", false},
		{"let me check", false},
		{"hmm", false},
		{"on second thought", false},
		{"merge it now", false},
	}
	for _, c := range cases {
		if got := IsHeldNote(c.reply); got != c.note {
			t.Errorf("IsHeldNote(%q) = %v, want %v", c.reply, got, c.note)
		}
	}
}

func TestParsePriority(t *testing.T) {
	cases := []struct {
		text, priority, title string
	}{
		{"add dark mode", "P3", "add dark mode"},
		{"this is a high priority feature", "P2", "this is a feature"},
		{"urgent: main is red", "P1", "main is red"},
		{"main is red, urgent", "P1", "main is red"},
		{"highest priority fix the build", "P1", "fix the build"},
		{"high-priority: timers drift", "P2", "timers drift"},
		{"medium priority tidy the logs", "P3", "tidy the logs"},
		{"low priority: rename the flag", "P4", "rename the flag"},
		{"lowest priority clean up docs", "P5", "clean up docs"},
		{"someday: a widget", "P5", "a widget"},
		{"rewrite the parser someday", "P5", "rewrite the parser"},
		{"low: rename the flag", "P4", "rename the flag"},
		{"rename the flag, low", "P4", "rename the flag"},
		{"timers drift - high", "P2", "timers drift"},
		{"P2 timers drift", "P2", "timers drift"},
		{"timers drift (p1)", "P1", "timers drift"},
		// Words that only describe the work do not set the priority.
		{"fix the low battery warning", "P3", "fix the low battery warning"},
		{"high CPU on the dev box", "P3", "high CPU on the dev box"},
		{"slow medium-size uploads", "P3", "slow medium-size uploads"},
		// The first marker wins; all markers leave the title.
		{"urgent, low priority thing", "P1", "thing"},
		{"  spaced   out  ", "P3", "spaced out"},
		{"urgent", "P1", ""},
	}
	for _, c := range cases {
		p, title := ParsePriority(c.text)
		if p != c.priority || title != c.title {
			t.Errorf("ParsePriority(%q) = %q, %q; want %q, %q", c.text, p, title, c.priority, c.title)
		}
	}
}

func TestClipAndEscape(t *testing.T) {
	if got := Escape("a <@U1> & <!channel> > b"); got != "a &lt;@U1&gt; &amp; &lt;!channel&gt; &gt; b" {
		t.Fatalf("escape: %q", got)
	}
	short := "short"
	if Clip(short, 2000, "") != short {
		t.Fatal("short text changed")
	}
	long := strings.Repeat("é", 3000)
	got := Clip(long, 2000, "loop show 12")
	if utf8.RuneCountInString(got) > 2000 || !utf8.ValidString(got) {
		t.Fatalf("clip: %d runes, valid %v", utf8.RuneCountInString(got), utf8.ValidString(got))
	}
	if !strings.HasSuffix(got, "… (full text on loopd: loop show 12)") {
		t.Fatalf("clip suffix: %q", got[len(got)-60:])
	}
	if got := Clip(long, 2000, ""); !strings.HasSuffix(got, "… (full text on loopd)") {
		t.Fatalf("clip without a hint: %q", got[len(got)-40:])
	}
}

// The repo words of an add request: a configured repo named as "in <name>", "<name>:" at the
// start or "[<name>]" sets the item's repo and leaves the title; anything else leaves the repo
// empty, for triage to decide.
func TestParseRepo(t *testing.T) {
	repos := []string{"alexberardi/jarvis-loop", "alexberardi/jarvis-server"}
	cases := []struct {
		text, repo, title string
	}{
		{"show the item's repo in /loop show in jarvis-loop", "alexberardi/jarvis-loop", "show the item's repo in /loop show"},
		{"jarvis-loop: show the repo", "alexberardi/jarvis-loop", "show the repo"},
		{"[jarvis-server] timers drift", "alexberardi/jarvis-server", "timers drift"},
		{"timers drift [jarvis-server]", "alexberardi/jarvis-server", "timers drift"},
		{"[ alexberardi/jarvis-loop ] tidy the digest", "alexberardi/jarvis-loop", "tidy the digest"},
		{"fix the digest in Jarvis-Loop, high priority", "alexberardi/jarvis-loop", "fix the digest, high priority"},
		{"in jarvis-server: the timer rounds down", "alexberardi/jarvis-server", "the timer rounds down"},
		{"JARVIS-SERVER: the timer rounds down", "alexberardi/jarvis-server", "the timer rounds down"},
		{"[jarvis-loop]", "alexberardi/jarvis-loop", ""},
		// The first marker wins; only its words leave the title.
		{"jarvis-loop: move the digest in jarvis-server", "alexberardi/jarvis-loop", "move the digest in jarvis-server"},
		// Not a marker: a repo named in passing, a longer name, another repo, a possessive.
		{"jarvis-loop is slow", "", "jarvis-loop is slow"},
		{"add a jarvis-loop: prefix to messages", "", "add a jarvis-loop: prefix to messages"},
		{"fix the leak in jarvis-loopd", "", "fix the leak in jarvis-loopd"},
		{"fix the leak in jarvis-mobile", "", "fix the leak in jarvis-mobile"},
		{"fix the leak in jarvis-loop's runner", "", "fix the leak in jarvis-loop's runner"},
		{"fix the leak in jarvis-loop/runner", "", "fix the leak in jarvis-loop/runner"},
		{"[jarvis-mobile] dark mode", "", "[jarvis-mobile] dark mode"},
		{"plain text", "", "plain text"},
	}
	for _, c := range cases {
		repo, title := ParseRepo(c.text, repos, nil)
		if repo != c.repo || title != c.title {
			t.Errorf("ParseRepo(%q) = %q, %q; want %q, %q", c.text, repo, title, c.repo, c.title)
		}
	}
	// No configured repos: nothing is a marker.
	if repo, title := ParseRepo("jarvis-loop: x", nil, nil); repo != "" || title != "jarvis-loop: x" {
		t.Errorf("no repos: %q %q", repo, title)
	}
	// A short name two owners share is ambiguous: only the full slug counts.
	two := []string{"a/tools", "b/tools"}
	if repo, _ := ParseRepo("tools: x", two, nil); repo != "" {
		t.Errorf("ambiguous short name taken as %q", repo)
	}
	if repo, title := ParseRepo("b/tools: x", two, nil); repo != "b/tools" || title != "x" {
		t.Errorf("full slug: %q %q", repo, title)
	}
}

// Repo aliases (docs/phases/06b-all-repos.md): configured words and a repo's short name count only
// as a leading "[x]" or "x:", or (configured ones) a trailing "in x"; never words in the middle.
func TestParseRepoAliases(t *testing.T) {
	const (
		node    = "alexberardi/jarvis-node-mobile"
		recipes = "alexberardi/jarvis-recipes-mobile"
		docs    = "alexberardi/jarvis-docs"
		inst    = "alexberardi/jarvis-installer"
		server  = "alexberardi/jarvis-server"
		nest    = "alexberardi/jarvis-device-nest"
		weather = "alexberardi/jarvis-cmd-weather"
		wdev    = "alexberardi/jarvis-device-weather"
	)
	repos := []string{node, recipes, docs, inst, server, "alexberardi/jarvis-loop", nest, weather, wdev, "alexberardi/jarvis-cmd-timer"}
	aliases := map[string]string{"mobile": node, "node app": node, "Recipes  App": recipes, "docs": docs, "installer": inst, "server": server}
	cases := []struct {
		text, repo, title string
	}{
		// Configured aliases, leading or trailing.
		{"[mobile] the home screen flickers", node, "the home screen flickers"},
		{"mobile: the home screen flickers", node, "the home screen flickers"},
		{"[node app] dark mode", node, "dark mode"},
		{"Node  App: dark mode", node, "dark mode"},
		{"the home screen flickers in mobile", node, "the home screen flickers"},
		{"the list scrolls badly in the recipes app", "", "the list scrolls badly in the recipes app"},
		{"the list scrolls badly in recipes app", recipes, "the list scrolls badly"},
		{"the list scrolls badly in recipes app, high priority", recipes, "the list scrolls badly, high priority"},
		{"fix the typo on the install page in docs.", docs, "fix the typo on the install page."},
		{"[installer] the wizard skips step 3", inst, "the wizard skips step 3"},
		{"server: timers drift", server, "timers drift"},
		// Never in the middle of a sentence.
		{"make the mobile layout denser", "", "make the mobile layout denser"},
		{"fix the crash in mobile when offline", "", "fix the crash in mobile when offline"},
		{"add [mobile] tags to the docs", "", "add [mobile] tags to the docs"},
		{"the docs: are wrong about x", "", "the docs: are wrong about x"},
		{"move the docs into the server", "", "move the docs into the server"},
		// A package by its suffix, when only one repo has it, only as a leading [x] or x:.
		{"[nest] the thermostat reads 0", nest, "the thermostat reads 0"},
		{"nest: the thermostat reads 0", nest, "the thermostat reads 0"},
		{"timer: rounds down", "alexberardi/jarvis-cmd-timer", "rounds down"},
		{"the thermostat reads 0 in nest", "", "the thermostat reads 0 in nest"},
		{"[weather] rain is wrong", "", "[weather] rain is wrong"}, // a command and a device share it
		{"[jarvis-cmd-weather] rain is wrong", weather, "rain is wrong"},
		{"[loop] tidy the digest", "alexberardi/jarvis-loop", "tidy the digest"},
		// Full names still count where they did; the first marker wins.
		{"fix the leak in jarvis-server where timers drift", server, "fix the leak where timers drift"},
		{"mobile: move the digest in jarvis-server", node, "move the digest in jarvis-server"},
		{"plain text", "", "plain text"},
	}
	for _, c := range cases {
		repo, title := ParseRepo(c.text, repos, aliases)
		if repo != c.repo || title != c.title {
			t.Errorf("ParseRepo(%q) = %q, %q; want %q, %q", c.text, repo, title, c.repo, c.title)
		}
	}
	// An alias that is a configured repo's own name means that repo.
	if repo, _ := ParseRepo("[jarvis-docs] x", repos, map[string]string{"jarvis-docs": node}); repo != docs {
		t.Errorf("a name aliased elsewhere: %q", repo)
	}
}

func TestParseAfter(t *testing.T) {
	cases := []struct {
		text, rest string
		after      []int64
	}{
		{"rename the flag", "rename the flag", nil},
		{"rename the flag after #12", "rename the flag", []int64{12}},
		{"rename the flag after #12, #13", "rename the flag", []int64{12, 13}},
		{"rename the flag after #12 and #13, urgent", "rename the flag, urgent", []int64{12, 13}},
		{"After #4: rename the flag", "rename the flag", []int64{4}},
		{"rename the flag (after #12,#13) in jarvis-loop", "rename the flag in jarvis-loop", []int64{12, 13}},
		// Not a dependency: no "#".
		{"retry after 5 minutes", "retry after 5 minutes", nil},
		{"clear the cache after login", "clear the cache after login", nil},
	}
	for _, c := range cases {
		after, rest := ParseAfter(c.text)
		if rest != c.rest || !slices.Equal(after, c.after) {
			t.Errorf("ParseAfter(%q) = %v, %q; want %v, %q", c.text, after, rest, c.after, c.rest)
		}
	}
}
