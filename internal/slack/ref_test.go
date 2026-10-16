package slack

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alexberardi/jarvis-loop/internal/store"
)

const pantry = "Fix the pantry list <script>"

func TestRef(t *testing.T) {
	if got := ref(7, "  "+pantry+"\n"); got != "#7 Fix the pantry list &lt;script&gt;" {
		t.Fatalf("ref %q", got)
	}
	if got := ref(7, " "); got != "#7" {
		t.Fatalf("no title: %q", got)
	}
	long := strings.Repeat("ab cd ", 20) // 120 characters
	got := strings.TrimPrefix(ref(7, long), "#7 ")
	if utf8.RuneCountInString(got) > refTitleMax || !strings.HasSuffix(got, "…") || strings.Contains(got, "loopd") {
		t.Fatalf("long title %q (%d)", got, utf8.RuneCountInString(got))
	}
	// Cut before escaping, so no entity is split.
	got = ref(7, strings.Repeat("é", 78)+"<<<")
	if !strings.HasSuffix(got, "é&lt;…") {
		t.Fatalf("cut %q", got)
	}
}

// Every message that pings the approver names the item in its text (what the push notification
// shows) and in its blocks.
func TestPingsNameTheItem(t *testing.T) {
	want := func(t *testing.T, c call, prefix string) {
		t.Helper()
		if txt := fmt.Sprint(c.Body["text"]); !strings.Contains(txt, prefix) {
			t.Errorf("text %q lacks %q", txt, prefix)
		}
		blocks := strings.TrimPrefix(allText(c), fmt.Sprint(c.Body["text"]))
		if !strings.Contains(blocks, prefix) {
			t.Errorf("blocks %s lack %q", blocks, prefix)
		}
	}
	t.Run("needs you", func(t *testing.T) {
		e := newBotEnv(t)
		id := e.add(pantry)
		e.toNeedsYou(id, "which list?")
		cs := only(t, e.calls(), "chat.postMessage")
		want(t, cs[len(cs)-1], fmt.Sprintf("<@%s> #%d Fix the pantry list &lt;script&gt; needs you: which list?", Approver, id))
	})
	t.Run("held", func(t *testing.T) {
		e := newBotEnv(t)
		id := e.add(pantry)
		e.toHeld(id, shaA)
		cs := only(t, e.calls(), "chat.postMessage")
		want(t, cs[len(cs)-1], fmt.Sprintf("<@%s> #%d Fix the pantry list &lt;script&gt; is held for your approval", Approver, id))
	})
	t.Run("qa", func(t *testing.T) {
		e := newBotEnv(t)
		id := e.add(pantry)
		e.toQA(id)
		cs := only(t, e.calls(), "chat.postMessage")
		want(t, cs[len(cs)-1], fmt.Sprintf("<@%s> #%d Fix the pantry list &lt;script&gt;: Merged — waiting for your check", Approver, id))
	})
	t.Run("usage limit", func(t *testing.T) {
		e := newBotEnv(t)
		id := e.add(pantry)
		m := e.b.usageLimitMessage(e.ctx, store.Event{Kind: "control", Data: map[string]any{"item": float64(id), "error": "limit"}})
		if !strings.HasPrefix(m.Text, fmt.Sprintf("<@%s> Usage limit hit on #%d Fix the pantry list &lt;script&gt;: limit", Approver, id)) {
			t.Errorf("usage limit %q", m.Text)
		}
	})
}

func TestStatusLinesNameTheItem(t *testing.T) {
	e := newBotEnv(t)
	name := func(id int64) string { return fmt.Sprintf("#%d Fix the pantry list &lt;script&gt;", id) }

	a := e.add(pantry)
	if out := e.command(Approver, fmt.Sprintf("priority %d P2", a)); out != name(a)+" is now P2." {
		t.Errorf("priority %q", out)
	}
	if out := e.command(Approver, fmt.Sprintf("drop %d", a)); out != name(a)+" dropped (closed)." {
		t.Errorf("drop %q", out)
	}
	b := e.add(pantry)
	if out := e.command(Approver, fmt.Sprintf("qa %d", b)); out != name(b)+" waits in qa for your check once merged." {
		t.Errorf("qa %q", out)
	}
	e.toQA(b)
	if out := e.command(Approver, fmt.Sprintf("verified %d", b)); out != name(b)+" verified (done)." {
		t.Errorf("verified %q", out)
	}
	c := e.add(pantry)
	e.toQA(c)
	if out := e.command(Approver, fmt.Sprintf("reopen %d still <b>", c)); out != fmt.Sprintf("%s done; filed #%d (P2): Reopen #%d: still &lt;b&gt;.", name(c), c+1, c) {
		t.Errorf("reopen %q", out)
	}
	e.calls()
	e.press(Approver, ChannelLoop, "qa-ts", "qa_reopen", "qa", fmt.Sprint(c))
	if eph := only(t, e.calls(), "chat.postEphemeral"); len(eph) != 1 || fmt.Sprint(eph[0].Body["text"]) != name(c)+" is not waiting for your check." {
		t.Errorf("reopen button %+v", eph)
	}
}

func TestApprovalEchoesNameTheItem(t *testing.T) {
	e, id, ts := heldSetup(t)
	name := fmt.Sprintf("#%d auth fix", id)
	e.calls()
	e.reply(Approver, ts, "ship it")
	echo := only(t, e.calls(), "chat.postMessage")[0]
	e.press(Approver, ChannelLoop, "echo-ts", "cancel_approval", "pending", elementValue(echo, "cancel_approval"))
	up := updatesTo(t, e.calls(), "echo-ts")
	if len(up) != 1 || fmt.Sprint(up[0].Body["text"]) != "Cancelled: 'ship it' was not applied. "+name+" stays held." {
		t.Fatalf("cancel %+v", up)
	}
	e.press(Approver, ChannelLoop, "held-ts", "approve", "held", fmt.Sprintf("%d:%s", id, shaA))
	up = updatesTo(t, e.calls(), "held-ts")
	if len(up) != 1 || fmt.Sprint(up[0].Body["text"]) != name+": approved by you for `aaaaaaa` (button)." {
		t.Fatalf("approve %+v", up)
	}
}

func updatesTo(t *testing.T, cs []call, ts string) []call {
	var out []call
	for _, c := range only(t, cs, "chat.update") {
		if c.Body["ts"] == ts {
			out = append(out, c)
		}
	}
	return out
}
