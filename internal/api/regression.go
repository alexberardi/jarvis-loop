package api

import (
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// Regression runs (docs/decisions/2026-10-18-regression-trigger.md): the user queues one and files
// its failures; the runner holding one uploads its files and reports it.

func (s *server) queueRegression(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Suite string `json:"suite"`
	}
	if !decode(w, r, &body) {
		return
	}
	reg, err := s.st.QueueRegression(r.Context(), PrincipalFrom(r.Context()).Actor(), body.Suite, "api")
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, reg)
}

func (s *server) listRegressions(w http.ResponseWriter, r *http.Request) {
	rs, err := s.st.Regressions(r.Context(), 50)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"regressions": rs})
}

func (s *server) getRegression(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	reg, err := s.st.GetRegression(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reg)
}

func pathN(w http.ResponseWriter, r *http.Request) (int, bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n <= 0 {
		badRequest(w, "n must be a positive number")
		return 0, false
	}
	return n, true
}

func (s *server) downloadRegressionFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	n, ok := pathN(w, r)
	if !ok {
		return
	}
	f, file, err := s.st.OpenRegressionFile(r.Context(), id, n)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer file.Close()
	serveFile(w, r, f.Name, f.ContentType, f.At, file)
}

// uploadRegressionFile takes one multipart file field, as an item's evidence upload does.
func (s *server) uploadRegressionFile(w http.ResponseWriter, r *http.Request) {
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
	tooLarge := &pipeline.Error{Code: store.CodeTooLarge, Message: "a regression file is limited to 20 MB"}
	for {
		part, err := mr.NextPart()
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				s.fail(w, tooLarge)
				return
			}
			badRequest(w, "multipart body has no file field")
			return
		}
		if part.FormName() != "file" {
			continue
		}
		f, err := s.st.AddRegressionFile(r.Context(), PrincipalFrom(r.Context()).Name, id, part.FileName(), part)
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				err = tooLarge
			}
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, f)
		return
	}
}

func (s *server) regressionResult(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body store.RegressionReport
	if !decode(w, r, &body) {
		return
	}
	reg, err := s.st.RegressionResult(r.Context(), PrincipalFrom(r.Context()).Name, id, body)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reg)
}

// fileRegressionTest is 'File it' for the user's own client: the item for a failed test (the
// open one already filed for it, with existing true).
func (s *server) fileRegressionTest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	n, ok := pathN(w, r)
	if !ok {
		return
	}
	it, existing, err := s.st.FileRegressionFailure(r.Context(), PrincipalFrom(r.Context()).Actor(), id, n)
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusCreated
	if existing {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"item": it, "existing": existing})
}

// regressionKinds are the claim kinds the API takes.
var regressionKinds = []string{pipeline.WorkItem, pipeline.WorkPlan, pipeline.WorkOrchestrate, pipeline.WorkRegression}

func validKind(k string) bool { return slices.Contains(regressionKinds, k) }
