package fakeclaude

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The persona play: the orchestrator's skill (skills/orchestrator/SKILL.md) as rules on loopd's
// snapshot, for the recorded situations of 2026-10-16 (scenarios/59-orchestrator-*.json). It does
// not read the skill: the scenarios test how the loop carries out the actions the skill
// prescribes (loopd's limits, refusals, dedup and events), not the model's judgment.

// The loop's own repo, where process tickets go, by its bare name: loopd resolves it to the
// configured slug (sandbox/jarvis-loop in the harness).
const loopRepo = "jarvis-loop"

// The persona's standing answers (the skill's "Answers you may give").
const (
	harnessAnswer = "Go on without it: list the run under not verified in the PR; the runner's verify runs it."
	bounceAnswer  = "Go on with the reviewer's notes: fix each must-fix point, then open the PR again."
	redCIRerun    = "Land reads CI again: re-run it once, the failure may be flaky."
	redCIBack     = "Red again on the same head: fix the code, not the expectation; the failing job's log is in ci-failure.log."
	// redCIRebuild marks the same-head red CI question that recommends a rebuild (main fixed).
	redCIRebuild = "rebuild (rebase on main)"
)

var (
	harnessQ = regexp.MustCompile(`(?i)\b(can't|cannot|can not|unable to) run (the )?(harness|emulator|model)`)
	// Red CI on a head: the build changed nothing (back to land), or land's second red (back to
	// build, with ci-failure.log).
	redCIQ  = regexp.MustCompile("^CI failed on `?[0-9a-f]{7}`?(:| and the build changed nothing)")
	sigNote = regexp.MustCompile("failure signature `([^`]+)` on #(\\d+)")
	// lastOutput is where a failed run's error carries the end of the agent's output.
	lastOutput = regexp.MustCompile(`(?s)Last output: (.*)$`)
)

type pEvent struct {
	ItemID  int64          `json:"item_id"`
	Actor   string         `json:"actor"`
	Kind    string         `json:"kind"`
	From    string         `json:"from"`
	To      string         `json:"to"`
	Outcome string         `json:"outcome"`
	Data    map[string]any `json:"data"`
}

func (e pEvent) str(k string) string {
	s, _ := e.Data[k].(string)
	return s
}

type pItem struct {
	ID         int64    `json:"id"`
	Title      string   `json:"title"`
	Stage      string   `json:"stage"`
	Question   string   `json:"question"`
	HeadSHA    string   `json:"head_sha"`
	Resources  []string `json:"resources"`
	Leased     bool     `json:"leased"`
	WaitingOn  []int64  `json:"waiting_on"`
	AgeInStage int64    `json:"age_in_stage_seconds"`
}

type pTrigger struct {
	Reason string `json:"reason"`
}

type pSnapshot struct {
	Turn *struct {
		Triggers []pTrigger `json:"triggers"`
	} `json:"turn"`
	Items   []pItem `json:"items"`
	Runners []struct {
		Name         string   `json:"name"`
		Capabilities []string `json:"capabilities"`
	} `json:"runners"`
	Triggered []struct {
		Item   pItem    `json:"item"`
		Events []pEvent `json:"events"`
	} `json:"triggered"`
	Notebook    []pEvent `json:"notebook"`
	Escalations []pEvent `json:"escalations"`
}

func act(kind string, kv ...any) map[string]any {
	a := map[string]any{"action": kind}
	for i := 0; i+1 < len(kv); i += 2 {
		a[kv[i].(string)] = kv[i+1]
	}
	return a
}

func sameWords(a, b string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(a), " "), strings.Join(strings.Fields(b), " "))
}

func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// personaActions is one turn of the persona on the snapshot (JSON).
func personaActions(turn string, raw []byte) []any {
	var snap pSnapshot
	json.Unmarshal(raw, &snap)
	actions := []any{}
	escalated := map[int64]bool{}
	for _, e := range snap.Escalations {
		escalated[e.ItemID] = true
	}
	escalate := func(id int64, text, rec, reason string) {
		if escalated[id] {
			return // an open escalation: not repeated
		}
		escalated[id] = true
		actions = append(actions, act("escalate", "item_id", id, "text", text, "recommendation", rec, "reason", reason))
	}

	// Failure signatures: this turn's run failures, and the ones the notebook remembers.
	sigItems := map[string][]int64{}
	sigLines := map[string]string{}
	seenOn := func(sig string, id int64) {
		if !slices.Contains(sigItems[sig], id) {
			sigItems[sig] = append(sigItems[sig], id)
		}
	}
	for _, n := range snap.Notebook {
		for _, m := range sigNote.FindAllStringSubmatch(n.str("text"), -1) {
			id, _ := strconv.ParseInt(m[2], 10, 64)
			seenOn(m[1], id)
		}
	}

	handled := map[int64]bool{}
	for _, tr := range snap.Triggered {
		it, evs := tr.Item, tr.Events
		if it.Stage != "needs_you" {
			continue
		}
		handled[it.ID] = true
		var last pEvent
		for i := len(evs) - 1; i >= 0; i-- {
			if evs[i].To == "needs_you" && evs[i].From != "needs_you" {
				last = evs[i]
				break
			}
		}
		var mine []pEvent // the orchestrator's answers on the item, oldest first
		for _, e := range evs {
			if e.Kind == "answered" && strings.HasPrefix(e.Actor, "orchestrator") {
				mine = append(mine, e)
			}
		}
		switch {
		case (last.Kind == "advanced" && last.Outcome == "failed") || last.Kind == "run_overtime":
			// A runner or environment error: a retry, not an answer; and its signature.
			out := last.str("error")
			if m := lastOutput.FindStringSubmatch(out); m != nil {
				out = m[1]
			}
			sig := pipeline.NormaliseSignature(out)
			actions = append(actions, act("retry", "item_id", it.ID, "reason", "a runner or environment error: "+clipLine(out, 120)))
			if sig != "" {
				seenOn(sig, it.ID)
				sigLines[sig] = clipLine(out, 300)
				actions = append(actions, act("note", "text", fmt.Sprintf("turn %s: failure signature `%s` on #%d; retried", turn, sig, it.ID),
					"reason", "to see it again on another item"))
			}

		case harnessQ.MatchString(it.Question):
			actions = append(actions, act("answer", "item_id", it.ID, "kind", pipeline.AnswerHarness, "text", harnessAnswer,
				"reason", "the agent can't run the harness in its sandbox: the standing answer until #67"))

		case last.Kind == "advanced" && last.Outcome == "bounce":
			gave := slices.ContainsFunc(mine, func(e pEvent) bool { return e.str("answer_kind") == pipeline.AnswerBounce })
			if !gave {
				actions = append(actions, act("answer", "item_id", it.ID, "kind", pipeline.AnswerBounce, "text", bounceAnswer,
					"reason", "the review bounce limit, with concrete must-fix notes"))
				break
			}
			escalate(it.ID, fmt.Sprintf("#%d %s hit the review bounce limit again, after I told it to go on with the reviewer's notes once.", it.ID, it.Title),
				"Read the reviewer's last notes and either narrow the ask (drop or split the point review keeps raising) or answer that point yourself. "+
					"Because the build already fixed one round of notes and review bounced it again, so another round alone will not converge.",
				"the second bounce limit on one item is the user's")

		case redCIQ.MatchString(it.Question) && strings.Contains(it.Question, redCIRebuild):
			// Main was red at the PR's base and is green now: the question recommends a rebuild
			// (a rerun would test the same merge again), which is the user's (Rebuild…), not a
			// red_ci answer: leave it to them.

		case redCIQ.MatchString(it.Question):
			n := 0
			for _, e := range mine {
				if e.str("answer_kind") == pipeline.AnswerRedCI && e.str("head_sha") == it.HeadSHA {
					n++
				}
			}
			switch n {
			case 0:
				actions = append(actions, act("answer", "item_id", it.ID, "kind", pipeline.AnswerRedCI, "text", redCIRerun,
					"reason", "CI red on a head the build did not change: re-run once, it may be flaky"))
			case 1:
				actions = append(actions, act("answer", "item_id", it.ID, "kind", pipeline.AnswerRedCI, "text", redCIBack,
					"reason", "CI red twice on the same head: not flaky, back to build with the log"))
			default:
				escalate(it.ID, fmt.Sprintf("#%d %s: CI is still red on the same head after a re-run and a pass back to build.", it.ID, it.Title),
					"Read ci-failure.log and decide whether the test or main is wrong; if main, file the fix first and make this item wait for it. "+
						"Because the build found nothing to change twice, so the failure is not in this item's code.",
					"a third red CI on one head is the user's")
			}

		default:
			// A question the history answers: the user's newest answer to the same question.
			var user, again pEvent
			for _, e := range evs {
				if e.Kind != "answered" || !sameWords(e.str("question"), it.Question) {
					continue
				}
				if strings.HasPrefix(e.Actor, "orchestrator") {
					again = e
				} else {
					user = e
				}
			}
			switch {
			case again.Kind != "":
				escalate(it.ID, fmt.Sprintf("#%d asked %q again after I answered it from your earlier reply (%q).", it.ID, it.Question, again.str("answer")),
					"Answer it once more in your own words, or send it back to triage with a sharper ask. "+
						"Because the agent did not take the answer from the history, so my repeating it will not help.",
					"my answer did not unstick it")
			case user.Kind != "":
				actions = append(actions, act("answer", "item_id", it.ID, "kind", pipeline.AnswerHistory, "text", user.str("answer"),
					"reason", fmt.Sprintf("you answered the same question before (%s)", user.Actor)))
			}
		}
	}

	// A retry's backoff ended (a due turn): retry the run failures the notebook is waiting on.
	waiting := map[int64]bool{}
	for _, ids := range sigItems {
		for _, id := range ids {
			waiting[id] = true
		}
	}
	due := snap.Turn != nil && slices.ContainsFunc(snap.Turn.Triggers, func(t pTrigger) bool { return t.Reason == "due" })
	for _, it := range snap.Items {
		if due && it.Stage == "needs_you" && waiting[it.ID] && !handled[it.ID] && !escalated[it.ID] {
			actions = append(actions, act("retry", "item_id", it.ID, "reason", "its backoff ended: retry the runner error"))
		}
	}

	// Process issues: the same signature on two or more items is one P1 ticket against the loop
	// (loopd adds a repeat's evidence to the open one).
	sigs := make([]string, 0, len(sigItems))
	for s := range sigItems {
		sigs = append(sigs, s)
	}
	sort.Strings(sigs)
	for _, sig := range sigs {
		ids := sigItems[sig]
		if len(ids) < 2 || sigLines[sig] == "" {
			continue
		}
		refs := make([]string, len(ids))
		for i, id := range ids {
			refs[i] = "#" + strconv.FormatInt(id, 10)
		}
		actions = append(actions,
			act("add", "repo", loopRepo, "priority", "P1", "title", "Runs fail the same way on several items: "+clipLine(sig, 80), "signature", sig,
				"ask", fmt.Sprintf("Items %s failed on the runner with the same error. Log lines:\n\n    %s\n\nLikely an environment problem on the runner, not the items' code. Meanwhile I retried them with the backoff.",
					strings.Join(refs, ", "), sigLines[sig]),
				"reason", "the same failure on two or more items is the loop's, not one item's"),
			act("note", "text", fmt.Sprintf("turn %s: P1 ticket for `%s` (%s)", turn, sig, strings.Join(refs, ", ")), "reason", "filed a process ticket"))
	}

	// Ready but unclaimed: in a work stage for an hour, waiting on nothing, and no runner offers
	// what it needs (land needs none of its resources).
	work := map[string]bool{"triage": true, "build": true, "review": true}
	for _, it := range snap.Items {
		if !work[it.Stage] || it.Leased || len(it.WaitingOn) > 0 || it.AgeInStage < 3600 || len(it.Resources) == 0 {
			continue
		}
		served := false
		var runners []string
		for _, r := range snap.Runners {
			caps := strings.Join(r.Capabilities, ", ")
			if caps == "" {
				caps = "none"
			}
			runners = append(runners, fmt.Sprintf("%s (capabilities: %s)", r.Name, caps))
			if !slices.ContainsFunc(it.Resources, func(res string) bool { return !slices.Contains(r.Capabilities, res) }) {
				served = true
			}
		}
		if served {
			continue
		}
		need := strings.Join(it.Resources, ", ")
		actions = append(actions,
			act("add", "repo", loopRepo, "priority", "P1", "title", "Ready items are not claimed: no runner offers "+need, "signature", "no runner offers "+need,
				"ask", fmt.Sprintf("#%d %s has waited in %s for %dh, waiting on nothing, and needs %s. Runners: %s. A runner's config is missing the capability, or the runner that has it is down.",
					it.ID, it.Title, it.Stage, it.AgeInStage/3600, need, strings.Join(runners, "; ")),
				"reason", "ready but unclaimed for over an hour: a loop config problem, not the item's"),
			act("note", "text", fmt.Sprintf("turn %s: #%d unclaimed for want of %s; P1 ticket filed", turn, it.ID, need), "reason", "filed a process ticket"))
	}
	return actions
}
