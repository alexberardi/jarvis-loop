// Package api is loopd's JSON-over-HTTP API (docs/phases/02-loopd-core.md): bearer-token auth,
// the handlers, and the JSON shapes. The rules live in internal/pipeline; the state in
// internal/store.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

const maxJSONBody = 1 << 20

type ctxKey struct{}

// PrincipalFrom returns the authenticated principal of a request.
func PrincipalFrom(ctx context.Context) Principal {
	p, _ := ctx.Value(ctxKey{}).(Principal)
	return p
}

type server struct {
	st     *store.Store
	tokens *Tokens
	log    *slog.Logger
}

// New returns the API handler.
func New(st *store.Store, tokens *Tokens, log *slog.Logger) http.Handler {
	s := &server{st: st, tokens: tokens, log: log}
	mux := http.NewServeMux()
	both := []string{KindUser, KindRunner}
	runner := []string{KindRunner}
	user := []string{KindUser}
	route := func(pattern string, kinds []string, h http.HandlerFunc) {
		mux.Handle(pattern, s.auth(kinds, h))
	}
	route("POST /items", both, s.addItem)
	route("GET /items", both, s.listItems)
	route("GET /items/{id}", both, s.getItem)
	route("PATCH /items/{id}", both, s.patchItem)
	route("POST /claim", runner, s.claim)
	route("POST /leases/{id}/heartbeat", runner, s.heartbeat)
	route("POST /items/{id}/advance", runner, s.advance)
	route("POST /items/{id}/evidence", runner, s.uploadEvidence)
	route("POST /items/{id}/events", runner, s.runnerEvent)
	route("GET /items/{id}/evidence/{n}", both, s.downloadEvidence)
	route("POST /items/{id}/answer", user, s.answer)
	route("POST /items/{id}/approve", user, s.approve)
	route("POST /items/{id}/hold", user, s.hold)
	route("POST /items/{id}/rebuild", both, s.rebuild)
	route("POST /items/{id}/drop", user, s.drop)
	route("POST /items/{id}/qa", user, s.setQA)
	route("POST /items/{id}/verified", user, s.verified)
	route("POST /items/{id}/reopen", user, s.reopen)
	route("POST /plans", user, s.createPlan)
	route("GET /plans", both, s.listPlans)
	route("GET /plans/{id}", both, s.getPlan)
	route("GET /plans/{id}/docs/{kind}", both, s.getPlanDoc)
	route("POST /plans/{id}/messages", user, s.planMessage)
	route("POST /plans/{id}/approve", user, s.approvePlan)
	route("POST /plans/{id}/drop", user, s.dropPlan)
	route("POST /plans/{id}/turns/{turn}/result", runner, s.planTurnResult)
	route("GET /control", both, s.getControl)
	route("PUT /control", user, s.putControl)
	return Logged(log, mux)
}

// Logged logs each request: method, path (never the query or headers, so never a token),
// status and duration.
func Logged(log *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		h.ServeHTTP(rec, r)
		log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"actor", rec.actor, "ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	actor  string
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// SetActor names the principal in the request log line, when w comes from Logged.
func SetActor(w http.ResponseWriter, p Principal) {
	if rec, ok := w.(*statusRecorder); ok {
		rec.actor = p.Actor()
	}
}

// BearerToken extracts the token of an "Authorization: Bearer" header.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func (s *server) auth(kinds []string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.tokens.Lookup(BearerToken(r))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="loopd"`)
			writeErr(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
			return
		}
		SetActor(w, p)
		if !slices.Contains(kinds, p.Kind) {
			writeErr(w, http.StatusForbidden, "forbidden", "a "+p.Kind+" token cannot do this")
			return
		}
		// Expiry is evaluated on every request, so a read never shows a dead lease as live.
		if err := s.st.Sweep(r.Context()); err != nil {
			s.fail(w, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

var statusByCode = map[string]int{
	store.CodeNotFound:            http.StatusNotFound,
	store.CodeNotYourLease:        http.StatusForbidden,
	store.CodeNoLease:             http.StatusConflict,
	store.CodeTooLarge:            http.StatusRequestEntityTooLarge,
	store.CodeNotYourMerge:        http.StatusForbidden,
	store.CodeAlreadyRecorded:     http.StatusConflict,
	pipeline.CodeWrongStage:       http.StatusConflict,
	pipeline.CodeSHAMismatch:      http.StatusConflict,
	pipeline.CodeSameAgent:        http.StatusConflict,
	pipeline.CodeApprovalRequired: http.StatusConflict,
	pipeline.CodeEvidenceRequired: http.StatusConflict,
	pipeline.CodeMissingField:     http.StatusBadRequest,
	pipeline.CodeInvalidField:     http.StatusBadRequest,
	pipeline.CodeUnknownOutcome:   http.StatusBadRequest,
}

// fail writes err as a JSON error: a rule refusal with its code, anything else as a 500 that
// is logged but not shown.
func (s *server) fail(w http.ResponseWriter, err error) {
	var pe *pipeline.Error
	if errors.As(err, &pe) {
		status, ok := statusByCode[pe.Code]
		if !ok {
			status = http.StatusBadRequest
		}
		writeErr(w, status, pe.Code, pe.Message)
		return
	}
	s.log.Error("internal error", "err", err)
	writeErr(w, http.StatusInternalServerError, "internal", "internal error")
}

func badRequest(w http.ResponseWriter, msg string) {
	writeErr(w, http.StatusBadRequest, "bad_request", msg)
}

// decode reads a JSON body strictly: unknown fields and trailing data are errors.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, _ := mime.ParseMediaType(ct); mt != "application/json" {
			badRequest(w, "Content-Type must be application/json")
			return false
		}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return true // an empty body is an empty object
		}
		badRequest(w, "bad JSON body: "+err.Error())
		return false
	}
	if dec.More() {
		badRequest(w, "bad JSON body: trailing data")
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		badRequest(w, "item id must be a positive number")
		return 0, false
	}
	return id, true
}

// ---- items ----

func (s *server) addItem(w http.ResponseWriter, r *http.Request) {
	var n store.NewItem
	if !decode(w, r, &n) {
		return
	}
	it, err := s.st.AddItem(r.Context(), PrincipalFrom(r.Context()).Actor(), n)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, it)
}

func (s *server) listItems(w http.ResponseWriter, r *http.Request) {
	stage, pri := r.URL.Query().Get("stage"), r.URL.Query().Get("priority")
	if stage != "" && !pipeline.Stage(stage).Valid() {
		s.fail(w, &pipeline.Error{Code: pipeline.CodeInvalidField, Message: "unknown stage " + strconv.Quote(stage)})
		return
	}
	if pri != "" && !pipeline.ValidPriority(pri) {
		s.fail(w, &pipeline.Error{Code: pipeline.CodeInvalidField, Message: "priority must be P1 to P5"})
		return
	}
	open := false
	switch r.URL.Query().Get("open") {
	case "":
	case "1", "true":
		open = true
	case "0", "false":
	default:
		s.fail(w, &pipeline.Error{Code: pipeline.CodeInvalidField, Message: "open must be 1 or 0"})
		return
	}
	toClose := false
	switch r.URL.Query().Get("pr_to_close") {
	case "":
	case "1", "true":
		toClose = true
	case "0", "false":
	default:
		s.fail(w, &pipeline.Error{Code: pipeline.CodeInvalidField, Message: "pr_to_close must be 1 or 0"})
		return
	}
	awaiting := false
	switch r.URL.Query().Get("awaiting_deploy") {
	case "":
	case "1", "true":
		awaiting = true
	case "0", "false":
	default:
		s.fail(w, &pipeline.Error{Code: pipeline.CodeInvalidField, Message: "awaiting_deploy must be 1 or 0"})
		return
	}
	rows, err := s.st.ListItemsFilter(r.Context(), store.ListFilter{Stage: stage, Priority: pri, Open: open, PRToClose: toClose, AwaitingDeploy: awaiting})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

func (s *server) getItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	d, err := s.st.GetItem(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *server) patchItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Priority *string `json:"priority"`
		Title    *string `json:"title"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.PatchItem(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.Priority, body.Title)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// ---- runner calls ----

// Brief is what an agent is told to do. For now it is the stage and the item; the stage skills
// arrive in rollout phase 4.
type Brief struct {
	Stage pipeline.Stage `json:"stage"`
	Item  pipeline.Item  `json:"item"`
}

func (s *server) claim(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Runner       string   `json:"runner"`
		Agent        string   `json:"agent"`
		Capabilities []string `json:"capabilities"`
		Stages       []string `json:"stages"`
		// Kinds: item, plan (plan turns); empty is items only.
		Kinds []string `json:"kinds"`
	}
	if !decode(w, r, &body) {
		return
	}
	for _, k := range body.Kinds {
		if k != pipeline.WorkItem && k != pipeline.WorkPlan {
			s.fail(w, &pipeline.Error{Code: pipeline.CodeInvalidField, Message: "kinds may be item and plan, got " + strconv.Quote(k)})
			return
		}
	}
	p := PrincipalFrom(r.Context())
	var stages []pipeline.Stage
	for _, st := range body.Stages {
		if !pipeline.Stage(st).IsWork() {
			s.fail(w, &pipeline.Error{Code: pipeline.CodeInvalidField, Message: "stages must be work stages (triage, build, review, land), got " + strconv.Quote(st)})
			return
		}
		stages = append(stages, pipeline.Stage(st))
	}
	if body.Runner != "" && body.Runner != p.Name {
		writeErr(w, http.StatusForbidden, "runner_mismatch", "this token belongs to runner "+strconv.Quote(p.Name))
		return
	}
	body.Agent = strings.TrimSpace(body.Agent)
	if body.Agent == "" {
		s.fail(w, &pipeline.Error{Code: pipeline.CodeMissingField, Message: "agent is required (review independence depends on it)"})
		return
	}
	caps, err := pipeline.ParseResources(strings.Join(body.Capabilities, ","))
	if err != nil {
		s.fail(w, err)
		return
	}
	c, err := s.st.Claim(r.Context(), p.Name, pipeline.ClaimRequest{Runner: p.Name, Agent: body.Agent, Capabilities: caps, Stages: stages, Kinds: body.Kinds})
	if err != nil {
		s.fail(w, err)
		return
	}
	if c == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if c.Kind == pipeline.WorkPlan {
		writeJSON(w, http.StatusOK, map[string]any{"kind": c.Kind, "plan": c.Plan, "turn": c.Turn, "lease": c.Lease})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": c.Kind, "item": c.Item, "stage": c.Stage, "lease": c.Lease, "brief": Brief{Stage: c.Stage, Item: c.Item},
	})
}

func (s *server) heartbeat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Note string `json:"note"`
	}
	if !decode(w, r, &body) {
		return
	}
	stop, err := s.st.Heartbeat(r.Context(), PrincipalFrom(r.Context()).Name, r.PathValue("id"), body.Note)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"stop": stop})
}

func (s *server) advance(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Outcome      string            `json:"outcome"`
		Fields       map[string]string `json:"fields"`
		ChangedPaths []string          `json:"changed_paths"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.Advance(r.Context(), PrincipalFrom(r.Context()).Name, id, pipeline.Outcome(body.Outcome), body.Fields, body.ChangedPaths)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// RunnerEventKinds are the events a runner may record on an item it holds the lease on, besides
// stage moves: what happened inside a run that the user should see in the item's history
// (land_waiting: the land script has waited on CI or GitHub's mergeability for a while;
// builder_fallback: a qwen item built with claude on a runner with no qwen config).
var RunnerEventKinds = []string{"guard_refused", "low_cache_hit", "land_waiting", "builder_fallback"}

// KindMainCI is main's CI on an item's merge commit: recorded after the merge (the lease has
// ended), only by the runner that merged it (store.MainCI).
const KindMainCI = "main_ci"

// KindPRClosed is a runner closing a dropped item's PR: recorded after the drop (there is no
// lease), once, by any runner (store.PRClosed).
const KindPRClosed = "pr_closed"

// KindDeployed is a runner reporting that the loop build it runs contains a qa item's merge
// commit (docs/decisions/2026-10-16-qa-after-deploy.md): there is no lease, any runner may.
const KindDeployed = "deployed"

func (s *server) runnerEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Kind string         `json:"kind"`
		Data map[string]any `json:"data"`
	}
	if !decode(w, r, &body) {
		return
	}
	var (
		ev  store.Event
		err error
	)
	switch {
	case body.Kind == KindMainCI:
		ev, err = s.st.MainCI(r.Context(), PrincipalFrom(r.Context()).Name, id, body.Data)
	case body.Kind == KindPRClosed:
		ev, err = s.st.PRClosed(r.Context(), PrincipalFrom(r.Context()).Name, id, body.Data)
	case body.Kind == KindDeployed:
		ev, err = s.st.Deployed(r.Context(), PrincipalFrom(r.Context()).Name, id, body.Data)
	case slices.Contains(RunnerEventKinds, body.Kind):
		ev, err = s.st.RunnerEvent(r.Context(), PrincipalFrom(r.Context()).Name, id, body.Kind, body.Data)
	default:
		s.fail(w, &pipeline.Error{Code: pipeline.CodeInvalidField, Message: "kind must be one of " + strings.Join(append(RunnerEventKinds, KindMainCI, KindPRClosed, KindDeployed), ", ")})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ev)
}

func (s *server) uploadEvidence(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, store.MaxEvidenceBytes+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		badRequest(w, "want a multipart/form-data body with a file field")
		return
	}
	// Stream the parts: label (if any) must come before file, which is how every client,
	// including `loop`, writes it.
	var label string
	for {
		part, err := mr.NextPart()
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				s.fail(w, &pipeline.Error{Code: store.CodeTooLarge, Message: "evidence is limited to 20 MB"})
				return
			}
			badRequest(w, "multipart body has no file field")
			return
		}
		switch part.FormName() {
		case "label":
			b, err := io.ReadAll(io.LimitReader(part, 1<<10))
			if err != nil {
				badRequest(w, "bad label")
				return
			}
			label = strings.TrimSpace(string(b))
		case "file":
			ev, err := s.st.AddEvidence(r.Context(), PrincipalFrom(r.Context()).Name, id, label, part.FileName(), part)
			if err != nil {
				var mbe *http.MaxBytesError
				if errors.As(err, &mbe) {
					err = &pipeline.Error{Code: store.CodeTooLarge, Message: "evidence is limited to 20 MB"}
				}
				s.fail(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, ev)
			return
		}
	}
}

// inlineTypes are served as themselves; everything else is a download, so an uploaded page can
// never run in loopd's origin.
var inlineTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp", "text/plain; charset=utf-8"}

func (s *server) downloadEvidence(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n <= 0 {
		badRequest(w, "evidence number must be a positive number")
		return
	}
	ev, f, err := s.st.OpenEvidence(r.Context(), id, n)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("X-Evidence-SHA256", ev.SHA256)
	if slices.Contains(inlineTypes, ev.ContentType) {
		h.Set("Content-Type", ev.ContentType)
		h.Set("Content-Disposition", `inline; filename="`+ev.Name+`"`)
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", `attachment; filename="`+ev.Name+`"`)
	}
	http.ServeContent(w, r, "", ev.At, f)
}

// ---- user calls ----

func (s *server) answer(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.Answer(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.Text)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (s *server) approve(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		HeadSHA string `json:"head_sha"`
		Words   string `json:"words"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.Approve(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.HeadSHA, body.Words)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (s *server) hold(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Words string `json:"words"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.Hold(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.Words)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// rebuild sends a held item back to build with notes (store.Rebuild): the user's reply that is
// not an approval, or a runner finding the held PR conflicting. It holds no lease: only a held
// item moves, and only to build.
func (s *server) rebuild(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Notes string `json:"notes"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.Rebuild(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.Notes)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// drop closes an open item at the user's word (store.Drop).
func (s *server) drop(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Words string `json:"words"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.Drop(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.Words)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// setQA marks an item needs_qa before it lands (store.SetQA).
func (s *server) setQA(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct{}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.SetQA(r.Context(), PrincipalFrom(r.Context()).Actor(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// verified is the user's check of a qa item passing (store.Verified).
func (s *server) verified(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Words string `json:"words"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.Verified(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.Words)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// reopen is the user's check of a qa item failing (store.Reopen): the item and the one it filed.
func (s *server) reopen(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Words string `json:"words"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, filed, err := s.st.Reopen(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.Words)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item": it, "filed": filed})
}

func (s *server) getControl(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.Control(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *server) putControl(w http.ResponseWriter, r *http.Request) {
	var p store.ControlPatch
	if !decode(w, r, &p) {
		return
	}
	c, err := s.st.SetControl(r.Context(), PrincipalFrom(r.Context()).Actor(), p)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// ---- plans (docs/phases/07-planning.md, "API") ----

// planPathID reads a plan id from the path: "3" or "P3".
func planPathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := strings.TrimPrefix(strings.TrimPrefix(r.PathValue("id"), "P"), "p")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		badRequest(w, "plan id must be a positive number (or P<n>)")
		return 0, false
	}
	return id, true
}

func (s *server) createPlan(w http.ResponseWriter, r *http.Request) {
	var n store.NewPlan
	if !decode(w, r, &n) {
		return
	}
	p, err := s.st.CreatePlan(r.Context(), PrincipalFrom(r.Context()).Actor(), n)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *server) listPlans(w http.ResponseWriter, r *http.Request) {
	open := false
	switch r.URL.Query().Get("open") {
	case "", "0", "false":
	case "1", "true":
		open = true
	default:
		s.fail(w, &pipeline.Error{Code: pipeline.CodeInvalidField, Message: "open must be 1 or 0"})
		return
	}
	rows, err := s.st.ListPlans(r.Context(), open)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": rows})
}

func (s *server) getPlan(w http.ResponseWriter, r *http.Request) {
	id, ok := planPathID(w, r)
	if !ok {
		return
	}
	d, err := s.st.GetPlan(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *server) getPlanDoc(w http.ResponseWriter, r *http.Request) {
	id, ok := planPathID(w, r)
	if !ok {
		return
	}
	v := 0
	if raw := r.URL.Query().Get("v"); raw != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(raw, "v"))
		if err != nil || n <= 0 {
			badRequest(w, "v must be a positive version number")
			return
		}
		v = n
	}
	d, err := s.st.GetPlanDoc(r.Context(), id, r.PathValue("kind"), v)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// planMessage is the user talking in a plan's thread: the message joins the plan's next turn.
func (s *server) planMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := planPathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	t, err := s.st.PlanMessage(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.Text)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// approvePlan approves a plan's brief or blueprint, bound to the version's sha256.
func (s *server) approvePlan(w http.ResponseWriter, r *http.Request) {
	id, ok := planPathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Kind   string `json:"kind"`
		SHA256 string `json:"sha256"`
		Words  string `json:"words"`
	}
	if !decode(w, r, &body) {
		return
	}
	a, err := s.st.ApprovePlan(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.Kind, body.SHA256, body.Words)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *server) dropPlan(w http.ResponseWriter, r *http.Request) {
	id, ok := planPathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Words string `json:"words"`
	}
	if !decode(w, r, &body) {
		return
	}
	p, err := s.st.DropPlan(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.Words)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// planTurnResult is a runner finishing the plan turn it holds the lease on.
func (s *server) planTurnResult(w http.ResponseWriter, r *http.Request) {
	id, ok := planPathID(w, r)
	if !ok {
		return
	}
	turn, err := strconv.ParseInt(r.PathValue("turn"), 10, 64)
	if err != nil || turn <= 0 {
		badRequest(w, "turn must be a positive number")
		return
	}
	var body pipeline.TurnResult
	if !decode(w, r, &body) {
		return
	}
	p, t, err := s.st.PlanTurnResult(r.Context(), PrincipalFrom(r.Context()).Name, id, turn, body)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan": p, "turn": t})
}
