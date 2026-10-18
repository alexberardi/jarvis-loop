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

// The orchestrator's routes (docs/phases/08-orchestrator.md, "What it may do"), and how it acts for
// the user (docs/decisions/2026-10-16-orchestrator-token.md).

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
	if via == "" || (c.HoldRules == nil && c.Limits == nil && c.MaxRunMinutes == nil && c.OrchTurnsPerDay == nil && c.RunwayWarnHours == nil) {
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

func actor(r *http.Request) string { return PrincipalFrom(r.Context()).Actor() }

func (s *server) orchAnswer(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Kind   string `json:"kind"`
		Text   string `json:"text"`
		Reason string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.OrchAnswer(r.Context(), actor(r), id, body.Kind, body.Text, body.Reason)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (s *server) orchRetry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.OrchRetry(r.Context(), actor(r), id, body.Reason)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (s *server) orchPriority(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Priority string `json:"priority"`
		Reason   string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.OrchPriority(r.Context(), actor(r), id, body.Priority, body.Reason)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (s *server) orchLink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		After  int64  `json:"after"`
		Reason string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.OrchLink(r.Context(), actor(r), id, body.After, body.Reason)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (s *server) orchAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		store.NewItem
		Signature string `json:"signature"`
		Reason    string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, dup, err := s.st.OrchAdd(r.Context(), actor(r), body.NewItem, body.Signature, body.Reason)
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusCreated
	if dup {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"item": it, "duplicate": dup})
}

func (s *server) orchEscalate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ItemID         int64  `json:"item_id"`
		Text           string `json:"text"`
		Recommendation string `json:"recommendation"`
		Reason         string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.ItemID < 0 {
		badRequest(w, "item_id must be a positive number, or 0 for none")
		return
	}
	ev, err := s.st.OrchEscalate(r.Context(), actor(r), body.ItemID, body.Text, body.Recommendation, body.Reason)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ev)
}

func (s *server) orchAgents(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ActiveAgents *int   `json:"active_agents"`
		Reason       string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.ActiveAgents == nil {
		s.fail(w, &pipeline.Error{Code: pipeline.CodeMissingField, Message: "active_agents is required"})
		return
	}
	c, err := s.st.OrchAgentsDown(r.Context(), actor(r), *body.ActiveAgents, body.Reason)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *server) orchNote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text   string `json:"text"`
		Reason string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	ev, err := s.st.OrchNote(r.Context(), actor(r), body.Text, body.Reason)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ev)
}

func (s *server) orchNotebook(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			badRequest(w, "limit must be a number")
			return
		}
		limit = n
	}
	notes, err := s.st.Notebook(r.Context(), limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": notes})
}

// orchMessages lists the messages to the orchestrator (mentions and DMs) after event id since, at
// most limit (default and cap 200): what triggers its turns (piece 3).
func (s *server) orchMessages(w http.ResponseWriter, r *http.Request) {
	var since int64
	limit := 200
	if v := r.URL.Query().Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			badRequest(w, "since must be an event id")
			return
		}
		since = n
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			badRequest(w, "limit must be a positive number")
			return
		}
		limit = min(n, 200)
	}
	msgs, err := s.st.OrchMessages(r.Context(), since, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

// replyChannel is where the orchestrator may answer: the loop's channels and DMs with it.
func replyChannel(c string) bool {
	return c == slack.ChannelLoop || c == slack.ChannelDigest || c == slack.ChannelPlan || slack.IsDM(c)
}

func (s *server) orchReply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channel  string `json:"channel"`
		ThreadTS string `json:"thread_ts"`
		Text     string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Channel != "" && !replyChannel(body.Channel) {
		badRequest(w, "channel must be #loop, #loop-digest, #plan or a DM with the orchestrator")
		return
	}
	ev, err := s.st.OrchReply(r.Context(), actor(r), body.Channel, body.ThreadTS, body.Text)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ev)
}

func (s *server) orchAskApproval(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	ev, err := s.st.OrchAskApproval(r.Context(), actor(r), id, body.Reason)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ev)
}

// orchSnapshot is what an orchestrate turn sees (store.OrchSnapshot).
func (s *server) orchSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.st.OrchSnapshot(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// orchTurnResult is a runner finishing the orchestrate turn it holds.
func (s *server) orchTurnResult(w http.ResponseWriter, r *http.Request) {
	turn, err := strconv.ParseInt(r.PathValue("turn"), 10, 64)
	if err != nil || turn <= 0 {
		badRequest(w, "turn must be a positive number")
		return
	}
	var body store.OrchTurnResult
	if !decode(w, r, &body) {
		return
	}
	t, err := s.st.OrchestrateResult(r.Context(), PrincipalFrom(r.Context()).Name, turn, body)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"turn": t})
}
