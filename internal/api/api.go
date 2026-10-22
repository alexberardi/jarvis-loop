// Package api is loopd's JSON-over-HTTP API (docs/phases/02-loopd-core.md): bearer-token auth,
// the handlers, and the JSON shapes. The rules live in internal/pipeline; the state in
// internal/store.
package api

import (
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

type server struct {
	st     *store.Store
	tokens *Tokens
	log    *slog.Logger
}

// New returns the API handler.
func New(st *store.Store, tokens *Tokens, log *slog.Logger) http.Handler {
	s := &server{st: st, tokens: tokens, log: log}
	return Logged(log, s.routes())
}

// routes are the API's routes, each behind its token rule.
func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	both := []string{KindUser, KindRunner}
	reads := []string{KindUser, KindRunner, KindOrchestrator}
	runner := []string{KindRunner}
	user := []string{KindUser}
	orch := []string{KindOrchestrator}
	route := func(pattern string, kinds []string, h http.HandlerFunc) {
		mux.Handle(pattern, s.auth(pattern, kinds, h))
	}
	route("POST /items", both, s.addItem)
	route("GET /items", reads, s.listItems)
	route("GET /items/{id}", reads, s.getItem)
	route("PATCH /items/{id}", both, s.patchItem)
	route("POST /claim", runner, s.claim)
	route("POST /leases/{id}/heartbeat", runner, s.heartbeat)
	route("POST /runners/self/state", runner, s.runnerState)
	route("POST /items/{id}/advance", runner, s.advance)
	route("POST /items/{id}/evidence", runner, s.uploadEvidence)
	route("POST /items/{id}/events", runner, s.runnerEvent)
	route("GET /items/{id}/evidence/{n}", reads, s.downloadEvidence)
	route("POST /items/{id}/answer", user, s.answer)
	route("POST /items/{id}/approve", user, s.approve)
	route("POST /items/{id}/trust", user, s.trust)
	route("POST /items/{id}/hold", user, s.hold)
	route("POST /items/{id}/rebuild", both, s.rebuild)
	route("POST /items/{id}/held-merge", runner, s.heldMerge)
	route("POST /items/{id}/drop", user, s.drop)
	route("POST /items/{id}/edit", user, s.edit)
	route("POST /items/{id}/link", user, s.link)
	route("POST /items/{id}/unlink", user, s.unlink)
	route("POST /items/{id}/resources", user, s.setResources)
	route("POST /items/{id}/feature", user, s.setFeature)
	route("POST /items/{id}/builder", user, s.setBuilder)
	route("POST /items/{id}/qa", user, s.setQA)
	route("POST /items/{id}/verified", user, s.verified)
	route("POST /items/{id}/reopen", user, s.reopen)
	route("POST /plans", user, s.createPlan)
	route("GET /plans", reads, s.listPlans)
	route("GET /plans/{id}", reads, s.getPlan)
	route("GET /plans/{id}/docs/{kind}", reads, s.getPlanDoc)
	route("POST /plans/{id}/messages", user, s.planMessage)
	route("POST /plans/{id}/approve", user, s.approvePlan)
	route("POST /plans/{id}/drop", user, s.dropPlan)
	route("POST /plans/{id}/turns/{turn}/result", runner, s.planTurnResult)
	route("GET /control", reads, s.getControl)
	route("GET /night", reads, s.getNight)
	route("GET /flow", reads, s.flow)
	route("GET /freezes", reads, s.listFreezes)
	route("POST /unfreeze", user, s.unfreeze)
	route("PUT /control", user, s.putControl)
	route("GET /usage", reads, s.getUsage)
	// The orchestrator's own actions (orchestrator.go).
	route("POST /orchestrator/items", orch, s.orchAdd)
	route("POST /orchestrator/items/{id}/answer", orch, s.orchAnswer)
	route("POST /orchestrator/items/{id}/retry", orch, s.orchRetry)
	route("POST /orchestrator/items/{id}/priority", orch, s.orchPriority)
	route("POST /orchestrator/items/{id}/link", orch, s.orchLink)
	route("POST /orchestrator/items/{id}/proof", orch, s.orchProof)
	route("POST /orchestrator/escalations", orch, s.orchEscalate)
	route("POST /orchestrator/agents", orch, s.orchAgents)
	route("POST /orchestrator/notes", orch, s.orchNote)
	route("GET /orchestrator/notes", reads, s.orchNotebook)
	// Its Slack side (piece 2): the messages people sent it, its replies, and the approval message
	// it has posted instead of approving.
	route("GET /orchestrator/messages", reads, s.orchMessages)
	route("POST /orchestrator/replies", orch, s.orchReply)
	route("POST /orchestrator/items/{id}/approval", orch, s.orchAskApproval)
	route("GET /orchestrator/snapshot", reads, s.orchSnapshot)
	route("POST /orchestrate/turns/{turn}/result", runner, s.orchTurnResult)
	// Regression runs (regression.go).
	route("POST /regressions", user, s.queueRegression)
	route("GET /regressions", reads, s.listRegressions)
	route("POST /regressions/scheduled", user, s.scheduleRegression)
	route("DELETE /regressions/scheduled", user, s.cancelScheduledRegression)
	route("GET /regressions/scheduled", reads, s.scheduledRegression)
	route("GET /regressions/{id}", reads, s.getRegression)
	route("GET /regressions/{id}/files/{n}", reads, s.downloadRegressionFile)
	route("POST /regressions/{id}/files", runner, s.uploadRegressionFile)
	route("POST /regressions/{id}/result", runner, s.regressionResult)
	route("POST /regressions/{id}/tests/{n}/file", user, s.fileRegressionTest)
	return mux
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
	store.CodeNotApprover:         http.StatusForbidden,
	store.CodeUsed:                http.StatusConflict,
	store.CodeNotFrozen:           http.StatusConflict,
	store.CodeRegressionOpen:      http.StatusConflict,
	pipeline.CodeLimit:            http.StatusConflict,
	pipeline.CodeTooSoon:          http.StatusTooManyRequests,
	CodeForbidden:                 http.StatusForbidden,
	CodeNotOnBehalf:               http.StatusForbidden,
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
	switch r.URL.Query().Get("proof_due") {
	case "":
	case "1", "true":
		// The qa items whose proof is due a check (the runner's proof pass); the other filters
		// do not apply.
		rows, err := s.st.ProofsDue(r.Context())
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": rows})
		return
	case "0", "false":
	default:
		s.fail(w, &pipeline.Error{Code: pipeline.CodeInvalidField, Message: "proof_due must be 1 or 0"})
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
		// OnlyResources: only items needing one of these (a subset of the capabilities).
		OnlyResources []string `json:"only_resources"`
		// PollSeconds: how often the runner asks (the held slots wait two of them for it).
		PollSeconds int      `json:"poll_seconds"`
		Stages      []string `json:"stages"`
		// RunnerStages: the work stages the runner takes in all its lanes (the held slots count
		// only its work in them); empty: it did not say.
		RunnerStages []string `json:"runner_stages"`
		// Kinds: item, plan (plan turns), orchestrate, regression; empty is items only.
		Kinds []string `json:"kinds"`
		// RunnerKinds: the kinds the runner takes in all its lanes, from its config (loopd keeps
		// them with the runner); empty: it did not say.
		RunnerKinds []string `json:"runner_kinds"`
		// Slots: how many runs the runner works at once, for the flow panel (0: not said).
		Slots int `json:"slots"`
		// Builders: the other builders the runner can run (qwen), for a paced claim's route.
		Builders []string `json:"builders"`
	}
	if !decode(w, r, &body) {
		return
	}
	for _, b := range body.Builders {
		if b != pipeline.BuilderQwen {
			s.fail(w, &pipeline.Error{Code: pipeline.CodeInvalidField, Message: "builders may be qwen, got " + strconv.Quote(b)})
			return
		}
	}
	for _, k := range append(slices.Clone(body.Kinds), body.RunnerKinds...) {
		if !validKind(k) {
			s.fail(w, &pipeline.Error{Code: pipeline.CodeInvalidField, Message: "kinds and runner_kinds may be item, plan, orchestrate and regression, got " + strconv.Quote(k)})
			return
		}
	}
	if body.Slots < 0 || body.Slots > maxSlots {
		s.fail(w, &pipeline.Error{Code: pipeline.CodeInvalidField, Message: "slots must be 0 to " + strconv.Itoa(maxSlots)})
		return
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
	var runnerStages []pipeline.Stage
	for _, st := range body.RunnerStages {
		if !pipeline.Stage(st).IsWork() {
			s.fail(w, &pipeline.Error{Code: pipeline.CodeInvalidField, Message: "runner_stages must be work stages (triage, build, review, land), got " + strconv.Quote(st)})
			return
		}
		runnerStages = append(runnerStages, pipeline.Stage(st))
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
	only, err := pipeline.ParseResources(strings.Join(body.OnlyResources, ","))
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, o := range only {
		if !slices.Contains(caps, o) {
			s.fail(w, &pipeline.Error{Code: pipeline.CodeInvalidField, Message: "only_resources must be among the capabilities, and " + strconv.Quote(o) + " is not"})
			return
		}
	}
	c, err := s.st.Claim(r.Context(), p.Name, pipeline.ClaimRequest{Runner: p.Name, Agent: body.Agent, Capabilities: caps, OnlyResources: only, PollSeconds: body.PollSeconds, RunnerStages: runnerStages, Stages: stages, Kinds: body.Kinds, RunnerKinds: body.RunnerKinds, Builders: body.Builders, Slots: body.Slots})
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
	if c.Kind == pipeline.WorkOrchestrate {
		writeJSON(w, http.StatusOK, map[string]any{"kind": c.Kind, "stage": c.Stage, "orchestrate": c.Orch, "lease": c.Lease})
		return
	}
	if c.Kind == pipeline.WorkRegression {
		writeJSON(w, http.StatusOK, map[string]any{"kind": c.Kind, "stage": c.Stage, "regression": c.Reg, "lease": c.Lease})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": c.Kind, "item": c.Item, "stage": c.Stage, "lease": c.Lease, "brief": Brief{Stage: c.Stage, Item: c.Item},
	})
}

// runnerState records the calling runner's state: "draining" (loop-runner drain) or "" (working).
func (s *server) runnerState(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State string `json:"state"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.st.SetRunnerState(r.Context(), PrincipalFrom(r.Context()).Name, body.State); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": body.State})
}

func (s *server) heartbeat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Note string `json:"note"`
		// Activity is what the run is doing (pipeline.Activity's limits); omitted keeps the last.
		Activity *pipeline.Activity `json:"activity"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Activity != nil {
		if err := body.Activity.Validate(); err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	stop, err := s.st.HeartbeatActivity(r.Context(), PrincipalFrom(r.Context()).Name, r.PathValue("id"), body.Note, body.Activity)
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
// ci_rerun: land re-ran the PR's failed CI jobs once, after the orchestrator's first red_ci answer;
// builder_fallback: a qwen item built with claude on a runner with no qwen config;
// branch_rewritten: the runner pushed a reviewed rebase over the PR's recorded head, with a lease;
// second_opinion: Gemini's findings on an approved review and Claude's verdicts, as counts).
var RunnerEventKinds = []string{"guard_refused", "low_cache_hit", "land_waiting", "ci_rerun", "builder_fallback", "branch_rewritten", "second_opinion"}

// KindMainCI is main's CI on an item's merge commit: recorded after the merge (the lease has
// ended), only by the runner that merged it (store.MainCI).
const KindMainCI = "main_ci"

// KindPRClosed is a runner closing a dropped item's PR: recorded after the drop (there is no
// lease), once, by any runner (store.PRClosed).
const KindPRClosed = "pr_closed"

// KindDeployed is a runner reporting that the loop build it runs contains a qa item's merge
// commit (docs/decisions/2026-10-16-qa-after-deploy.md): there is no lease, any runner may.
const KindDeployed = "deployed"

// KindProofChecked is a runner's check of a qa item's due proof (docs/decisions/2026-10-18-qa-proofs.md):
// its command checks' results; loopd evaluates the usage checks and decides. No lease, any
// runner may (store.ProofChecked).
const KindProofChecked = store.KindProofChecked

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
	case body.Kind == KindProofChecked:
		ev, err = s.st.ProofChecked(r.Context(), PrincipalFrom(r.Context()).Name, id, body.Data)
	case slices.Contains(RunnerEventKinds, body.Kind):
		ev, err = s.st.RunnerEvent(r.Context(), PrincipalFrom(r.Context()).Name, id, body.Kind, body.Data)
	default:
		s.fail(w, &pipeline.Error{Code: pipeline.CodeInvalidField, Message: "kind must be one of " + strings.Join(append(RunnerEventKinds, KindMainCI, KindPRClosed, KindDeployed, KindProofChecked), ", ")})
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
	w.Header().Set("X-Evidence-SHA256", ev.SHA256)
	serveFile(w, r, ev.Name, ev.ContentType, ev.At, f)
}

// serveFile serves an uploaded file: an image or plain text inline, anything else as a download,
// in a sandbox either way.
func serveFile(w http.ResponseWriter, r *http.Request, name, contentType string, at time.Time, f io.ReadSeeker) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	if slices.Contains(inlineTypes, contentType) {
		h.Set("Content-Type", contentType)
		h.Set("Content-Disposition", `inline; filename="`+name+`"`)
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	}
	http.ServeContent(w, r, "", at, f)
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

// rebuild sends a held or needs_you item back to build with notes (store.Rebuild): the user's
// note (a reply or the Rebuild button), or a runner finding the held PR conflicting. It holds no
// lease: only a held or needs_you item moves (a runner's call only a held one), and only to build.
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

// heldMerge records a runner's merge of the base into a held item's PR (store.HeldMerge): the new
// head goes to review. Like a runner's rebuild it holds no lease: only a held item moves, and only
// to review, which checks the new head before land.
func (s *server) heldMerge(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		HeadSHA string `json:"head_sha"`
		Notes   string `json:"notes"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.HeldMerge(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.HeadSHA, body.Notes)
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

// edit fixes an item's title or ask before build starts (store.Edit).
func (s *server) edit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Title *string `json:"title"`
		Ask   *string `json:"ask"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.Edit(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.Title, body.Ask)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// link makes an item wait for more items (store.Link).
func (s *server) link(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		After []int64 `json:"after"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.Link(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.After)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// unlink stops an item waiting for one other (store.Unlink).
func (s *server) unlink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		After int64 `json:"after"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.Unlink(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.After)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// setResources replaces an item's resources after triage (store.SetResources); an empty list is none.
func (s *server) setResources(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Resources []string `json:"resources"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.SetResources(r.Context(), PrincipalFrom(r.Context()).Actor(), id, strings.Join(body.Resources, ","))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// setFeature tags an item with a feature, or clears it (store.SetFeature).
func (s *server) setFeature(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Feature string `json:"feature"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.SetFeature(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.Feature)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// setBuilder sets who builds an item before its build starts: qwen, claude or auto (store.SetBuilder).
func (s *server) setBuilder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Builder string `json:"builder"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.SetBuilder(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.Builder)
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

// maxSlots bounds a runner's reported slots.
const maxSlots = 64

// flow is the flow panel's numbers (store.Flow) for ?window= (default 24h).
func (s *server) flow(w http.ResponseWriter, r *http.Request) {
	window := r.URL.Query().Get("window")
	if window == "" {
		window = store.DefaultFlowWindow
	}
	f, err := s.st.Flow(r.Context(), window)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// listFreezes are the repos frozen while their main is red.
func (s *server) listFreezes(w http.ResponseWriter, r *http.Request) {
	fz, err := s.st.Freezes(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fz)
}

// unfreeze lifts a repo's freeze by hand (loop unfreeze <repo>): the user's word only.
func (s *server) unfreeze(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repo string `json:"repo"`
	}
	if !decode(w, r, &body) {
		return
	}
	f, err := s.st.Unfreeze(r.Context(), PrincipalFrom(r.Context()).Actor(), body.Repo)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *server) getControl(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.Control(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// getNight is the night shift's state: its mode, the classes off, the user's override and
// whether it is on now (docs/decisions/2026-10-20-night-shift.md).
func (s *server) getNight(w http.ResponseWriter, r *http.Request) {
	n, err := s.st.NightShift(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

// getUsage is the usage report: claude's windows, pace and forecast, the days, stages and items.
func (s *server) getUsage(w http.ResponseWriter, r *http.Request) {
	u, err := s.st.Usage(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *server) putControl(w http.ResponseWriter, r *http.Request) {
	var p store.ControlPatch
	if !decode(w, r, &p) {
		return
	}
	if err := s.controlForUser(r.Context(), p); err != nil {
		s.fail(w, err)
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
