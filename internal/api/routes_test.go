package api

import "testing"

// The route table (api.go) is not on jarvis-loop's hold list, so a PR adding a route lands on
// review and CI (docs/decisions/2026-10-18-narrow-loop-holds.md). This pins the token rules of the
// routes that approve, trust, hold or change the control: a change that opens one to a runner or
// the orchestrator fails CI instead.
func TestApprovalAndControlRoutesAreTheUsers(t *testing.T) {
	e := newEnv(t)
	for _, r := range []struct {
		method, path string
		body         map[string]any
	}{
		{"POST", "/items/1/approve", map[string]any{"head_sha": shaA}},
		{"POST", "/items/1/trust", map[string]any{"head_sha": shaA}},
		{"POST", "/items/1/hold", map[string]any{}},
		{"POST", "/plans/1/approve", map[string]any{}},
		{"PUT", "/control", map[string]any{"hold_rules": []any{}}},
		{"POST", "/unfreeze", map[string]any{"repo": "o/r"}},
	} {
		for _, tok := range []string{runnerTok, orchTok} {
			e.refused(tok, r.method, r.path, r.body, 403, CodeForbidden)
		}
	}
	// Acting for the user never reaches an approval or a trusted-file OK.
	for _, pattern := range []string{"POST /items/{id}/approve", "POST /items/{id}/trust", "POST /plans/{id}/approve"} {
		if onBehalfRoutes[pattern] {
			t.Errorf("%s is open to the orchestrator acting for the user", pattern)
		}
	}
}
