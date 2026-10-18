package slack

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Who may start a regression run and file its failures (regression.go). In an approve*.go file for
// the same reason as approve.go: jarvis-loop's "slack-approver" hold rule covers it.

// regressionMention handles `@jarvis-loop regression [web|mobile|all]`: the approver's queues a
// run (one at a time); the report comes to #loop when it ends.
func (b *Bot) regressionMention(ctx context.Context, m slackEvent, suite string) {
	if !IsApprover(m.User) {
		b.refuse(ctx, m.Channel, m.User, 0, "regression")
		return
	}
	r, err := b.c.Store.QueueRegression(ctx, actorPrefix+m.User, suite, "slack")
	if err != nil {
		b.ephemeral(ctx, m.Channel, m.User, "No regression run queued: "+Escape(errMessage(err)))
		return
	}
	b.ephemeral(ctx, m.Channel, m.User, fmt.Sprintf("Regression R%d (%s) queued: it takes the next free agent slot, and its report comes to #loop when it ends.", r.ID, r.Suite))
}

// onRegressionFile is the 'File it' button ("R3:2": run 3's test 2): the approver's files the
// failure as an item, or points at the open one already filed for the same test.
func (b *Bot) onRegressionFile(ctx context.Context, channel, msgTS, user, value string) {
	if !IsApprover(user) {
		b.refuse(ctx, channel, user, 0, "regression_file")
		return
	}
	id, n, ok := parseRegressionRef(value)
	if !ok {
		return
	}
	it, existing, err := b.c.Store.FileRegressionFailure(ctx, actorPrefix+user, id, n)
	if err != nil {
		b.ephemeral(ctx, channel, user, "Not filed: "+Escape(errMessage(err)))
		return
	}
	reply := fmt.Sprintf("Filed as #%d: %s", it.ID, b.field(it.Title, 200, it.ID))
	if existing {
		reply = fmt.Sprintf("Already open as #%d: %s", it.ID, b.field(it.Title, 200, it.ID))
	}
	if msgTS != "" && channel == ChannelLoop {
		b.post(ctx, "regression filed", Message{Channel: ChannelLoop, ThreadTS: msgTS, Text: reply})
		if r, err := b.c.Store.GetRegression(ctx, id); err == nil {
			m := b.regressionMessage(r)
			m.TS = msgTS
			b.update(ctx, "regression report update", m)
		}
		return
	}
	b.ephemeral(ctx, channel, user, reply)
}

func parseRegressionRef(v string) (int64, int, bool) {
	run, test, ok := strings.Cut(strings.TrimPrefix(v, "R"), ":")
	if !ok {
		return 0, 0, false
	}
	id, err1 := strconv.ParseInt(run, 10, 64)
	n, err2 := strconv.Atoi(test)
	return id, n, err1 == nil && err2 == nil && id > 0 && n > 0
}
