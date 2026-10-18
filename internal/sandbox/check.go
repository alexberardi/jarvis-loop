package sandbox

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alexberardi/jarvis-loop/internal/runner"
	"github.com/alexberardi/jarvis-loop/internal/slack"
	"github.com/alexberardi/jarvis-loop/internal/slackfake"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// check waits until every positive expectation holds, then until the loop is quiet, and then
// checks the negative ones (no_events, no_messages), which would otherwise hold vacuously before
// anything was posted.
func (p *player) check(e Expect) error {
	within := 10 * time.Second
	if e.Within != "" {
		d, err := time.ParseDuration(e.Within)
		if err != nil {
			return fmt.Errorf("within: %w", err)
		}
		within = d
	}
	var problems []string
	err := eventually(within, func() error {
		problems = p.problems(e, false)
		if len(problems) > 0 {
			return fmt.Errorf("%d unmet", len(problems))
		}
		return nil
	})
	if err == nil {
		p.settle()
		problems = p.problems(e, true)
		if len(problems) == 0 {
			return nil
		}
	}
	return fmt.Errorf("after %s:\n  - %s", within, strings.Join(problems, "\n  - "))
}

// settle waits until neither loopd nor Slack has seen anything new for a moment (at most 3 s).
func (p *player) settle() {
	const quiet = 150 * time.Millisecond
	state := func() string {
		id, _ := p.s.lastEventID()
		return fmt.Sprintf("%d/%d/%d", id, len(p.s.Slack.Calls()), len(p.s.Slack.Messages()))
	}
	last, since := state(), time.Now()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && time.Since(since) < quiet {
		time.Sleep(10 * time.Millisecond)
		if cur := state(); cur != last {
			last, since = cur, time.Now()
		}
	}
}

func (p *player) problems(e Expect, negatives bool) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	out = append(out, p.planProblems(e)...)
	for _, alias := range sortedKeys(e.Stage) {
		r, err := p.ref(alias)
		if err != nil {
			add("%v", err)
			continue
		}
		d, err := p.s.Store.GetItem(p.ctx, r.id)
		if err != nil {
			add("%s: %v", alias, err)
			continue
		}
		if string(d.Item.Stage) != e.Stage[alias] {
			add("%s (#%d) is %s, want %s%s", alias, r.id, d.Item.Stage, e.Stage[alias], questionNote(d))
		}
	}
	for _, alias := range sortedKeys(e.Repo) {
		r, err := p.ref(alias)
		if err != nil {
			add("%v", err)
			continue
		}
		d, _ := p.s.Store.GetItem(p.ctx, r.id)
		if d.Item.Repo != e.Repo[alias] {
			add("%s's repo is %q, want %q", alias, d.Item.Repo, e.Repo[alias])
		}
	}
	for _, alias := range sortedKeys(e.Question) {
		r, err := p.ref(alias)
		if err != nil {
			add("%v", err)
			continue
		}
		d, _ := p.s.Store.GetItem(p.ctx, r.id)
		if ok, err := matches(e.Question[alias], d.Item.Question); err != nil || !ok {
			add("%s's question is %q, want /%s/ %v", alias, d.Item.Question, e.Question[alias], errOrNil(err))
		}
	}
	for _, alias := range sortedKeys(e.Leased) {
		r, err := p.ref(alias)
		if err != nil {
			add("%v", err)
			continue
		}
		d, _ := p.s.Store.GetItem(p.ctx, r.id)
		if (d.Lease != nil) != e.Leased[alias] {
			add("%s leased = %v, want %v", alias, d.Lease != nil, e.Leased[alias])
		}
	}
	for _, m := range e.Evidence {
		if msg := p.evidenceProblem(m); msg != "" {
			add("%s", msg)
		}
	}
	evs, err := p.s.Events()
	if err != nil {
		add("events: %v", err)
	}
	for _, m := range e.Events {
		n, err := p.countEvents(evs, m)
		switch {
		case err != nil:
			add("%v", err)
		case m.Count != nil && n != *m.Count:
			add("%d event(s) %s, want %d%s", n, m, *m.Count, p.eventsNote(evs, m))
		case m.Count == nil && n == 0:
			add("no event %s%s", m, p.eventsNote(evs, m))
		}
	}
	for _, m := range e.NoEvents {
		if !negatives {
			break
		}
		if n, err := p.countEvents(evs, m); err != nil {
			add("%v", err)
		} else if n > 0 {
			add("%d event(s) %s, want none", n, m)
		}
	}
	msgs := p.s.Slack.Messages()
	after := 0
	for i, m := range e.Messages {
		idx, err := p.findMessage(msgs, m, after)
		if err != nil {
			add("%v", err)
			continue
		}
		if idx < 0 {
			add("message %d %s not found after the previous one%s", i+1, m, p.messagesNote(msgs, m))
			continue
		}
		after = idx + 1
	}
	for _, m := range e.NoMessages {
		if !negatives {
			break
		}
		if idx, err := p.findMessage(msgs, m, 0); err != nil {
			add("%v", err)
		} else if idx >= 0 {
			add("message %s exists, want none: %q", m, Body(msgs[idx]))
		}
	}
	if c := e.Control; c != nil {
		ctl, err := p.s.Store.Control(p.ctx)
		if err != nil {
			add("control: %v", err)
		} else {
			if c.ActiveAgents != nil && ctl.ActiveAgents != *c.ActiveAgents {
				add("active agents %d, want %d", ctl.ActiveAgents, *c.ActiveAgents)
			}
			if c.Stopped != nil && ctl.Stopped != *c.Stopped {
				add("stopped %v, want %v", ctl.Stopped, *c.Stopped)
			}
		}
	}
	if e.PRs != nil {
		if n := len(p.s.GitHub.PRs()); n != *e.PRs {
			add("%d PR(s) on the fake GitHub, want %d", n, *e.PRs)
		}
	}
	for _, alias := range sortedKeys(e.PR) {
		r, err := p.ref(alias)
		if err != nil {
			add("%v", err)
			continue
		}
		want, br := e.PR[alias], ItemBranch(r.id, r.title)
		var pr *PR
		for _, x := range p.s.GitHub.PRs() {
			if x.Head == br {
				x := x
				pr = &x
			}
		}
		if pr == nil {
			add("no PR for %s (%s)", alias, br)
			continue
		}
		if want.Number != 0 && pr.Number != want.Number {
			add("%s's PR is #%d, want #%d", alias, pr.Number, want.Number)
		}
		if want.Repo != "" && pr.Repo != want.Repo {
			add("%s's PR is on %s, want %s", alias, pr.Repo, want.Repo)
		}
		if pr.State == "OPEN" && pr.HeadSHA != p.s.GitHub.RemoteHead(pr.Repo, br) && len(pr.MergeCalls) == 0 && want.State == "" {
			add("%s's PR head %s is not the branch's head", alias, pr.HeadSHA)
		}
		if want.Edits != nil && pr.Edits != *want.Edits {
			add("%s's PR was updated %d time(s), want %d", alias, pr.Edits, *want.Edits)
		}
		if want.State != "" && pr.State != want.State {
			add("%s's PR is %s, want %s", alias, pr.State, want.State)
		}
		if want.MergeCalls != nil && len(pr.MergeCalls) != *want.MergeCalls {
			add("%s's PR got %d merge call(s) %v, want %d", alias, len(pr.MergeCalls), pr.MergeCalls, *want.MergeCalls)
		}
		if want.Comments != nil && len(pr.Comments) != *want.Comments {
			add("%s's PR has %d comment(s), want %d", alias, len(pr.Comments), *want.Comments)
		}
		newest := ""
		if len(pr.Comments) > 0 {
			newest = pr.Comments[len(pr.Comments)-1]
		} else if want.Comment != "" {
			add("%s's PR has no comment, want one matching /%s/", alias, want.Comment)
		}
		for _, kv := range [][3]string{{"title", want.Title, pr.Title}, {"body", want.Body, pr.Body}, {"newest comment", want.Comment, newest}} {
			if ok, err := matches(kv[1], kv[2]); err != nil || !ok {
				add("%s's PR %s is %q, want /%s/ %s", alias, kv[0], clip(kv[2], 300), kv[1], errOrNil(err))
			}
		}
	}
	if rm := e.Remote; rm != nil {
		out = append(out, p.remoteProblems(*rm)...)
	}
	for _, stage := range e.SamePrefix {
		out = append(out, p.prefixProblems(stage)...)
	}
	for _, stage := range sortedKeys(e.AgentRuns) {
		runs, _ := filepath.Glob(filepath.Join(p.s.Dir, "work", "runs", "*-"+stage+"-*"))
		if len(runs) != e.AgentRuns[stage] {
			add("%d agent run(s) for %s, want %d", len(runs), stage, e.AgentRuns[stage])
		}
	}
	for _, m := range e.RunFiles {
		out = append(out, p.runFileProblems(m)...)
	}
	if len(e.Board) > 0 {
		out = append(out, p.boardProblems(e.Board)...)
	}
	if e.BoardOrchestrator != "" {
		out = append(out, p.orchPanelProblems(e.BoardOrchestrator)...)
	}
	if e.Flow != nil {
		out = append(out, p.flowProblems(*e.Flow)...)
	}
	for _, rel := range e.Absent {
		if _, err := os.Lstat(filepath.Join(p.s.Dir, rel)); err == nil {
			add("%s exists, want it absent", rel)
		}
	}
	return out
}

func errOrNil(err error) string {
	if err != nil {
		return "(" + err.Error() + ")"
	}
	return ""
}

func questionNote(d store.Detail) string {
	if d.Item.Question != "" {
		return fmt.Sprintf(" (question: %q)", clip(d.Item.Question, 200))
	}
	return ""
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func matches(pattern, s string) (bool, error) {
	if pattern == "" {
		return true, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false, fmt.Errorf("bad regexp %q: %w", pattern, err)
	}
	return re.MatchString(s), nil
}

func (m EventMatch) String() string {
	var parts []string
	for _, kv := range [][2]string{{"item", m.Item}, {"kind", m.Kind}, {"actor", m.Actor}, {"outcome", m.Outcome}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+kv[1])
		}
	}
	for _, k := range sortedKeys(m.Data) {
		parts = append(parts, "data."+k+"=~/"+m.Data[k]+"/")
	}
	return "{" + strings.Join(parts, " ") + "}"
}

// actor resolves the aliases used in scenarios to loopd's actor strings.
func actor(a string) string {
	switch a {
	case "approver":
		return "slack:" + slack.Approver
	case "other":
		return "slack:" + Other
	case "runner":
		return "runner:" + RunnerName
	case "hand":
		return "runner:" + HandRunner
	case "user":
		return "user:" + UserName
	case "orchestrator":
		return "orchestrator:" + OrchName
	}
	return a
}

func (p *player) countEvents(evs []store.Event, m EventMatch) (int, error) {
	var id int64 = -1
	if m.Item != "" {
		r, err := p.ref(m.Item)
		if err != nil {
			return 0, err
		}
		id = r.id
	}
	var plan int64 = -1
	if m.Plan != "" {
		pid, err := p.planRef(m.Plan)
		if err != nil {
			return 0, err
		}
		plan = pid
	}
	n := 0
	for _, e := range evs {
		if id >= 0 && e.ItemID != id || plan >= 0 && e.PlanID != plan || m.Kind != "" && e.Kind != m.Kind || m.Actor != "" && e.Actor != actor(m.Actor) ||
			m.Outcome != "" && e.Outcome != m.Outcome {
			continue
		}
		ok := true
		for k, pat := range m.Data {
			v, present := e.Data[k]
			if !present {
				ok = false
				break
			}
			s := fmt.Sprint(v)
			if _, isStr := v.(string); !isStr {
				b, _ := json.Marshal(v)
				s = string(b)
			}
			hit, err := matches(pat, s)
			if err != nil {
				return 0, err
			}
			ok = ok && hit
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// eventsNote lists the item's events, to show what happened instead.
func (p *player) eventsNote(evs []store.Event, m EventMatch) string {
	var id int64 = -1
	if r, err := p.ref(m.Item); err == nil {
		id = r.id
	}
	var lines []string
	for _, e := range evs {
		if id >= 0 && e.ItemID != id {
			continue
		}
		l := e.Kind
		if e.Outcome != "" {
			l += "(" + e.Outcome + ")"
		}
		lines = append(lines, l+" by "+e.Actor)
	}
	if len(lines) > 30 {
		lines = lines[len(lines)-30:]
	}
	return "; events: " + strings.Join(lines, ", ")
}

func (m MsgMatch) String() string {
	var parts []string
	for _, kv := range [][2]string{{"thread", m.Thread}, {"parent", m.Parent}, {"plan", m.Plan}, {"channel", m.Channel}, {"from", m.From},
		{"kind", m.Kind}, {"to", m.To}, {"text", m.Text}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+quote(kv[1]))
		}
	}
	if len(m.Buttons) > 0 {
		parts = append(parts, "buttons="+strings.Join(m.Buttons, "|"))
	}
	if m.Pinned != nil {
		parts = append(parts, fmt.Sprintf("pinned=%v", *m.Pinned))
	}
	if m.Reactions != nil {
		parts = append(parts, "reactions=["+strings.Join(m.Reactions, ",")+"]")
	}
	return "{" + strings.Join(parts, " ") + "}"
}

func quote(s string) string { return fmt.Sprintf("%q", s) }

func channelID(name string) string {
	switch name {
	case "loop", "#loop":
		return slack.ChannelLoop
	case "digest", "loop-digest", "#loop-digest":
		return slack.ChannelDigest
	case "plan", "#plan":
		return slack.ChannelPlan
	case "dm":
		return slackfake.DMChannel(OrchApp, slack.Approver)
	}
	return name
}

func userOf(name string) string {
	switch name {
	case "bot":
		return slackfake.BotUser
	case "orchestrator":
		return OrchUser
	case "approver", "you":
		return slack.Approver
	case "other":
		return Other
	}
	return name
}

// findMessage returns the index of the first message at or after `from` that matches, or -1.
func (p *player) findMessage(msgs []slackfake.Message, m MsgMatch, from int) (int, error) {
	var thread, parent string
	if m.Thread != "" {
		r, err := p.ref(m.Thread)
		if err != nil {
			return -1, err
		}
		if thread = p.s.Thread(r.id); thread == "" {
			return -1, nil
		}
	}
	if m.Parent != "" {
		r, err := p.ref(m.Parent)
		if err != nil {
			return -1, err
		}
		if parent = p.s.Thread(r.id); parent == "" {
			return -1, nil
		}
	}
	var planTS string
	if m.Plan != "" {
		id, err := p.planRef(m.Plan)
		if err != nil {
			return -1, err
		}
		if planTS = p.s.PlanThread(id); planTS == "" {
			return -1, nil
		}
	}
	for i := from; i < len(msgs); i++ {
		x := msgs[i]
		if thread != "" && x.ThreadTS != thread || parent != "" && (x.TS != parent || x.Channel != slack.ChannelLoop) ||
			planTS != "" && (x.Channel != slack.ChannelPlan || x.TS != planTS && x.ThreadTS != planTS) ||
			m.Reactions != nil && !slices.Equal(x.Reactions, m.Reactions) ||
			m.Channel != "" && x.Channel != channelID(m.Channel) || m.From != "" && x.User != userOf(m.From) ||
			m.Kind != "" && x.Kind != m.Kind || m.To != "" && x.To != userOf(m.To) || m.Pinned != nil && x.Pinned != *m.Pinned {
			continue
		}
		hit, err := matches(m.Text, x.Text)
		if err != nil {
			return -1, err
		}
		if !hit {
			hit, _ = matches(m.Text, Body(x))
		}
		if !hit {
			continue
		}
		btns := Buttons(x)
		ok := true
		for _, b := range m.Buttons {
			ok = ok && slices.Contains(btns, b)
		}
		if ok {
			return i, nil
		}
	}
	return -1, nil
}

// messagesNote shows the thread (or channel) the message was looked for in.
func (p *player) messagesNote(msgs []slackfake.Message, m MsgMatch) string {
	var thread string
	alias := m.Thread
	if alias == "" {
		alias = m.Parent
	}
	if r, err := p.ref(alias); err == nil {
		thread = p.s.Thread(r.id)
	}
	if id, err := p.planRef(m.Plan); m.Plan != "" && err == nil {
		thread = p.s.PlanThread(id)
	}
	var sub []slackfake.Message
	for _, x := range msgs {
		if thread != "" && x.TS != thread && x.ThreadTS != thread {
			continue
		}
		if thread == "" && m.Channel != "" && x.Channel != channelID(m.Channel) {
			continue
		}
		sub = append(sub, x)
	}
	if len(sub) > 25 {
		sub = sub[len(sub)-25:]
	}
	return "; there:\n" + Render(sub, "")
}

// mustNot checks what must never happen, whatever the scenario: a bot message or reaction outside
// the three channels, a token in anything sent to Slack, a broadcast or a mention of anyone but the
// approver, an over-long message, a token in a PR, and an event from an unexpected actor (someone other than the
// approver only ever appears on a refusal).
func (p *player) mustNot() []string {
	var out []string
	allowed := []string{slack.ChannelLoop, slack.ChannelDigest, slack.ChannelPlan}
	for _, c := range p.s.Slack.Calls() {
		raw, _ := json.Marshal(c.Body)
		for _, sec := range p.s.Secrets {
			if strings.Contains(string(raw), sec) {
				out = append(out, fmt.Sprintf("%s carries a token", c.Method))
			}
		}
		switch c.Method {
		case "chat.postMessage", "chat.update", "chat.postEphemeral", "pins.add", "reactions.add", "reactions.remove":
			ch, _ := c.Body["channel"].(string)
			orch := c.Auth == "Bearer "+p.s.Secrets[7]
			// The orchestrator's app also answers in its DMs.
			if !slices.Contains(allowed, ch) && !(orch && slack.IsDM(ch)) {
				out = append(out, fmt.Sprintf("%s to %s, outside #loop, #loop-digest and #plan", c.Method, ChannelName(ch)))
			}
			if c.Auth != "Bearer "+p.s.Secrets[3] && !orch {
				out = append(out, c.Method+" without a bot token")
			}
		case "apps.connections.open":
			if c.Auth != "Bearer "+p.s.Secrets[4] && c.Auth != "Bearer "+p.s.Secrets[8] {
				out = append(out, c.Method+" without an app token")
			}
		}
	}
	mention := regexp.MustCompile(`<@([A-Z0-9]+)>`)
	for _, m := range p.s.Slack.Messages() {
		if m.User != slackfake.BotUser && m.User != OrchUser {
			continue
		}
		body := m.Text + "\n" + Body(m)
		for _, sec := range p.s.Secrets {
			if strings.Contains(body, sec) {
				out = append(out, fmt.Sprintf("a message in %s shows a token", ChannelName(m.Channel)))
			}
		}
		for _, bad := range []string{"<!channel>", "<!here>", "<!everyone>"} {
			if strings.Contains(body, bad) {
				out = append(out, fmt.Sprintf("a message pings %s: %q", bad, clip(m.Text, 120)))
			}
		}
		for _, mm := range mention.FindAllStringSubmatch(body, -1) {
			if mm[1] != slack.Approver {
				out = append(out, fmt.Sprintf("a message mentions %s, not the approver: %q", mm[1], clip(m.Text, 120)))
			}
		}
		if n := utf8.RuneCountInString(m.Text); n > slack.MaxChars {
			out = append(out, fmt.Sprintf("a message has %d characters (max %d)", n, slack.MaxChars))
		}
		for _, b := range m.Blocks {
			if n := utf8.RuneCountInString(plainText(b["text"])); n > slack.MaxChars {
				out = append(out, fmt.Sprintf("a block has %d characters (max %d)", n, slack.MaxChars))
			}
		}
	}
	for _, pr := range p.s.GitHub.PRs() {
		for _, sec := range p.s.Secrets {
			if strings.Contains(pr.Title+pr.Body, sec) {
				out = append(out, fmt.Sprintf("PR #%d shows a token", pr.Number))
			}
		}
	}
	evs, _ := p.s.Events()
	known := []string{"loopd", actor("approver"), actor("runner"), actor("hand"), actor("user"), actor("orchestrator")}
	for _, e := range evs {
		if slices.Contains(known, e.Actor) {
			continue
		}
		if e.Kind == "refused" && strings.HasPrefix(e.Actor, "slack:") {
			continue // someone else tried, and was refused: that is the point of recording it
		}
		out = append(out, fmt.Sprintf("event %s (#%d) by unexpected actor %q", e.Kind, e.ItemID, e.Actor))
	}
	return out
}

func (p *player) remoteProblems(rm RemoteMatch) []string {
	var out []string
	gh := p.s.GitHub
	if h, want := gh.RemoteHead(RepoSlug, "main"), gh.MainHead(RepoSlug); h != want {
		out = append(out, fmt.Sprintf("main moved: %s, want %s (where it started, or the last merge or scripted push)", h, want))
	}
	if rm.Main != "" {
		r, err := p.ref(rm.Main)
		if err != nil {
			out = append(out, err.Error())
		} else {
			var merge string
			for _, x := range gh.PRs() {
				if x.Head == ItemBranch(r.id, r.title) {
					merge = x.MergeCommit
				}
			}
			if h := gh.RemoteHead(RepoSlug, "main"); merge == "" || h != merge {
				out = append(out, fmt.Sprintf("main is %s, want %s's squash commit %q", h, rm.Main, merge))
			} else if parent := gh.Subject(RepoSlug, h); !strings.HasSuffix(parent, fmt.Sprintf("(#%d)", prNumber(gh, r))) {
				out = append(out, fmt.Sprintf("main's head %q is not a squash of %s's PR", parent, rm.Main))
			}
		}
	}
	slug := RepoSlug
	if rm.Repo != "" {
		slug = rm.Repo
		if h, want := gh.RemoteHead(slug, "main"), gh.MainHead(slug); h != want {
			out = append(out, fmt.Sprintf("%s's main moved: %s, want %s", slug, h, want))
		}
	}
	want := []string{"main"}
	for _, alias := range sortedKeys(rm.Branches) {
		r, err := p.ref(alias)
		if err != nil {
			out = append(out, err.Error())
			continue
		}
		br := ItemBranch(r.id, r.title)
		want = append(want, br)
		head := gh.RemoteHead(slug, br)
		if head == "" {
			out = append(out, fmt.Sprintf("no branch %s on the remote", br))
			continue
		}
		subj := gh.Subject(slug, head)
		if ok, err := matches(rm.Branches[alias], subj); err != nil || !ok {
			out = append(out, fmt.Sprintf("%s's head is %q, want /%s/ %s", br, subj, rm.Branches[alias], errOrNil(err)))
		}
		if files, ok := rm.Files[alias]; ok {
			if got := gh.Files(slug, head); !slices.Equal(got, files) {
				out = append(out, fmt.Sprintf("%s's head changed %v, want %v", br, got, files))
			}
		}
	}
	if rm.Only {
		for _, b := range gh.Branches(slug) {
			if !slices.Contains(want, b) {
				out = append(out, fmt.Sprintf("unexpected branch %s on the remote", b))
			}
		}
	}
	return out
}

// prefixProblems checks that the runner's runs of a stage share their prompt prefix: each run's
// brief starts with the stage's static brief (byte for byte) and every run has the same argv.
func (p *player) prefixProblems(stage string) []string {
	if slices.Contains(runner.Personas, stage) {
		return p.personaPrefixProblems(stage)
	}
	runs, _ := filepath.Glob(filepath.Join(p.s.Dir, "work", "runs", "*-"+stage+"-*"))
	if len(runs) < 2 {
		return []string{fmt.Sprintf("%d %s run(s), want at least 2 to compare prefixes", len(runs), stage)}
	}
	var out []string
	static := runner.StaticBrief(stage)
	var argv0 []byte
	for i, r := range runs {
		brief, err := os.ReadFile(filepath.Join(r, "brief.md"))
		if err != nil {
			out = append(out, err.Error())
			continue
		}
		cut := strings.Index(string(brief), runner.ItemHeading)
		if cut < 0 || string(brief[:cut]) != static {
			out = append(out, fmt.Sprintf("%s: the brief's start is not the %s stage's static brief", filepath.Base(r), stage))
		}
		argv, err := os.ReadFile(filepath.Join(r, "argv.json"))
		if err != nil {
			out = append(out, err.Error())
			continue
		}
		if i == 0 {
			argv0 = argv
		} else if string(argv) != string(argv0) {
			out = append(out, fmt.Sprintf("%s: argv %s differs from %s", filepath.Base(r), argv, argv0))
		}
	}
	return out
}

// personaPrefixProblems checks a plan persona's turns share their prefix: a new session's prompt
// starts with the persona's static brief, a resumed one's is the turn alone, and the argv is the
// same but for --resume <session>.
func (p *player) personaPrefixProblems(persona string) []string {
	runs, _ := filepath.Glob(filepath.Join(p.s.Dir, "work", "runs", "P*-"+persona+"-*"))
	if len(runs) < 2 {
		return []string{fmt.Sprintf("%d %s turn(s), want at least 2 to compare prefixes", len(runs), persona)}
	}
	var out []string
	static := runner.StaticBrief(persona) + runner.TurnHeading
	var argv0 []string
	for i, r := range runs {
		brief, err := os.ReadFile(filepath.Join(r, "brief.md"))
		if err != nil {
			out = append(out, err.Error())
			continue
		}
		var argv []string
		if b, err := os.ReadFile(filepath.Join(r, "argv.json")); err != nil || json.Unmarshal(b, &argv) != nil {
			out = append(out, fmt.Sprintf("%s: no argv.json", filepath.Base(r)))
			continue
		}
		resumed := slices.Index(argv, "--resume")
		switch {
		case resumed < 0 && !strings.HasPrefix(string(brief), static):
			out = append(out, fmt.Sprintf("%s: a new session's prompt does not start with the %s brief", filepath.Base(r), persona))
		case resumed >= 0 && !strings.HasPrefix(string(brief), strings.TrimPrefix(runner.TurnHeading, "\n")):
			out = append(out, fmt.Sprintf("%s: a resumed turn's prompt is not the turn alone", filepath.Base(r)))
		}
		if resumed >= 0 {
			argv = slices.Delete(argv, resumed, min(resumed+2, len(argv)))
		}
		if i == 0 {
			argv0 = argv
		} else if !slices.Equal(argv, argv0) {
			out = append(out, fmt.Sprintf("%s: argv %q differs from %q (besides --resume)", filepath.Base(r), argv, argv0))
		}
	}
	return out
}

// runFileProblems checks one file of one of an item's agent runs, or of a plan's turns.
func (p *player) runFileProblems(m RunMatch) []string {
	who, pattern := m.Item, ""
	if m.Plan != "" {
		id, err := p.planRef(m.Plan)
		if err != nil {
			return []string{err.Error()}
		}
		who, pattern = "plan "+m.Plan, fmt.Sprintf("P%d-%s-*", id, m.Stage)
	} else {
		r, err := p.ref(m.Item)
		if err != nil {
			return []string{err.Error()}
		}
		pattern = fmt.Sprintf("%d-%s-*", r.id, m.Stage)
	}
	m.Item = who
	runs, _ := filepath.Glob(filepath.Join(p.s.Dir, "work", "runs", pattern))
	sort.Strings(runs)
	n := m.Run
	if n == 0 {
		n = len(runs)
	}
	if n < 1 || n > len(runs) {
		return []string{fmt.Sprintf("%s has %d %s run(s), want run %d", m.Item, len(runs), m.Stage, n)}
	}
	path := filepath.Join(runs[n-1], m.File)
	b, err := os.ReadFile(path)
	if m.Absent {
		if err == nil {
			return []string{fmt.Sprintf("%s's %s run %d has %s, want none", m.Item, m.Stage, n, m.File)}
		}
		return nil
	}
	if err != nil {
		return []string{fmt.Sprintf("%s's %s run %d: %v", m.Item, m.Stage, n, err)}
	}
	var out []string
	if ok, err := matches(m.Text, string(b)); err != nil || !ok {
		out = append(out, fmt.Sprintf("%s's %s run %d %s does not match /%s/ %s:\n%s", m.Item, m.Stage, n, m.File, m.Text, errOrNil(err), clip(string(b), 1500)))
	}
	if m.NoText != "" {
		if ok, err := matches(m.NoText, string(b)); err != nil || ok {
			out = append(out, fmt.Sprintf("%s's %s run %d %s matches /%s/, want not %s", m.Item, m.Stage, n, m.File, m.NoText, errOrNil(err)))
		}
	}
	return out
}

func prNumber(gh *GitHub, r itemRef) int {
	for _, x := range gh.PRs() {
		if x.Head == ItemBranch(r.id, r.title) {
			return x.Number
		}
	}
	return 0
}

// evidenceProblem checks an item's evidence files against m ("" when they match).
func (p *player) evidenceProblem(m EvidenceMatch) string {
	r, err := p.ref(m.Item)
	if err != nil {
		return err.Error()
	}
	d, err := p.s.Store.GetItem(p.ctx, r.id)
	if err != nil {
		return fmt.Sprintf("%s: %v", m.Item, err)
	}
	n := 0
	var seen []string
	for _, ev := range d.Evidence {
		seen = append(seen, fmt.Sprintf("%s (%s)", ev.Name, ev.Label))
		if ev.Name != m.Name {
			continue
		}
		if ok, err := matches(m.Label, ev.Label); err != nil || !ok {
			continue
		}
		if m.Text != "" || m.NoText != "" {
			_, f, err := p.s.Store.OpenEvidence(p.ctx, r.id, ev.N)
			if err != nil {
				return fmt.Sprintf("%s's evidence %d: %v", m.Item, ev.N, err)
			}
			b, _ := io.ReadAll(f)
			f.Close()
			if ok, err := matches(m.Text, string(b)); err != nil || !ok {
				continue
			}
			if m.NoText != "" {
				if bad, err := matches(m.NoText, string(b)); err != nil || bad {
					continue
				}
			}
		}
		n++
	}
	switch {
	case m.Count != nil && n != *m.Count:
		return fmt.Sprintf("%s has %d evidence file(s) %s /%s/ text /%s/ no_text /%s/, want %d; it has %v", m.Item, n, m.Name, m.Label, m.Text, m.NoText, *m.Count, seen)
	case m.Count == nil && n == 0:
		return fmt.Sprintf("%s has no evidence file %s /%s/ text /%s/ no_text /%s/; it has %v", m.Item, m.Name, m.Label, m.Text, m.NoText, seen)
	}
	return ""
}
