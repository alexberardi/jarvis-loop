package api

import "net/http"

// The user's approval of a held merge, their OK for verify to run trusted files, and keeping a
// held merge held. On jarvis-loop's hold list (docs/decisions/2026-10-18-narrow-loop-holds.md).

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

// trust is the approver's OK for verify to run the commit it refused for changing trusted files.
func (s *server) trust(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		HeadSHA string `json:"head_sha"`
	}
	if !decode(w, r, &body) {
		return
	}
	it, err := s.st.Trust(r.Context(), PrincipalFrom(r.Context()).Actor(), id, body.HeadSHA)
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
