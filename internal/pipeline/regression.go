package pipeline

import (
	"regexp"
	"slices"
	"strings"
	"time"
)

// Regression runs (docs/decisions/2026-10-18-regression-trigger.md): the approver asks for the
// full end-to-end regression (`@jarvis-loop regression [web|mobile|all]`, or the board's button);
// loopd queues one run, a runner that asks for them (kind regression) takes it in the next free
// slot, runs jarvis-server's scripts/e2e itself (no agent) and reports it.

// WorkRegression is the claim kind of a regression run.
const WorkRegression = "regression"

// RegressionStage is the stage a running regression counts as among the live leases: it holds
// an agent slot like any run.
const RegressionStage Stage = "regression"

// The suites a run may ask for (scripts/e2e's own words; voice is left out: it takes the real
// model and minutes, and runs only by hand).
const (
	SuiteWeb    = "web"
	SuiteMobile = "mobile"
	SuiteAll    = "all"
)

// Suites are the suites a run may ask for.
var Suites = []string{SuiteWeb, SuiteMobile, SuiteAll}

// RegressionMaxRun caps one regression run (its lease is judged against it, not against the
// control's max_run_minutes, which is sized for an agent's run): the mobile suite builds an APK
// and boots an emulator.
const RegressionMaxRun = 3 * time.Hour

// SuiteResources are the resources a suite needs from the runner (and the limits it counts
// against): the mobile flows need the emulator, the overnight run the voice lane's resources too.
func SuiteResources(suite string) []string {
	if suite == SuiteOvernight {
		// The voice lane runs the real model's voice turn (the GPU) and the voice stack.
		return []string{"emulator", "gpu", "voice"}
	}
	if suite == SuiteMobile || suite == SuiteAll {
		return []string{"emulator"}
	}
	return nil
}

var regressionRe = regexp.MustCompile(`(?i)^regression(?:\s+(web|mobile|all))?[.!]?$`)

// ParseRegression reads "regression", "regression web|mobile|all" (the mention's words, after the
// bot's own mention): the suite (all when it names none) and true, or false when the text is
// anything else (an item to add, which may well mention a regression).
func ParseRegression(text string) (string, bool) {
	m := regressionRe.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return "", false
	}
	if m[1] == "" {
		return SuiteAll, true
	}
	return strings.ToLower(m[1]), true
}

// RegCandidate is the queued regression run a runner could take.
type RegCandidate struct {
	RunID     int64
	Suite     string
	Resources []string
	CreatedAt time.Time
}

// regressionFits says the queued run may go to this runner now: it asks for them, none runs,
// it has the resources the suite needs, and they are under their limits.
func regressionFits(c Control, reg *RegCandidate, live []LiveLease, req ClaimRequest) bool {
	if reg == nil || !req.wants(WorkRegression) || slices.ContainsFunc(live, func(l LiveLease) bool { return l.Stage == RegressionStage }) {
		return false
	}
	if !subset(reg.Resources, req.Capabilities) {
		return false
	}
	for _, r := range reg.Resources {
		limit, ok := c.Limits[r]
		if !ok {
			continue
		}
		n := 0
		for _, l := range live {
			if slices.Contains(l.Resources, r) {
				n++
			}
		}
		if n >= limit {
			return false
		}
	}
	return true
}

// RegressionTaker says some runner seen within RunnerSeenWindow of now can take a run that needs
// resources: it claims WorkRegression and has all of them (#416). No runner seen at all is none
// (#505: such a run is not scheduled nightly and expires unnoted).
func RegressionTaker(resources []string, runners []RunnerSeen, now time.Time) bool {
	return slices.ContainsFunc(runners, func(r RunnerSeen) bool {
		return now.Sub(r.LastSeen) <= RunnerSeenWindow && (ClaimRequest{Kinds: r.Kinds}).wants(WorkRegression) && subset(resources, r.Capabilities)
	})
}
