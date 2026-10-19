package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Gemini's critique of a blueprint (docs/decisions/2026-10-19-gemini-plan-critique.md): when an
// engineering turn says its blueprint is ready and the runner config has a gemini block, agy
// critiques the blueprint against the approved brief before the Approve button is posted. The
// engineering session is resumed to answer each point (agree and revise, or disagree and why),
// and the critique with the answers becomes the plan's critique document. Advisory: whatever
// Gemini says, or fails to say, the turn is reported ready, as the persona left it or revised.

// The kinds of point a critique makes.
var CritiqueKinds = []string{"missing", "order", "risk", "untestable", "contradiction", "too_big"}

// MaxCritiquePoints caps the points the runner keeps from one critique.
const MaxCritiquePoints = 20

// critiqueAnswerMax caps the engineering turn that answers the critique.
const critiqueAnswerMax = 10 * time.Minute

// MaxCritiqueAnswer caps one answer's why.
const MaxCritiqueAnswer = 600

// CritiqueSchema is the JSON schema agy's critique must match.
const CritiqueSchema = `{
  "type": "object",
  "properties": {
    "points": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "severity": {"type": "string", "enum": ["high", "medium", "low"]},
          "section": {"type": "string"},
          "kind": {"type": "string", "enum": ["missing", "order", "risk", "untestable", "contradiction", "too_big"]},
          "point": {"type": "string"}
        },
        "required": ["severity", "section", "kind", "point"]
      }
    }
  },
  "required": ["points"]
}
`

// CritiquePoint is one of Gemini's points on a blueprint, numbered by the runner (ID, from 1).
type CritiquePoint struct {
	ID       int    `json:"id"`
	Severity string `json:"severity"`
	// Section is the blueprint's section or piece the point is about.
	Section string `json:"section"`
	Kind    string `json:"kind"`
	Point   string `json:"point"`
}

// CritiqueAnswer is engineering's answer to one point.
type CritiqueAnswer struct {
	ID      int    `json:"id"`
	Verdict string `json:"verdict"` // agree (and revised) or disagree
	Why     string `json:"why"`
}

// Critique is what Gemini's critique of one blueprint came to.
type Critique struct {
	// Reason is why there was no critique (an Agy* reason, or AgyNoTime); "" when Gemini answered.
	Reason       string
	Model        string
	BlueprintSHA string
	Points       []CritiquePoint
	Dropped      int
	// Answers are engineering's, by point ID; Unanswered is why there are none.
	Answers    map[int]CritiqueAnswer
	Unanswered string
	// RevisedSHA is the blueprint after the answer turn changed it; "" when it did not.
	RevisedSHA string
	Seconds    int
}

// ParseCritique reads agy's output for a critique: the points, numbered, the malformed and the
// ones past MaxCritiquePoints dropped. why is "" or an Agy* reason.
func ParseCritique(stdout []byte) (points []CritiquePoint, dropped int, why string) {
	var doc struct {
		Points []struct {
			Severity string `json:"severity"`
			Section  string `json:"section"`
			Kind     string `json:"kind"`
			Point    string `json:"point"`
		} `json:"points"`
	}
	if _, why = agyResponse(stdout, &doc, func() bool { return doc.Points != nil }); why != "" {
		return nil, 0, why
	}
	points = []CritiquePoint{}
	for _, p := range doc.Points {
		sev := strings.ToLower(strings.TrimSpace(p.Severity))
		kind := strings.ToLower(strings.TrimSpace(p.Kind))
		text := strings.TrimSpace(p.Point)
		if !slices.Contains(Severities, sev) || text == "" || len(points) == MaxCritiquePoints {
			dropped++
			continue
		}
		if !slices.Contains(CritiqueKinds, kind) {
			kind = "risk"
		}
		points = append(points, CritiquePoint{ID: len(points) + 1, Severity: sev, Section: clip(oneLine(p.Section), 200), Kind: kind, Point: clip(text, 1000)})
	}
	return points, dropped, ""
}

// CritiquePrompt is agy's prompt: the approved brief and the blueprint inline, and what to look
// for.
func CritiquePrompt(brief, blueprint string) string {
	half := maxPromptDiff / 2
	return fmt.Sprintf(`You are an adversarial second reviewer of an engineering blueprint, from a different model family than the agents that wrote the product brief and the blueprint. Your job is to find what they missed before the user approves it and it is split into work items, not to agree with them.

Look for:
- missing pieces: what the brief asks for that no piece delivers (tests, migrations, docs, rollout);
- wrong order or dependencies: a piece that needs another it does not depend on, or a dependency that is not needed;
- risks the blueprint does not name or handle;
- untestable pieces: a piece whose result nobody could check;
- contradictions with the brief: what the blueprint does differently from what the brief says;
- pieces too big for one item (one agent, one pull request).

Report only what you can point at: the blueprint's section or piece ("piece 2", "Risks"), the kind, and the point in a few sentences (what is wrong and what would fix it). Severity: high for something that should change before approval, medium for something worth changing, low for the rest. If you find nothing, return an empty list.

Answer only with JSON matching the given schema: {"points": [{"severity", "section", "kind", "point"}]}.

## The product brief (approved)

%s

## The blueprint

%s
`, strings.TrimSpace(cutText(brief, half, "brief")), strings.TrimSpace(cutText(blueprint, half, "blueprint")))
}

// cutText cuts s at max bytes (on a rune boundary), saying so.
func cutText(s string, max int, what string) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n\n[the " + what + " is cut here]\n"
}

// CritiqueAnswerPrompt is the resumed engineering session's prompt: answer each point.
func CritiqueAnswerPrompt(p pipeline.Plan, points []CritiquePoint, dir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Gemini's critique\n\n- Plan: P%d: %s\n- Persona: %s\n- Working directory: `%s`. Write only in `out/` and `.loop/scratch/`.\n\n",
		p.ID, oneLine(p.Title), pipeline.PersonaEngineering, dir)
	fmt.Fprintf(&b, `Before your blueprint goes to the user for approval, a reviewer from a different model family (Gemini) critiqued it against the approved brief. Its %d point(s) are below and in `+"`.loop/context/gemini-critique.json`"+`. Answer every one:

- **agree**: the point is right; revise the blueprint for it (edit your blueprint in `+"`out/`"+`) and say what you changed.
- **disagree**: it is wrong, already covered, or not worth the change; say why in a sentence.

Do not agree to be agreeable: a wrong point taken makes the plan worse. The blueprint stays ready for approval either way.

`, len(points))
	for _, pt := range points {
		fmt.Fprintf(&b, "%d. **%s, %s** (%s): %s\n", pt.ID, pt.Severity, strings.ReplaceAll(pt.Kind, "_", " "), orDash(pt.Section), strings.ReplaceAll(pt.Point, "\n", " "))
	}
	b.WriteString(`
Write ` + "`out/result.json`" + ` as your last step: a reply for the plan's thread (the answers, short: the user reads them in Slack), an answer for every point, and the blueprint's file when you revised it (leave ` + "`blueprint`" + ` out when you did not):

` + "```json\n" + `{"reply": "…", "answers": [{"id": 1, "verdict": "agree", "why": "…"}], "blueprint": "blueprint.md"}
` + "```\n")
	return b.String()
}

// ValidateCritiqueAnswers checks the answer turn's result: a reply, one answer (agree or
// disagree, with why) for every point, and a revised blueprint when it names one.
func ValidateCritiqueAnswers(raw []byte, points []CritiquePoint, outDir string, slugs []string) (reply string, answers map[int]CritiqueAnswer, blueprint string, err error) {
	var v struct {
		Reply     string           `json:"reply"`
		Answers   []CritiqueAnswer `json:"answers"`
		Blueprint string           `json:"blueprint"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return "", nil, "", fmt.Errorf("out/result.json: %v", err)
	}
	reply = strings.TrimSpace(v.Reply)
	if reply == "" {
		return "", nil, "", fmt.Errorf("the result has no reply")
	}
	ids := map[int]bool{}
	for _, p := range points {
		ids[p.ID] = true
	}
	answers = map[int]CritiqueAnswer{}
	for _, a := range v.Answers {
		switch {
		case !ids[a.ID]:
			return "", nil, "", fmt.Errorf("an answer to point %d, which does not exist", a.ID)
		case answers[a.ID].Verdict != "":
			return "", nil, "", fmt.Errorf("two answers to point %d", a.ID)
		case a.Verdict != "agree" && a.Verdict != "disagree":
			return "", nil, "", fmt.Errorf("point %d: verdict must be agree or disagree, not %q", a.ID, a.Verdict)
		case !textOK(a.Why, MaxCritiqueAnswer):
			return "", nil, "", fmt.Errorf("point %d: why is required (at most %d characters)", a.ID, MaxCritiqueAnswer)
		}
		a.Why = strings.TrimSpace(a.Why)
		answers[a.ID] = a
	}
	if len(answers) != len(ids) {
		return "", nil, "", fmt.Errorf("%d answers for %d points: answer every one", len(answers), len(ids))
	}
	if v.Blueprint != "" {
		if blueprint, err = loadBlueprint(outDir, v.Blueprint, slugs); err != nil {
			return "", nil, "", err
		}
	}
	return reply, answers, blueprint, nil
}

// taken counts the points engineering agreed with.
func (c Critique) taken() int {
	n := 0
	for _, a := range c.Answers {
		if a.Verdict == "agree" {
			n++
		}
	}
	return n
}

// Summary is the one line the reply starts with.
func (c Critique) Summary() string {
	switch {
	case c.Reason != "":
		return fmt.Sprintf("Gemini second opinion unavailable (%s): posted without it.", c.Reason)
	case len(c.Points) == 0:
		return "Gemini raised no points on the blueprint."
	case c.Unanswered != "":
		return fmt.Sprintf("Gemini raised %s; they were not answered (%s): posted the blueprint as it was.", points(len(c.Points)), c.Unanswered)
	}
	taken := c.taken()
	return fmt.Sprintf("Gemini raised %s: %d taken, %d answered.", points(len(c.Points)), taken, len(c.Points)-taken)
}

func points(n int) string {
	if n == 1 {
		return "1 point"
	}
	return fmt.Sprintf("%d points", n)
}

// CritiqueDoc is the plan's critique document: the summary, every point with engineering's
// answer, and the blueprint it is about.
func CritiqueDoc(c Critique) string {
	var b strings.Builder
	b.WriteString("# Gemini's critique of the blueprint\n\n")
	fmt.Fprintf(&b, "%s\n\n", c.Summary())
	fmt.Fprintf(&b, "Model: `%s`. The blueprint it read: sha256 `%s`", c.Model, c.BlueprintSHA)
	if c.RevisedSHA != "" {
		fmt.Fprintf(&b, "; engineering revised it after the answers (sha256 `%s`)", c.RevisedSHA)
	}
	b.WriteString(".")
	if c.Dropped > 0 {
		fmt.Fprintf(&b, " %d malformed or extra point(s) dropped.", c.Dropped)
	}
	b.WriteString(" Advisory: nothing here blocks the approval.\n")
	for _, p := range c.Points {
		fmt.Fprintf(&b, "\n## %d. %s, %s: %s\n\n%s\n\n", p.ID, p.Severity, strings.ReplaceAll(p.Kind, "_", " "), orDash(p.Section), p.Point)
		if a, ok := c.Answers[p.ID]; ok {
			verdict := "Taken"
			if a.Verdict == "disagree" {
				verdict = "Answered (disagreed)"
			}
			fmt.Fprintf(&b, "**%s.** %s\n", verdict, a.Why)
		} else {
			b.WriteString("**Not answered.**\n")
		}
	}
	return b.String()
}

// critiqued: the latest critique (in the turn's context) is of this blueprint already, as
// Gemini read it or as engineering revised it after the answers.
func critiqued(dir, sha string) bool {
	b, err := os.ReadFile(filepath.Join(dir, ".loop", "context", pipeline.DocCritique+".md"))
	return err == nil && bytes.Contains(b, []byte("sha256 `"+sha+"`"))
}

// withSummary is the reply after the summary line, clipped to keep it within MaxReply. The line
// goes first: the thread shows a long reply cut.
func withSummary(line, reply string) string {
	room := pipeline.MaxReply - utf8.RuneCountInString(line) - 2
	if utf8.RuneCountInString(reply) > room {
		reply = string([]rune(reply)[:max(room-1, 0)]) + "…"
	}
	return line + "\n\n" + reply
}

// critique runs Gemini's critique of a ready blueprint and the answer turn, and returns the
// turn's result with them (the reply's summary line, the critique document, the revised
// blueprint). ok is false when loopd ended the lease or the runner is stopping meanwhile: then
// nothing is reported. A turn it does not apply to comes back as it was.
func (r *Runner) critique(ctx context.Context, ru *run, res pipeline.TurnResult, log *slog.Logger) (pipeline.TurnResult, bool) {
	g := ru.cfg.Gemini
	if g == nil || ru.turn.Persona != pipeline.PersonaEngineering || !res.Ready || res.BackToBrief != "" {
		return res, true
	}
	ctxDir := filepath.Join(ru.wt, ".loop", "context")
	blueprint := res.Blueprint
	if blueprint == "" {
		b, err := os.ReadFile(filepath.Join(ctxDir, pipeline.DocBlueprint+".md"))
		if err != nil {
			return res, true // loopd refuses a ready turn with no blueprint anyway
		}
		blueprint = string(b)
	}
	c := Critique{Model: g.Model, BlueprintSHA: pipeline.DocSHA256(blueprint)}
	if critiqued(ru.wt, c.BlueprintSHA) {
		log.Info("gemini: this blueprint was critiqued already")
		return res, true
	}
	brief, _ := os.ReadFile(filepath.Join(ctxDir, pipeline.DocBrief+".md"))
	left := r.maxRun(ctx, ru) - 30*time.Second - r.Clock.Now().Sub(ru.started)
	limit := min(g.TimeoutDur(), left)
	if limit < time.Minute {
		c.Reason = AgyNoTime
	} else {
		call := r.agyCall(ctx, ru, agyCallSpec{Prompt: CritiquePrompt(string(brief), blueprint), Schema: CritiqueSchema,
			Name: "gemini-critique", Status: "Gemini critiquing the blueprint"}, limit)
		c.Seconds, c.Reason = call.Seconds, call.Reason
		if c.Reason == "" {
			c.Points, c.Dropped, c.Reason = ParseCritique(call.Stdout)
			if c.Reason == AgyBadOutput && quotaRe.Match(call.Stderr) {
				c.Reason = AgyQuota
			}
		}
	}
	if c.Reason == AgyStopped {
		return res, false
	}
	if c.Reason != "" {
		log.Warn("gemini: critique unavailable", "reason", c.Reason)
		res.Reply = withSummary("_"+c.Summary()+"_", res.Reply)
		return res, true
	}
	if len(c.Points) > 0 {
		reply, sid, revised, why := r.answerCritique(ctx, ru, res.SessionID, c.Points, &c)
		switch {
		case why == "stopped":
			return res, false
		case why != "":
			c.Unanswered = why
			log.Warn("gemini: the critique was not answered", "why", why)
		default:
			res.Reply = reply
			if sid != "" {
				res.SessionID = sid
			}
			if revised != "" && revised != blueprint {
				res.Blueprint, c.RevisedSHA = revised, pipeline.DocSHA256(revised)
			}
		}
	}
	log.Info("gemini: critique", "points", len(c.Points), "taken", c.taken(), "revised", c.RevisedSHA != "")
	res.Reply = withSummary(c.Summary(), res.Reply)
	res.Critique = CritiqueDoc(c)
	return res, true
}

// answerCritique resumes the engineering session to answer the points, in the plan's directory
// with the turn's settings and guard. It returns the reply, the session, the revised blueprint
// ("" when not revised), or why there are no answers ("stopped": the lease ended).
func (r *Runner) answerCritique(ctx context.Context, ru *run, session string, points []CritiquePoint, c *Critique) (string, string, string, string) {
	if session == "" {
		return "", "", "", "no session to resume"
	}
	b, _ := json.MarshalIndent(map[string]any{"points": points}, "", "  ")
	if err := os.WriteFile(filepath.Join(ru.wt, ".loop", "context", "gemini-critique.json"), b, 0o644); err != nil {
		return "", "", "", "the runner could not write the points"
	}
	// The blueprint turn's own files stay.
	for _, f := range [][2]string{{"stream.jsonl", "stream-blueprint.jsonl"}, {"stderr.log", "stderr-blueprint.log"}, {"brief.md", "brief-blueprint.md"}} {
		if err := os.Rename(filepath.Join(ru.dir, f[0]), filepath.Join(ru.dir, f[1])); err != nil && !errors.Is(err, fs.ErrNotExist) {
			r.Log.Warn("gemini: keeping the blueprint turn's files", "file", f[0], "err", err)
		}
	}
	result := filepath.Join(ru.wt, "out", "result.json")
	os.Remove(result)
	if err := r.writeBrief(ru, CritiqueAnswerPrompt(*ru.plan, points, ru.wt)); err != nil {
		return "", "", "", "the runner could not write the prompt"
	}
	ru.resume = session
	if argv, err := json.Marshal(r.agentCommand(ru).Args[1:]); err == nil {
		os.WriteFile(filepath.Join(ru.dir, "argv-critique.json"), argv, 0o600)
	}
	// The turn has at most critiqueAnswerMax, past what the blueprint turn and agy took.
	ru.spent = r.Clock.Now().Sub(ru.started)
	ru.maxRun = ru.spent + 30*time.Second + critiqueAnswerMax
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-ru.done:
			cancel()
		case <-tctx.Done():
		}
	}()
	out := r.runAgent(tctx, ru, "")
	r.flushRefusals(ctx, ru)
	switch {
	case out.reason == endStopped || out.reason == endShutdown:
		return "", "", "", "stopped"
	case out.stats.limit != nil:
		return "", "", "", "Claude's usage limit"
	case out.reason != endExited:
		return "", "", "", "the turn ended early (" + out.reason + ")"
	}
	raw, err := readResult(result)
	if err != nil {
		return "", "", "", fmt.Sprintf("no usable out/result.json, exit %d", out.exitCode)
	}
	reply, answers, blueprint, err := ValidateCritiqueAnswers(raw, points, filepath.Join(ru.wt, "out"), r.repoSlugs())
	if err != nil {
		return "", "", "", "the answers were refused: " + clip(err.Error(), 200)
	}
	c.Answers = answers
	return reply, out.stats.sessionID, blueprint, ""
}
