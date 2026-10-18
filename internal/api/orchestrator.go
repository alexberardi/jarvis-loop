package api

import (
	"net/http"
	"strconv"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/slack"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// The orchestrator's routes (docs/phases/08-orchestrator.md, "What it may do"). How it acts for the
// user is in onbehalf.go.

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
