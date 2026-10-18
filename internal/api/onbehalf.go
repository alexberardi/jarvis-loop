package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/slack"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// How the orchestrator acts for the user (docs/decisions/2026-10-16-orchestrator-token.md), and
// the auth layer's error codes. On jarvis-loop's hold list
// (docs/decisions/2026-10-18-narrow-loop-holds.md).

// OnBehalfHeader names the approver's Slack message ("<channel>/<ts>") an orchestrator request
// acts on: the request then runs as the approver, via the orchestrator.
const OnBehalfHeader = "X-Loop-On-Behalf"

// Error codes of the auth layer.
const (
	CodeForbidden   = "forbidden"
	CodeNotOnBehalf = "not_on_behalf"
)

// onBehalfRoutes are the user's routes the orchestrator may call for the approver. Approvals (of a
// merge, of a plan's document) and trusted-file OKs are not among them: those stay on their
// sha-bound buttons, so any route not listed here (a new one included) is refused.
var onBehalfRoutes = map[string]bool{
	"POST /items":               true,
	"PATCH /items/{id}":         true,
	"POST /items/{id}/answer":   true,
	"POST /items/{id}/hold":     true,
	"POST /items/{id}/rebuild":  true,
	"POST /items/{id}/drop":     true,
	"POST /items/{id}/qa":       true,
	"POST /items/{id}/verified": true,
	"POST /items/{id}/reopen":   true,
	"POST /plans":               true,
	"POST /plans/{id}/messages": true,
	"POST /plans/{id}/drop":     true,
	"PUT /control":              true,
}

// controlForUser refuses an on-behalf PUT /control whose decoded patch sets more than stopped and
// active_agents ("pause the loop"): the hold rules, limits and run time are the user's own (the
// spec's "Never"). The handler calls it on the patch it applies, so the check cannot read the body
// differently from it.
func (s *server) controlForUser(ctx context.Context, c store.ControlPatch) error {
	via := store.ViaFrom(ctx)
	if via == "" || (c.HoldRules == nil && c.Limits == nil && c.MaxRunMinutes == nil && c.OrchTurnsPerDay == nil) {
		return nil
	}
	return s.st.Refused(ctx, via, 0, "control", &pipeline.Error{Code: CodeNotOnBehalf,
		Message: "for the user, the orchestrator may only pause the loop (stopped, active_agents): the hold rules, limits, run time and the orchestrator's cap are the user's own"})
}

// slackActor is how the approver's own Slack actions appear in events (internal/slack's
// actorPrefix): an action for them reads the same, plus data.via.
func slackActor(user string) string { return "slack:" + user }

// actionOf names a route's action in a refused event: "approve" for POST /items/{id}/approve.
func actionOf(pattern string) string {
	method, path, _ := strings.Cut(pattern, " ")
	last := path[strings.LastIndex(path, "/")+1:]
	if strings.HasPrefix(last, "{") {
		return strings.ToLower(method)
	}
	return last
}

// refusedRoute records an orchestrator's refused call on an item's route as a refused event on the
// item (so the board and Slack show it), then returns err.
func (s *server) refusedRoute(r *http.Request, p Principal, pattern string, err error) error {
	if p.Kind != KindOrchestrator || !strings.Contains(pattern, " /items/{id}") {
		return err
	}
	id, perr := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if perr != nil || id <= 0 {
		return err
	}
	return s.st.Refused(r.Context(), p.Actor(), id, actionOf(pattern), err)
}

// actForUser turns an orchestrator's request that names the approver's message into the
// approver's: the route must be one of onBehalfRoutes, and the message one loopd's Slack layer saw
// from the approver, recent and not spent on this action and target (store.UseApproverMessage).
func (s *server) actForUser(ctx context.Context, r *http.Request, p Principal, pattern, ref string) (Principal, context.Context, error) {
	if p.Kind != KindOrchestrator {
		return p, ctx, &pipeline.Error{Code: CodeForbidden, Message: "only an orchestrator token acts for the user"}
	}
	if !onBehalfRoutes[pattern] {
		return p, ctx, s.refusedRoute(r, p, pattern, &pipeline.Error{Code: CodeNotOnBehalf,
			Message: "approvals and trusted-file OKs are the user's own, on their buttons: post the approval message instead"})
	}
	channel, ts, ok := strings.Cut(ref, "/")
	if !ok || channel == "" || ts == "" {
		return p, ctx, &pipeline.Error{Code: pipeline.CodeInvalidField, Message: OnBehalfHeader + " must be <channel>/<ts>"}
	}
	m, err := s.st.UseApproverMessage(ctx, channel, ts, slack.Approver, actionOf(pattern), r.PathValue("id"), p.Actor())
	if err != nil {
		return p, ctx, s.refusedRoute(r, p, pattern, err)
	}
	return Principal{Kind: "slack", Name: m.User}, store.WithVia(ctx, p.Actor()), nil
}

// article is "a" or "an" for a token kind.
func article(kind string) string {
	if strings.ContainsAny(kind[:1], "aeiou") {
		return "an"
	}
	return "a"
}
