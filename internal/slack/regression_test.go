package slack

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

func (e *botEnv) mention(user, channel, text string) {
	eventSeq++
	e.b.HandleEnvelope(e.ctx, envelope("events_api", map[string]any{"event_id": fmt.Sprintf("Ev%d", eventSeq), "event": map[string]any{
		"type": "app_mention", "channel": channel, "user": user, "text": text, "ts": fmt.Sprintf("1.%d", eventSeq)}}))
}

// finishRegression claims the queued run as the runner and reports one pass and one failure with
// a screenshot.
func (e *botEnv) finishRegression() store.Regression {
	e.t.Helper()
	c, err := e.st.Claim(e.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "a", Kinds: []string{pipeline.WorkRegression}})
	if err != nil || c == nil || c.Reg == nil {
		e.t.Fatalf("claim: %+v %v", c, err)
	}
	if _, err := e.st.AddRegressionFile(e.ctx, "devbox", c.Reg.ID, "2-shot.png", strings.NewReader("\x89PNG\r\n\x1a\n")); err != nil {
		e.t.Fatal(err)
	}
	r, err := e.st.RegressionResult(e.ctx, "devbox", c.Reg.ID, store.RegressionReport{ServerHead: "0123456789abcdef", Tests: []store.RegressionReportTest{
		{Suite: "web", Title: "inbox › deletes", Status: "passed"},
		{Suite: "web", Title: "login › <bad> password", Status: "failed", Error: "expected \"Wrong password\"", Screenshot: "2-shot.png"},
	}})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func TestRegressionMention(t *testing.T) {
	e := newBotEnv(t)
	e.b.c.LinkBase = "http://loopd.lan:7781"
	// Someone else: refused, nothing queued.
	e.mention(other, ChannelLoop, "<@U0BOT> regression")
	if rs, _ := e.st.Regressions(e.ctx, 10); len(rs) != 0 {
		t.Fatalf("someone else queued %+v", rs)
	}
	if eph := only(t, e.calls(), "chat.postEphemeral"); len(eph) != 1 || !strings.Contains(allText(eph[0]), "only the approver") {
		t.Fatalf("refusal %+v", eph)
	}
	// The approver: a web run, acknowledged to them only; a second one is refused while it waits.
	e.mention(Approver, ChannelLoop, "<@U0BOT> regression web")
	rs, _ := e.st.Regressions(e.ctx, 10)
	if len(rs) != 1 || rs[0].Suite != "web" || rs[0].RequestedBy != "slack:"+Approver || rs[0].Via != "slack" {
		t.Fatalf("queued %+v", rs)
	}
	e.mention(Approver, ChannelLoop, "<@U0BOT> regression")
	cs := e.calls()
	if posts := only(t, cs, "chat.postMessage"); len(posts) != 0 {
		t.Fatalf("queueing posted %+v", posts)
	}
	eph := only(t, cs, "chat.postEphemeral")
	if len(eph) != 2 || !strings.Contains(allText(eph[0]), "Regression R1 (web) queued") || !strings.Contains(allText(eph[1]), "R1 (web) is already queued") {
		t.Fatalf("acks %+v", eph)
	}
	if rows, _ := e.st.ListItems(e.ctx, "", ""); len(rows) != 0 {
		t.Fatalf("the command added an item: %+v", rows)
	}

	// The report: one message, the counts, the failure (escaped) with its assertion, screenshot
	// link and File it button, and the link to the board.
	r := e.finishRegression()
	posts := only(t, e.calls(), "chat.postMessage")
	if len(posts) != 1 {
		t.Fatalf("report posts %+v", posts)
	}
	txt := allText(posts[0])
	for _, want := range []string{"*Regression R1 (web) :red_circle: failed*: 1 passed, 1 failed, 0 skipped", "jarvis-server `0123456`",
		"<http://loopd.lan:7781/board/regressions/1|full report>", "*web: login › &lt;bad&gt; password*", "```expected", "Wrong password",
		"<http://loopd.lan:7781/board/regressions/1/files/1|screenshot>", `"action_id":"regression_file"`, `"value":"R1:2"`} {
		if !strings.Contains(txt, want) {
			t.Errorf("report lacks %q:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "inbox › deletes") {
		t.Errorf("the report lists a pass:\n%s", txt)
	}
	reportTS := "1700000000.000100"

	// File it: someone else is refused; the approver's files it once.
	e.press(other, ChannelLoop, reportTS, "regression_file", "", "R1:2")
	if rows, _ := e.st.ListItems(e.ctx, "", ""); len(rows) != 0 {
		t.Fatal("someone else filed it")
	}
	e.calls()
	e.press(Approver, ChannelLoop, reportTS, "regression_file", "", fmt.Sprintf("R%d:2", r.ID))
	e.press(Approver, ChannelLoop, reportTS, "regression_file", "", fmt.Sprintf("R%d:2", r.ID))
	rows, _ := e.st.ListItems(e.ctx, "", "")
	if len(rows) != 1 || rows[0].Title != "E2E failing: login › <bad> password" {
		t.Fatalf("filed %+v", rows)
	}
	cs = e.calls()
	var replies []string
	for _, c := range only(t, cs, "chat.postMessage") {
		if c.Body["thread_ts"] == reportTS {
			replies = append(replies, allText(c))
		}
	}
	if len(replies) != 2 || !strings.Contains(replies[0], "Filed as #1") || !strings.Contains(replies[1], "Already open as #1") {
		t.Fatalf("thread replies %q", replies)
	}
	var upd []string
	for _, c := range only(t, cs, "chat.update") {
		if c.Body["ts"] == reportTS {
			upd = append(upd, allText(c))
		}
	}
	if len(upd) == 0 || !strings.Contains(upd[len(upd)-1], "Filed as #1.") || strings.Contains(upd[len(upd)-1], "regression_file") {
		t.Fatalf("the report was not updated: %q", upd)
	}
}

func TestRegressionMentionWordsThatAreAnItem(t *testing.T) {
	e := newBotEnv(t)
	e.mention(Approver, ChannelLoop, "<@U0BOT> regression in the inbox: delete is gone")
	if rs, _ := e.st.Regressions(e.ctx, 10); len(rs) != 0 {
		t.Fatalf("queued %+v", rs)
	}
	if rows, _ := e.st.ListItems(e.ctx, "", ""); len(rows) != 1 {
		t.Fatalf("not added as an item: %+v", rows)
	}
}

func TestRegressionReportCouldNotRun(t *testing.T) {
	e := newBotEnv(t)
	if _, err := e.st.QueueRegression(e.ctx, "user:alex", "mobile", "board"); err != nil {
		t.Fatal(err)
	}
	c, err := e.st.Claim(e.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "a", Capabilities: []string{"emulator"}, Kinds: []string{pipeline.WorkRegression}})
	if err != nil || c == nil {
		t.Fatal(c, err)
	}
	if _, err := e.st.RegressionResult(e.ctx, "devbox", c.Reg.ID, store.RegressionReport{Error: "scripts/e2e exited 2 with no report <x>"}); err != nil {
		t.Fatal(err)
	}
	posts := only(t, e.calls(), "chat.postMessage")
	if len(posts) != 1 || !strings.Contains(allText(posts[0]), "*Regression R1 (mobile) :warning: could not run*") ||
		!strings.Contains(allText(posts[0]), "no report &lt;x&gt;") || !strings.Contains(allText(posts[0]), "/board/regressions/1 on loopd") {
		t.Fatalf("report %+v", posts)
	}
}

// A run with many failures shows the first MaxReportFailures and a line for the rest: Slack
// refuses a message of more than 50 blocks, and the report would never be posted.
func TestRegressionReportCapsTheFailures(t *testing.T) {
	e := newBotEnv(t)
	if _, err := e.st.QueueRegression(e.ctx, "user:alex", "web", "board"); err != nil {
		t.Fatal(err)
	}
	c, err := e.st.Claim(e.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "a", Kinds: []string{pipeline.WorkRegression}})
	if err != nil || c == nil || c.Reg == nil {
		t.Fatal(c, err)
	}
	const n = 60
	tests := []store.RegressionReportTest{{Suite: "web", Title: "inbox › deletes", Status: "passed"}}
	for i := range n {
		tests = append(tests, store.RegressionReportTest{Suite: "web", Title: fmt.Sprintf("page %d › loads", i), Status: "failed", Error: "timeout"})
	}
	if _, err := e.st.RegressionResult(e.ctx, "devbox", c.Reg.ID, store.RegressionReport{Tests: tests}); err != nil {
		t.Fatal(err)
	}
	posts := only(t, e.calls(), "chat.postMessage")
	if len(posts) != 1 {
		t.Fatalf("posts %+v", posts)
	}
	blocks := blocksOf(posts[0])
	if len(blocks) != 1+MaxReportFailures+1 {
		t.Fatalf("%d blocks, want %d", len(blocks), 1+MaxReportFailures+1)
	}
	last := fmt.Sprint(blocks[len(blocks)-1])
	if want := fmt.Sprintf("…and %d more failures in the full report.", n-MaxReportFailures); !strings.Contains(last, want) {
		t.Fatalf("last block %s, want %q", last, want)
	}
	if !strings.Contains(fmt.Sprint(blocks[MaxReportFailures]), fmt.Sprintf("page %d › loads", MaxReportFailures-1)) {
		t.Fatalf("block %d: %v", MaxReportFailures, blocks[MaxReportFailures])
	}
}

// A run that decided e2e checks says so under its report (#195).
func TestRegressionReportQALine(t *testing.T) {
	if got := regressionQALine(store.Event{Data: map[string]any{"verified": []any{float64(4), float64(9)}, "reopened": []any{float64(5)}}}); got != "QA from this run's E2E checks: verified #4, #9; reopened #5." {
		t.Fatalf("line %q", got)
	}
	if got := regressionQALine(store.Event{Data: map[string]any{}}); got != "" {
		t.Fatalf("no item decided: %q", got)
	}
}

// 'regression tonight' schedules the overnight run (the approver's only), 'regression tonight
// off' cancels it; its report waits for 08:30 and then lists the lanes, the failures with their
// recordings, the flaky tests apart and the duration.
func TestRegressionTonight(t *testing.T) {
	e := newBotEnv(t)
	e.b.c.LinkBase = "http://loopd.lan:7781"
	e.mention(other, ChannelLoop, "<@U0BOT> regression tonight")
	if r, _ := e.st.ScheduledRegression(e.ctx); r != nil {
		t.Fatalf("someone else scheduled %+v", r)
	}
	e.calls()
	e.mention(Approver, ChannelLoop, "<@U0BOT> regression tonight")
	e.mention(Approver, ChannelLoop, "<@U0BOT> regression tonight")
	eph := only(t, e.calls(), "chat.postEphemeral")
	if len(eph) != 2 || !strings.Contains(allText(eph[0]), "Regression R1 (overnight) scheduled: it starts at 01:00 on Thu Oct 15 or once the queue has been quiet for 10 minutes, whichever is later") ||
		!strings.Contains(allText(eph[1]), "already scheduled for 01:00 on Thu Oct 15") {
		t.Fatalf("acks %+v", eph)
	}
	e.mention(Approver, ChannelLoop, "<@U0BOT> regression tonight off")
	if r, _ := e.st.ScheduledRegression(e.ctx); r != nil {
		t.Fatalf("still scheduled %+v", r)
	}
	if eph := only(t, e.calls(), "chat.postEphemeral"); len(eph) != 1 || !strings.Contains(allText(eph[0]), "Regression R1 (overnight) cancelled.") {
		t.Fatalf("cancel ack %+v", eph)
	}
	if rows, _ := e.st.ListItems(e.ctx, "", ""); len(rows) != 0 {
		t.Fatalf("the command added an item: %+v", rows)
	}

	// Scheduled again; the queue is quiet all evening: claimed at 01:10, done at 03:24.
	e.mention(Approver, ChannelLoop, "<@U0BOT> regression tonight")
	req := pipeline.ClaimRequest{Runner: "devbox", Agent: "a", Capabilities: []string{"emulator", "gpu", "voice"}, Kinds: []string{pipeline.WorkRegression}}
	e.clk.Advance(17 * time.Hour)
	e.st.Claim(e.ctx, "devbox", req)
	e.clk.Advance(10 * time.Minute)
	c, err := e.st.Claim(e.ctx, "devbox", req)
	if err != nil || c == nil || c.Reg == nil {
		t.Fatalf("claim at 01:10: %+v %v", c, err)
	}
	for _, f := range []string{"3-shot.png", "4-reply.wav"} {
		if _, err := e.st.AddRegressionFile(e.ctx, "devbox", c.Reg.ID, f, strings.NewReader("RIFF")); err != nil {
			t.Fatal(err)
		}
	}
	for range 134 {
		e.clk.Advance(time.Minute)
		if _, err := e.st.Heartbeat(e.ctx, "devbox", c.Lease.ID, "e2e"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.st.RegressionResult(e.ctx, "devbox", c.Reg.ID, store.RegressionReport{ServerHead: "0123456789abcdef", MobileHead: "fedcba9876543210", Tests: []store.RegressionReportTest{
		{Suite: "web", Title: "inbox › deletes", Status: "passed"},
		{Suite: "web", Title: "login › wrong password", Status: "failed", Error: "expected \"Wrong password\"", Screenshot: "3-shot.png"},
		{Suite: "mobile", Title: "Sign in", Status: "passed"},
		{Suite: "mobile", Title: "Biometric login", Status: "skipped"},
		{Suite: "pipeline", Title: "timer correction", Status: "passed"},
		{Suite: "voice", Title: "lights", Status: "failed", Error: "no reply heard", Recording: "4-reply.wav"},
		{Suite: "voice", Title: "wake + timer", Status: store.TestFlaky, Error: "first run: the wake word was missed"},
	}}); err != nil {
		t.Fatal(err)
	}
	e.b.Tick(e.ctx)
	if posts := only(t, e.calls(), "chat.postMessage"); len(posts) != 0 {
		t.Fatalf("posted at night: %+v", posts)
	}
	// 08:30: one report.
	e.clk.Advance(5*time.Hour + 6*time.Minute)
	e.b.Tick(e.ctx)
	e.b.Tick(e.ctx)
	var reports []call
	for _, p := range only(t, e.calls(), "chat.postMessage") {
		if strings.Contains(allText(p), "Overnight regression") {
			reports = append(reports, p)
		}
	}
	if len(reports) != 1 {
		t.Fatalf("morning reports %+v", reports)
	}
	txt := allText(reports[0])
	for _, want := range []string{
		"*Overnight regression R2 :red_circle: failed*: 3 passed, 2 failed, 1 flaky, 1 skipped in 2h 14m",
		"jarvis-server `0123456` · jarvis-node-mobile `fedcba9`",
		"web 1 passed, 1 failed · mobile 1 passed, 1 skipped · pipeline 1 passed · voice 0 passed, 1 failed, 1 flaky",
		"*voice: lights*", "no reply heard", "<http://loopd.lan:7781/board/regressions/2/files/2|recording>",
		"<http://loopd.lan:7781/board/regressions/2/files/1|screenshot>", `"value":"R2:2"`, `"value":"R2:6"`,
		"*Flaky* (failed, then passed on the rerun): voice: wake + timer.",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("report lacks %q:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, `"value":"R2:7"`) {
		t.Errorf("a flaky test has a File it button:\n%s", txt)
	}
}

// An overnight run still not started at 07:00 gets a note in #loop saying why, once (#317).
func TestRegressionUnstartedNote(t *testing.T) {
	e := newBotEnv(t)
	e.mention(Approver, ChannelLoop, "<@U0BOT> regression tonight")
	it, err := e.st.AddItem(e.ctx, "slack:U1", store.NewItem{Title: "late work", Repo: "jarvis-server", Priority: "P3"})
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(17*time.Hour + 6*time.Hour - time.Minute)
	notes := func() []call {
		var out []call
		for _, p := range only(t, e.calls(), "chat.postMessage") {
			if strings.Contains(allText(p), "has not started") {
				out = append(out, p)
			}
		}
		return out
	}
	e.b.Tick(e.ctx)
	if posts := notes(); len(posts) != 0 {
		t.Fatalf("a note at 06:59: %+v", posts)
	}
	e.clk.Advance(time.Minute)
	e.b.Tick(e.ctx)
	e.b.Tick(e.ctx)
	posts := notes()
	if len(posts) != 1 {
		t.Fatalf("notes at 07:00: %+v", posts)
	}
	want := fmt.Sprintf("*Overnight regression R1 has not started* (set for 01:00 on Thu Oct 15): the queue is busy: #%d waits in triage.", it.ID)
	if txt := allText(posts[0]); !strings.Contains(txt, want) {
		t.Fatalf("note %s\nwant %q", txt, want)
	}
}
