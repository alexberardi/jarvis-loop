# Gemini critiques a blueprint before its Approve button (2026-10-19)

Item #191 (the user, 2026-10-17), the second piece of the Gemini second opinion
([#190's decision](2026-10-18-gemini-second-opinion.md): the `gemini` block, agy, "unavailable never
blocks"). The product and engineering personas are both Claude; a blueprint the user approves is
split into items at once, so a missing piece or a wrong dependency costs a whole round of builds.

## Decision

**When.** An engineering turn's result says `ready` (not `back_to_brief`), and the runner config
has a `gemini` block. The blueprint is the one the turn wrote, or the latest when it wrote none.
A blueprint the latest critique already read (its sha256 is in the critique) is not critiqued
again, so "looks good, ready" turns cost nothing. Product's briefs are not critiqued (see below).

**The critique** (`runner/plancritique.go`). One agy call through #190's `agyCall` (the same
binary, model, timeout, fence and failure reasons), in the plan's directory, read-only. The prompt
carries the approved brief and the blueprint inline and asks for: missing pieces, wrong order or
dependencies, risks, untestable pieces, contradictions with the brief, pieces too big for one item.
The schema: `points`, each with `severity` (high, medium, low), `section` (the blueprint's section
or piece), `kind` (missing, order, risk, untestable, contradiction, too_big) and `point`. At most
20, numbered. agy gets what is left of the turn's time cap (`plan_turn_minutes`), at most
`gemini.timeout`; under a minute left is `no_time`.

**The answers.** With points, the runner resumes the engineering session (`--resume`, the session
the turn just returned, same settings and guard) with a prompt listing them and
`.loop/context/gemini-critique.json`. It answers each, `agree` (and revises the blueprint in
`out/`) or `disagree` (and why): `{"reply", "answers": [{"id", "verdict", "why"}], "blueprint"?}`,
checked as strictly as a turn's result (one answer per point; a revised blueprint must parse and
name configured repos). The answer turn has at most 10 minutes past what the turn and agy took.
The blueprint turn's own transcript, stderr and prompt stay as `stream-blueprint.jsonl`,
`stderr-blueprint.log` and `brief-blueprint.md`.

**What the user sees.** The turn is reported ready, as always. Its reply opens with one line, first
so that the thread's clipping never hides it:

- `Gemini raised 7 points: 4 taken, 3 answered.`, then the persona's answers; the Approve button
  is for the revised blueprint, with a link to the critique;
- `Gemini raised no points on the blueprint.`;
- `Gemini raised 3 points; they were not answered (<why>): posted the blueprint as it was.`;
- `_Gemini second opinion unavailable (quota): posted without it._` (or `timeout`, `error`,
  `bad_output`, `no_start`, `no_time`), with no critique document.

**The record.** A new plan document kind, `critique`: the summary, every point with its answer
(taken, answered, or not answered), the model and the blueprint's sha256. loopd stores it like the
other documents (versioned, append-only, a `plan_doc` event) from the turn result's `critique`
field, only from engineering and only while the plan is at blueprint. It is never approved
(`ApplyPlanApprove` still takes brief or blueprint only). The board shows it after the blueprint
as "Gemini critique"; the next engineering turn finds it in `.loop/context/critique.md`.

**Advisory; `gemini.blocking` changes nothing here.** The user's spec for blueprints has no
blocking behaviour: the user approves every blueprint anyway, and the critique is in front of them
when they do. The flag is #190's (review bounces); for a blueprint it is read and ignored.

**Unavailable never holds a blueprint back.** Every agy failure, an answer turn that gives nothing
usable, or no session to resume: the blueprint goes out as the persona wrote it, the reply says
why. Only loopd ending the lease (or the runner stopping) during the critique reports nothing, as
for any stopped turn: loopd requeues it.

## Why

- Before the Approve button, not after: the critique is worth most while the blueprint can still
  change, and the persona that wrote it is the one to revise it (its session has the context).
- The persona answers rather than a fresh judge (unlike #190's adjudication): here the author
  should defend or fix its own plan, and the user reads both sides before approving.
- One summary line first: Slack shows the reply clipped at 1,800 characters.

## Not done

- **Briefs.** The same hook for product's ready briefs did not fall out of the same code cheaply:
  the answer turn revises the blueprint and validates pieces, the critique kind is per blueprint,
  and the schema's sections are the blueprint's. A follow-up can add a `brief` variant.
- The real agy against Gemini, its quota behaviour, and whether `--sandbox` nests under the fence,
  as in #190.
