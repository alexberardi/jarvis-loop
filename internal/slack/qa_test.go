package slack

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

const shaM = "cccccccccccccccccccccccccccccccccccccccc"

// toQA takes an item through review and land, merged with needs_qa (a repo with qa: true).
func (e *botEnv) toQA(id int64) {
	e.t.Helper()
	e.runner(id, "a1", pipeline.Reproduced, map[string]string{"area": "app", "size": "S", "risk": "normal", "resources": ""})
	e.runner(id, "builder", pipeline.PROpened, map[string]string{"repo": "jarvis-server", "pr": "142", "branch": "fix/x", "head_sha": shaA},
		"M app/screen.tsx")
	e.runner(id, "reviewer", pipeline.Approved, map[string]string{"summary": "tests cover it", "head_sha": shaA})
	e.runner(id, "lander", pipeline.Merged, map[string]string{"merge_sha": shaM, "head_sha": shaA, "needs_qa": "true"}, "M app/screen.tsx")
	if s := e.item(id).Item.Stage; s != pipeline.QA {
		e.t.Fatalf("stage %s, want qa", s)
	}
}

func TestQAMessage(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("bigger pairing code")
	e.toQA(id)
	e.drain()
	cs := only(t, e.calls(), "chat.postMessage")
	msg := cs[len(cs)-1]
	txt := allText(msg)
	for _, want := range []string{"<@" + Approver + ">", "Merged — waiting for your check", "pull/142", "`ccccccc`"} {
		if !strings.Contains(txt, want) {
			t.Errorf("qa message lacks %q: %s", want, txt)
		}
	}
	if ids := actionIDs(msg); len(ids) != 2 || ids[0] != "qa_verified" || ids[1] != "qa_reopen" {
		t.Fatalf("buttons %v", ids)
	}
	if v := elementValue(msg, "qa_reopen"); v != fmt.Sprint(id) {
		t.Fatalf("reopen value %q", v)
	}
	if d := e.item(id); d.Display != "qa" {
		t.Fatalf("display %q", d.Display)
	}
}

func TestVerifiedButton(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("bigger pairing code")
	e.toQA(id)
	e.drain()
	e.calls()
	e.press(other, ChannelLoop, "qa-ts", "qa_verified", "qa", fmt.Sprint(id))
	if d := e.item(id); d.Item.Stage != pipeline.QA || lastEvent(d).Kind != "refused" || lastEvent(d).Actor != "slack:"+other {
		t.Fatalf("someone else verified: %s %+v", d.Item.Stage, lastEvent(d))
	}
	e.press(Approver, ChannelLoop, "qa-ts", "qa_verified", "qa", fmt.Sprint(id))
	d := e.item(id)
	if d.Item.Stage != pipeline.Done || lastEvent(d).Kind != "verified" || lastEvent(d).Data["words"] != "Verified button" {
		t.Fatalf("verified: %s %+v", d.Item.Stage, lastEvent(d))
	}
	e.drain()
	var lines []string
	for _, c := range only(t, e.calls(), "chat.postMessage") {
		if txt := fmt.Sprint(c.Body["text"]); !strings.HasPrefix(txt, "Refused") {
			lines = append(lines, txt)
		}
	}
	if len(lines) != 1 || lines[0] != "Verified by you. Done." {
		t.Fatalf("thread lines %q", lines)
	}
}

// Reopen asks for the words in the thread; the approver's next reply there files a P2 item
// linked to this one, and this one is done.
func TestReopenButtonThenReply(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("bigger pairing code")
	e.toQA(id)
	e.drain()
	ts := e.thread(id)
	e.calls()

	// A reply before Reopen is not taken.
	e.reply(Approver, ts, "hmm, checking tonight")
	cs := only(t, e.calls(), "chat.postMessage")
	if len(cs) != 1 || !strings.Contains(allText(cs[0]), "Not taken") || e.item(id).Item.Stage != pipeline.QA {
		t.Fatalf("reply before Reopen: %q, stage %s", allText(cs[0]), e.item(id).Item.Stage)
	}
	// Someone else's Reopen is refused.
	e.press(other, ChannelLoop, "qa-ts", "qa_reopen", "qa", fmt.Sprint(id))
	if d := e.item(id); d.Item.Stage != pipeline.QA || lastEvent(d).Kind != "refused" {
		t.Fatalf("someone else reopened: %+v", lastEvent(d))
	}
	e.calls()
	e.press(Approver, ChannelLoop, "qa-ts", "qa_reopen", "qa", fmt.Sprint(id))
	cs = only(t, e.calls(), "chat.postMessage")
	if len(cs) != 1 || cs[0].Body["thread_ts"] != ts || !strings.Contains(allText(cs[0]), "Reply in this thread with what is wrong") {
		t.Fatalf("reopen prompt: %+v", cs)
	}
	if e.item(id).Item.Stage != pipeline.QA {
		t.Fatal("Reopen without words moved the item")
	}
	// Someone else's reply is refused, not taken as the words.
	e.reply(other, ts, "it's fine")
	if d := e.item(id); d.Item.Stage != pipeline.QA || lastEvent(d).Kind != "refused" {
		t.Fatalf("someone else's words: %+v", lastEvent(d))
	}
	e.calls()
	e.reply(Approver, ts, "the code is still tiny on the Pixel")
	d := e.item(id)
	ev := lastEvent(d)
	if d.Item.Stage != pipeline.Done || ev.Kind != "reopened" || ev.Data["words"] != "the code is still tiny on the Pixel" {
		t.Fatalf("reopen: %s %+v", d.Item.Stage, ev)
	}
	n := e.item(id + 1)
	if n.Item.ReopenOf != id || n.Item.Priority != "P2" || n.Item.Ask != "the code is still tiny on the Pixel" {
		t.Fatalf("filed: %+v", n.Item)
	}
	e.drain()
	var line string
	var parent string
	for _, c := range only(t, e.calls(), "chat.postMessage") {
		if c.Body["thread_ts"] == ts {
			line = fmt.Sprint(c.Body["text"])
		} else if strings.Contains(allText(c), fmt.Sprintf("#%d ", id+1)) {
			parent = allText(c)
		}
	}
	want := fmt.Sprintf("Reopened by you: 'the code is still tiny on the Pixel'. Filed #%d (P2) with those words; this one is done.", id+1)
	if line != want {
		t.Fatalf("thread line %q, want %q", line, want)
	}
	if !strings.Contains(parent, fmt.Sprintf("Reopens #%d", id)) {
		t.Fatalf("new item's parent %q", parent)
	}
	// A second reply files nothing more.
	e.reply(Approver, ts, "also the colour")
	if _, err := e.st.GetItem(e.ctx, id+2); err == nil {
		t.Fatal("a reply on a done item filed another item")
	}
}

func TestLoopQACommands(t *testing.T) {
	e := newBotEnv(t)
	a := e.add("bigger pairing code")
	if out := e.command(other, fmt.Sprintf("qa %d", a)); !strings.Contains(out, "only the approver") {
		t.Fatalf("someone else's qa: %q", out)
	}
	if out := e.command(Approver, fmt.Sprintf("qa %d", a)); !strings.Contains(out, "waits in qa") || !e.item(a).Item.NeedsQA {
		t.Fatalf("qa: %q", out)
	}
	if out := e.command(Approver, fmt.Sprintf("reopen %d broken", a)); !strings.Contains(out, "not qa") {
		t.Fatalf("reopen before qa: %q", out)
	}
	e.toQA(a)
	if out := e.command(Approver, fmt.Sprintf("qa %d", a)); !strings.Contains(out, "mark it before it lands") {
		t.Fatalf("qa after landing: %q", out)
	}
	b := e.add("louder chime") // to qa before a's reopen files a P2 the runner would claim first
	e.toQA(b)
	if out := e.command(Approver, fmt.Sprintf("reopen %d", a)); !strings.Contains(out, "/loop reopen") {
		t.Fatalf("reopen without words: %q", out)
	}
	if out := e.command(other, fmt.Sprintf("reopen %d broken", a)); !strings.Contains(out, "only the approver") {
		t.Fatalf("someone else's reopen: %q", out)
	}
	out := e.command(Approver, fmt.Sprintf("reopen #%d the code is still tiny", a))
	if !strings.Contains(out, fmt.Sprintf("#%d bigger pairing code done; filed #%d (P2)", a, b+1)) {
		t.Fatalf("reopen: %q", out)
	}
	if out := e.command(Approver, fmt.Sprintf("verified %d", b)); !strings.Contains(out, fmt.Sprintf("#%d louder chime verified", b)) || e.item(b).Item.Stage != pipeline.Done {
		t.Fatalf("verified: %q", out)
	}
	help := e.command(other, "help")
	for _, w := range []string{"/loop qa", "/loop verified", "/loop reopen"} {
		if !strings.Contains(help, w) {
			t.Errorf("help lacks %s", w)
		}
	}
}

func TestQAMarkedLine(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("x")
	e.drain()
	e.calls()
	if _, err := e.st.SetQA(e.ctx, "slack:"+Approver, id); err != nil {
		t.Fatal(err)
	}
	e.drain()
	cs := only(t, e.calls(), "chat.postMessage")
	if len(cs) != 1 || fmt.Sprint(cs[0].Body["text"]) != "Marked for your check once merged (you): it waits in qa after it lands." {
		t.Fatalf("lines %+v", cs)
	}
}

// QA after deploy (docs/decisions/2026-10-16-qa-after-deploy.md): merged with qa_after_deploy
// says the check comes once deployed, with no buttons; the runner's deployed posts the usual ask
// with Verified and Reopen, naming the build.
func TestQAAfterDeployMessages(t *testing.T) {
	e := newBotEnv(t)
	id := e.add("loop change")
	e.runner(id, "a1", pipeline.Reproduced, map[string]string{"area": "loop", "size": "S", "risk": "normal", "resources": ""})
	e.runner(id, "builder", pipeline.PROpened, map[string]string{"repo": "jarvis-server", "pr": "142", "branch": "fix/x", "head_sha": shaA},
		"M internal/x.go")
	e.runner(id, "reviewer", pipeline.Approved, map[string]string{"summary": "tests cover it", "head_sha": shaA})
	e.runner(id, "lander", pipeline.Merged, map[string]string{"merge_sha": shaM, "head_sha": shaA, "qa_after_deploy": "true"}, "M internal/x.go")
	e.drain()
	cs := only(t, e.calls(), "chat.postMessage")
	msg := cs[len(cs)-1]
	txt := allText(msg)
	for _, want := range []string{"<@" + Approver + ">", "Merged; I'll ask for your check once it's deployed", "pull/142", "`ccccccc`"} {
		if !strings.Contains(txt, want) {
			t.Errorf("merged message lacks %q: %s", want, txt)
		}
	}
	if head := fmt.Sprintf("<@%s> #%d loop change: Merged; I'll ask for your check once it's deployed", Approver, id); fmt.Sprint(msg.Body["text"]) != head {
		t.Errorf("merged Text = %q, want %q", msg.Body["text"], head)
	}
	if strings.Contains(txt, "Press Verified") {
		t.Errorf("merged message asks for the check before the deploy: %s", txt)
	}
	if ids := actionIDs(msg); len(ids) != 0 {
		t.Fatalf("buttons before the deploy: %v", ids)
	}

	// A reply before the deploy is not taken, and says what happens next.
	e.reply(Approver, e.thread(id), "ok")
	cs = only(t, e.calls(), "chat.postMessage")
	if len(cs) != 1 || !strings.Contains(allText(cs[0]), "once it's deployed") {
		t.Fatalf("reply before the deploy: %v", cs)
	}

	rev := "abcdef0123456789abcdef0123456789abcdef01"
	if _, err := e.st.Deployed(e.ctx, "r1", id, map[string]any{"revision": rev}); err != nil {
		t.Fatal(err)
	}
	e.drain()
	cs = only(t, e.calls(), "chat.postMessage")
	msg = cs[len(cs)-1]
	txt = allText(msg)
	for _, want := range []string{"<@" + Approver + ">", "Deployed — waiting for your check", "`abcdef0`", "`ccccccc`", "Press Verified when it works"} {
		if !strings.Contains(txt, want) {
			t.Errorf("deployed message lacks %q: %s", want, txt)
		}
	}
	if head := fmt.Sprintf("<@%s> #%d loop change: Deployed — waiting for your check", Approver, id); fmt.Sprint(msg.Body["text"]) != head {
		t.Errorf("deployed Text = %q, want %q", msg.Body["text"], head)
	}
	if ids := actionIDs(msg); len(ids) != 2 || ids[0] != "qa_verified" || ids[1] != "qa_reopen" {
		t.Fatalf("buttons after the deploy: %v", ids)
	}
}
