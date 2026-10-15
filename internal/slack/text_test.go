package slack

import (
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
