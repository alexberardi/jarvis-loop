package pipeline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// QA proofs (docs/decisions/2026-10-18-qa-proofs.md): an item in qa carries a proof, one or more
// checks that say how the loop can tell the merged change works. The builder states it in its
// result (pr_opened's proof field); for an item that reached qa without one, an orchestrate turn
// writes it (the proof sweep). The runner and loopd check it: every automatic check passed →
// verified by the loop; a check failing twice in a row → the usual reopen; what only the user can
// judge (manual) stays for the user, with what to look at.

// The kinds of check.
const (
	// ProofUsage is an event pattern in loopd's own log, seen since the merge (or the deploy):
	// the feature worked in real use. loopd evaluates it.
	ProofUsage = "usage"
	// ProofCommand is a command the runner runs, fenced, in a checkout of the repo's main.
	ProofCommand = "command"
	// ProofPi is a command for the test Pi; it waits until the loop has a pi device.
	ProofPi = "pi"
	// ProofE2E names tests of the E2E suite (spec or flow paths, as e2e/coverage.json lists them, or
	// "suite: title"); the next regression run that ran them decides it (ApplyE2EResults).
	ProofE2E = "e2e"
	// ProofManual is what only the user can judge, with what to look at.
	ProofManual = "manual"
)

// ProofKinds are the kinds a check may be.
var ProofKinds = []string{ProofUsage, ProofCommand, ProofPi, ProofE2E, ProofManual}

// A check's state, and a proof's status (Proof.Status).
const (
	CheckPending = "pending" // an automatic check not passed yet
	CheckPassed  = "passed"
	CheckFailed  = "failed"  // a command check that failed on its last run
	CheckWaiting = "waiting" // pi (nothing runs it yet) and e2e (until a regression run has its tests)
	CheckManual  = "manual"  // the user's
	// ProofNone is the status of an item without a proof: the user checks it, as before proofs.
	ProofNone = "none"
)

// Who wrote a proof.
const (
	ProofByBuild = "build"
	ProofBySweep = "sweep"
)

// ProofRecheck is how often a pending (or failed) proof is checked again.
const ProofRecheck = 30 * time.Minute

// ProofFailsToReopen is how many runs in a row a command check must fail before the item is
// reopened: one failure is checked again at the next pass, so a flaky check does not file work.
const ProofFailsToReopen = 2

// Limits on a proof.
const (
	MaxProofChecks   = 10
	maxProofRun      = 500
	maxProofLook     = 300
	maxProofTests    = 20
	maxProofValues   = 10
	maxProofMatch    = 5
	maxProofEvidence = 1500
)

// ProofCheck is one check of a proof: what the builder (or the sweep) wrote, and how it went.
type ProofCheck struct {
	Kind string `json:"kind"`
	// Usage: an event of kind Event, by one of Actor (when set), whose data has, for each key of
	// Match, one of its values.
	Event string              `json:"event,omitempty"`
	Actor []string            `json:"actor,omitempty"`
	Match map[string][]string `json:"match,omitempty"`
	// Command and pi: the shell command (exit 0 passes).
	Run string `json:"run,omitempty"`
	// E2E: the suite's test names.
	Tests []string `json:"tests,omitempty"`
	// Manual: what the user should look at.
	Look string `json:"look,omitempty"`

	State string `json:"state"`
	// Fails is how many runs in a row it failed.
	Fails int `json:"fails,omitempty"`
	// Evidence is what its last check showed (a command's output tail, the usage seen).
	Evidence  string     `json:"evidence,omitempty"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
}

// Proof is an item's proof.
type Proof struct {
	Checks []ProofCheck `json:"checks"`
	// By is who wrote it: ProofByBuild or ProofBySweep.
	By string `json:"by"`
	// CheckedAt is its last check (nil: never checked).
	CheckedAt *time.Time `json:"checked_at,omitempty"`
}

// proofInput is a check as written: the kind's fields only, no state.
type proofInput struct {
	Kind  string              `json:"kind"`
	Event string              `json:"event"`
	Actor []string            `json:"actor"`
	Match map[string][]string `json:"match"`
	Run   string              `json:"run"`
	Tests []string            `json:"tests"`
	Look  string              `json:"look"`
}

// proofKindFields are the fields each kind takes besides kind, and which are required.
var proofKindFields = map[string]struct{ allowed, required []string }{
	ProofUsage:   {[]string{"event", "actor", "match"}, []string{"event"}},
	ProofCommand: {[]string{"run"}, []string{"run"}},
	ProofPi:      {[]string{"run"}, []string{"run"}},
	ProofE2E:     {[]string{"tests"}, []string{"tests"}},
	ProofManual:  {[]string{"look"}, []string{"look"}},
}

var (
	proofEventRe = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)
	proofKeyRe   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
)

// ParseProof checks a proof as written (a JSON list of checks) strictly, and returns it with every
// check in its first state: usage and command pending, pi and e2e waiting, manual the user's.
func ParseProof(raw, by string) (*Proof, error) {
	bad := func(format string, args ...any) (*Proof, error) {
		return nil, errf(CodeInvalidField, "proof: "+format, args...)
	}
	var list []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return bad("must be a JSON list of checks: %v", err)
	}
	if len(list) == 0 || len(list) > MaxProofChecks {
		return bad("needs 1 to %d checks, not %d", MaxProofChecks, len(list))
	}
	p := &Proof{By: by, Checks: []ProofCheck{}}
	for i, rawC := range list {
		n := i + 1
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(rawC, &keys); err != nil {
			return bad("check %d is not an object", n)
		}
		var in proofInput
		dec := json.NewDecoder(bytes.NewReader(rawC))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return bad("check %d: %v", n, err)
		}
		spec, ok := proofKindFields[in.Kind]
		if !ok {
			return bad("check %d: kind must be one of %s", n, strings.Join(ProofKinds, ", "))
		}
		for k := range keys {
			if k != "kind" && !slices.Contains(spec.allowed, k) {
				return bad("check %d (%s): field %q is not one of its own (%s)", n, in.Kind, k, strings.Join(spec.allowed, ", "))
			}
		}
		for _, k := range spec.required {
			if _, ok := keys[k]; !ok {
				return bad("check %d (%s): %s is required", n, in.Kind, k)
			}
		}
		c := ProofCheck{Kind: in.Kind, State: CheckPending}
		switch in.Kind {
		case ProofUsage:
			if !proofEventRe.MatchString(in.Event) {
				return bad("check %d (usage): event must be an event kind (lowercase, like claimed or rebuild)", n)
			}
			if err := proofValues(in.Actor); err != nil {
				return bad("check %d (usage): actor: %v", n, err)
			}
			if len(in.Match) > maxProofMatch {
				return bad("check %d (usage): at most %d match keys", n, maxProofMatch)
			}
			for k, vs := range in.Match {
				if !proofKeyRe.MatchString(k) {
					return bad("check %d (usage): match key %q must be a data field name", n, k)
				}
				if len(vs) == 0 {
					return bad("check %d (usage): match %s needs at least one value", n, k)
				}
				if err := proofValues(vs); err != nil {
					return bad("check %d (usage): match %s: %v", n, k, err)
				}
			}
			c.Event, c.Actor, c.Match = in.Event, in.Actor, in.Match
		case ProofCommand, ProofPi:
			run := strings.TrimSpace(in.Run)
			if run == "" || strings.ContainsAny(run, "\r\n") || utf8.RuneCountInString(run) > maxProofRun {
				return bad("check %d (%s): run must be one line of 1 to %d characters", n, in.Kind, maxProofRun)
			}
			c.Run = run
			if in.Kind == ProofPi {
				c.State = CheckWaiting
			}
		case ProofE2E:
			if len(in.Tests) == 0 || len(in.Tests) > maxProofTests {
				return bad("check %d (e2e): needs 1 to %d test names", n, maxProofTests)
			}
			for _, t := range in.Tests {
				if strings.TrimSpace(t) == "" || strings.ContainsAny(t, "\r\n") || utf8.RuneCountInString(t) > 200 {
					return bad("check %d (e2e): a test name must be one line of 1 to 200 characters", n)
				}
			}
			c.Tests, c.State = in.Tests, CheckWaiting
		case ProofManual:
			look := strings.TrimSpace(in.Look)
			if look == "" || strings.ContainsAny(look, "\r\n") || utf8.RuneCountInString(look) > maxProofLook {
				return bad("check %d (manual): look must be one line of 1 to %d characters", n, maxProofLook)
			}
			c.Look, c.State = look, CheckManual
		}
		p.Checks = append(p.Checks, c)
	}
	return p, nil
}

func proofValues(vs []string) error {
	if len(vs) > maxProofValues {
		return fmt.Errorf("at most %d values", maxProofValues)
	}
	for _, v := range vs {
		if v == "" || strings.ContainsAny(v, "\r\n") || utf8.RuneCountInString(v) > 100 {
			return fmt.Errorf("a value must be one line of 1 to 100 characters")
		}
	}
	return nil
}

// Status is where a proof stands: failed (a check failed on its last run), pending (an automatic
// check to run), waiting (only pi or e2e checks left, which nothing runs yet), manual (only the
// user's checks left) or passed. A nil proof is ProofNone.
func (p *Proof) Status() string {
	if p == nil || len(p.Checks) == 0 {
		return ProofNone
	}
	has := map[string]bool{}
	for _, c := range p.Checks {
		has[c.State] = true
	}
	for _, s := range []string{CheckFailed, CheckPending, CheckWaiting, CheckManual} {
		if has[s] {
			return s
		}
	}
	return CheckPassed
}

// Looks are the manual checks' "look at" lines.
func (p *Proof) Looks() []string {
	var out []string
	if p != nil {
		for _, c := range p.Checks {
			if c.Kind == ProofManual {
				out = append(out, c.Look)
			}
		}
	}
	return out
}

// Open are the checks in state, described.
func (p *Proof) Open(state string) []string {
	var out []string
	if p != nil {
		for _, c := range p.Checks {
			if c.State == state {
				out = append(out, c.String())
			}
		}
	}
	return out
}

// String describes a check in a few words.
func (c ProofCheck) String() string {
	switch c.Kind {
	case ProofUsage:
		s := "usage: `" + c.Event + "`"
		if len(c.Actor) > 0 {
			s += " by " + strings.Join(c.Actor, " or ")
		}
		keys := make([]string, 0, len(c.Match))
		for k := range c.Match {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			s += " " + k + "=" + strings.Join(c.Match[k], "|")
		}
		return s
	case ProofCommand:
		return "command: `" + c.Run + "`"
	case ProofPi:
		return "pi: `" + c.Run + "` (waits for the test Pi)"
	case ProofE2E:
		if c.State == CheckWaiting {
			return "e2e: " + strings.Join(c.Tests, ", ") + " (waits for the next regression run)"
		}
		return "e2e: " + strings.Join(c.Tests, ", ")
	case ProofManual:
		return "look at " + c.Look
	}
	return c.Kind
}

// ProofDue reports whether an item's proof is due a check: in qa, not waiting for a deploy, with
// an automatic check still to pass, and never checked or not for ProofRecheck.
func ProofDue(it Item, now time.Time) bool {
	if it.Stage != QA || it.AwaitingDeploy || it.Proof == nil {
		return false
	}
	if st := it.Proof.Status(); st != CheckPending && st != CheckFailed {
		return false
	}
	return it.Proof.CheckedAt == nil || !now.Before(it.Proof.CheckedAt.Add(ProofRecheck))
}

// CheckResult is how one command check went on the runner (Check is its index in the proof).
type CheckResult struct {
	Check    int    `json:"check"`
	State    string `json:"state"`
	Evidence string `json:"evidence,omitempty"`
}

// UsageSeen is what loopd found for a usage check: how many matching events since the merge, and
// the newest one's id.
type UsageSeen struct {
	Count int
	Last  int64
}

// ActorProof is the actor of what a proof does on its own (verified, reopened).
const ActorProof = "loop (proof)"

// ApplyProofChecked applies a check of an item's proof: the runner's command results and loopd's
// usage findings (by check index). Every automatic check passed and nothing left for the user →
// qa → done, kind verified. A command check failing ProofFailsToReopen runs in a row → qa → done,
// kind reopened (the caller files the new item with the words, as the user's Reopen does).
// Otherwise the item stays in qa, kind proof_checked, with changed set when its status changed.
func ApplyProofChecked(it Item, results []CheckResult, usage map[int]UsageSeen, now time.Time) (Transition, error) {
	if it.Stage != QA || it.Proof == nil {
		return Transition{}, errf(CodeWrongStage, "item is %s and has no proof to check", it.Stage)
	}
	if it.AwaitingDeploy {
		return Transition{}, errf(CodeWrongStage, "item waits for a deploy; its proof is checked after")
	}
	if !ProofDue(it, now) {
		return Transition{}, errf(CodeWrongStage, "the proof is %s and not due a check", it.Proof.Status())
	}
	p := *it.Proof
	p.Checks = slices.Clone(p.Checks)
	before := p.Status()
	at := now
	seen := map[int]bool{}
	for _, r := range results {
		if r.Check < 0 || r.Check >= len(p.Checks) || p.Checks[r.Check].Kind != ProofCommand {
			return Transition{}, errf(CodeInvalidField, "result for check %d: not a command check of this proof", r.Check)
		}
		if seen[r.Check] {
			return Transition{}, errf(CodeInvalidField, "two results for check %d", r.Check)
		}
		seen[r.Check] = true
		c := &p.Checks[r.Check]
		c.Evidence, c.CheckedAt = clipRunes(strings.TrimSpace(r.Evidence), maxProofEvidence), &at
		switch r.State {
		case CheckPassed:
			c.State, c.Fails = CheckPassed, 0
		case CheckFailed:
			c.State = CheckFailed
			c.Fails++
		default:
			return Transition{}, errf(CodeInvalidField, "result for check %d: state must be passed or failed", r.Check)
		}
	}
	for i, u := range usage {
		if i < 0 || i >= len(p.Checks) || p.Checks[i].Kind != ProofUsage {
			continue
		}
		c := &p.Checks[i]
		c.CheckedAt = &at
		if u.Count > 0 {
			c.State = CheckPassed
			c.Evidence = fmt.Sprintf("seen %d time(s) since the merge (newest: event %d)", u.Count, u.Last)
		} else if c.State != CheckPassed {
			c.Evidence = "not seen yet since the merge"
		}
	}
	p.CheckedAt = &at
	it.Proof = &p
	after := p.Status()
	data := map[string]any{"status": after, "by": p.By, "checks": proofSummary(p)}
	for _, c := range p.Checks {
		if c.Kind == ProofCommand && c.State == CheckFailed && c.Fails >= ProofFailsToReopen {
			words := fmt.Sprintf("Proof failed %d times in a row: %s: %s", c.Fails, c.String(), firstLine(c.Evidence))
			it.Stage, it.AwaitingDeploy = Done, false
			data["words"] = words
			return Transition{Item: it, From: QA, To: Done, Kind: "reopened", Data: data}, nil
		}
	}
	if after == CheckPassed {
		words := "Proof passed: " + strings.Join(proofSummary(p), "; ")
		it.Stage, it.AwaitingDeploy = Done, false
		data["words"] = words
		return Transition{Item: it, From: QA, To: Done, Kind: "verified", Data: data}, nil
	}
	data["changed"] = after != before
	return Transition{Item: it, From: QA, To: QA, Kind: "proof_checked", Data: data}, nil
}

// E2ETest is one test of a finished regression run, as an e2e check reads it: Name is
// "suite: title", File the spec or flow (relative to its suite's dir, as report.json has it), N its
// number in the run and Screenshot the run's file n of its screenshot (0: none).
type E2ETest struct {
	N          int
	Name       string
	File       string
	Status     string
	Error      string
	Screenshot int
}

// E2ERun is a finished regression run's tests.
type E2ERun struct {
	ID    int64
	Tests []E2ETest
}

// E2EMatches says whether a check's test name names t: its "suite: title", or its file, where
// the name may carry the path from the repo's root (e2e/coverage.json's e2e/web/tests/x.spec.ts
// is the web suite's tests/x.spec.ts).
func E2EMatches(name string, t E2ETest) bool {
	if name == t.Name {
		return true
	}
	return t.File != "" && (name == t.File || strings.HasSuffix(name, "/"+t.File))
}

// ApplyE2EResults applies a finished regression run to an item's e2e checks still waiting: a
// check whose every test name the run ran (skipped tests do not count) passes when all of those
// passed, and fails when one failed. A test the run did not include (its suite did not run, or a
// filter) leaves the check waiting for the next run. decided is false when the run decides none of
// its checks (nothing to record). A failure reopens the item at once (the suite retries a test
// itself; a run is not repeated every 30 minutes as a command is), with the failing test's
// assertion as the words and its screenshot in data (the caller attaches it to the new item);
// every automatic check passed and nothing left for the user → verified; otherwise proof_checked.
// The caller checks the run started after the item's merge (or deploy).
func ApplyE2EResults(it Item, run E2ERun, now time.Time) (tr Transition, decided bool, err error) {
	if it.Stage != QA || it.Proof == nil {
		return Transition{}, false, errf(CodeWrongStage, "item is %s and has no proof to check", it.Stage)
	}
	if it.AwaitingDeploy {
		return Transition{}, false, errf(CodeWrongStage, "item waits for a deploy; its proof is checked after")
	}
	p := *it.Proof
	p.Checks = slices.Clone(p.Checks)
	before := p.Status()
	at := now
	report := fmt.Sprintf("/board/regressions/%d", run.ID)
	var failed *E2ETest
	for i := range p.Checks {
		c := &p.Checks[i]
		if c.Kind != ProofE2E || c.State != CheckWaiting {
			continue
		}
		passed := 0
		var bad *E2ETest
		all := true
		for _, name := range c.Tests {
			n := 0
			for j := range run.Tests {
				t := &run.Tests[j]
				if t.Status == "skipped" || !E2EMatches(name, *t) {
					continue
				}
				n++
				// Flaky: it failed, then passed on the overnight run's rerun; not a failure.
				if t.Status == "passed" || t.Status == "flaky" {
					passed++
				} else if bad == nil {
					bad = t
				}
			}
			all = all && n > 0
		}
		switch {
		case bad != nil:
			c.State = CheckFailed
			c.Evidence = clipRunes(fmt.Sprintf("regression R%d failed: %s (%s): %s (%s)", run.ID, bad.Name, bad.File, firstLine(bad.Error), report), maxProofEvidence)
		case all:
			c.State = CheckPassed
			c.Evidence = fmt.Sprintf("regression R%d passed: %d test(s) (%s)", run.ID, passed, report)
		default:
			continue
		}
		c.CheckedAt = &at
		decided = true
		if bad != nil && failed == nil {
			failed = bad
		}
	}
	if !decided {
		return Transition{}, false, nil
	}
	it.Proof = &p
	after := p.Status()
	data := map[string]any{"status": after, "by": p.By, "checks": proofSummary(p), "regression": run.ID}
	if failed != nil {
		words := fmt.Sprintf("E2E failed in regression R%d: %s (%s): %s. Report: %s", run.ID, failed.Name, failed.File, firstLine(failed.Error), report)
		it.Stage, it.AwaitingDeploy = Done, false
		data["words"], data["test"], data["test_n"] = words, failed.Name, failed.N
		if failed.Screenshot != 0 {
			data["screenshot"] = failed.Screenshot
		}
		return Transition{Item: it, From: QA, To: Done, Kind: "reopened", Data: data}, true, nil
	}
	if after == CheckPassed {
		words := "Proof passed: " + strings.Join(proofSummary(p), "; ") + fmt.Sprintf(". Regression R%d: %s", run.ID, report)
		it.Stage, it.AwaitingDeploy = Done, false
		data["words"] = words
		return Transition{Item: it, From: QA, To: Done, Kind: "verified", Data: data}, true, nil
	}
	data["changed"] = after != before
	return Transition{Item: it, From: QA, To: QA, Kind: "proof_checked", Data: data}, true, nil
}

// ApplySetProof is a proof written for an item that has none (the sweep's): only in qa. It may
// also replace a proof the sweep wrote with only manual checks (Replaceable): written before E2E
// results could pass a check (#195), it may now have tests that cover the change.
func ApplySetProof(it Item, p *Proof) (Transition, error) {
	if it.Stage != QA {
		return Transition{}, errf(CodeWrongStage, "item is %s, not qa", it.Stage)
	}
	data := map[string]any{"by": p.By, "status": p.Status(), "checks": proofSummary(*p)}
	if it.Proof != nil {
		if !it.Proof.Replaceable() {
			return Transition{}, errf(CodeWrongStage, "item already has a proof (by %s)", it.Proof.By)
		}
		data["replaced"] = proofSummary(*it.Proof)
	}
	it.Proof = p
	return Transition{Item: it, From: QA, To: QA, Kind: "proof_set", Data: data}, nil
}

// Replaceable says the sweep wrote the proof and it has only manual checks: the sweep may write
// it again with checks the loop can run.
func (p *Proof) Replaceable() bool {
	if p == nil || p.By != ProofBySweep {
		return false
	}
	for _, c := range p.Checks {
		if c.Kind != ProofManual {
			return false
		}
	}
	return true
}

// proofSummary is each check with its state.
func proofSummary(p Proof) []string {
	out := []string{}
	for _, c := range p.Checks {
		out = append(out, c.String()+" ("+c.State+")")
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "no output"
	}
	return clipRunes(s, 300)
}

// The QA column's groups (the board), in its order.
const (
	QAWaiting  = "waiting for a proof"
	QANeedsYou = "needs you"
	QAFailed   = "failed"
)

// QAGroups are the QA column's groups in order.
var QAGroups = []string{QAWaiting, QANeedsYou, QAFailed}

// QAGroup is where a qa item stands for the board, and the line that says why: waiting for a proof
// (its deploy, or which automatic, pi or e2e checks), needs you (what to look at; an item without
// a proof is the user's check, as before proofs), or failed (a command check that failed once and
// is checked again before it reopens).
func QAGroup(p *Proof, awaitingDeploy bool) (group, detail string) {
	st := p.Status()
	switch {
	case st == CheckFailed:
		var fs []string
		for _, c := range p.Checks {
			if c.State == CheckFailed {
				fs = append(fs, c.String()+": "+firstLine(c.Evidence))
			}
		}
		return QAFailed, "failed once, checked again before it reopens: " + strings.Join(fs, "; ")
	case awaitingDeploy && st != ProofNone:
		return QAWaiting, "waits for the deploy, then: " + strings.Join(append(p.Open(CheckPending), p.Open(CheckWaiting)...), "; ")
	case st == CheckPending || st == CheckWaiting:
		return QAWaiting, "waits for: " + strings.Join(append(p.Open(CheckPending), p.Open(CheckWaiting)...), "; ")
	case st == CheckManual:
		return QANeedsYou, "look at " + strings.Join(p.Looks(), "; ")
	case awaitingDeploy:
		return QAWaiting, "waits for the deploy, then your check (no proof)"
	}
	return QANeedsYou, "no proof: your check"
}
