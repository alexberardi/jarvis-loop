package runner

import (
	"fmt"
	"strings"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/skills"
)

// Item is the part of a loopd item the runner uses.
type Item struct {
	ID          int64    `json:"id"`
	Title       string   `json:"title"`
	Ask         string   `json:"ask"`
	Repo        string   `json:"repo"`
	Priority    string   `json:"priority"`
	Stage       string   `json:"stage"`
	Area        string   `json:"area"`
	Size        string   `json:"size"`
	Risk        string   `json:"risk"`
	HoldReason  string   `json:"hold_reason"`
	Resources   []string `json:"resources"`
	BounceCount int      `json:"bounce_count"`
	PR          int      `json:"pr"`
	Branch      string   `json:"branch"`
	HeadSHA     string   `json:"head_sha"`
	Summary     string   `json:"summary"`
	// Approval is the user's approval of a held merge, for the head they saw (land reads it).
	Approval *ItemApproval `json:"approval,omitempty"`
}

// ItemApproval is the part of an approval land needs.
type ItemApproval struct {
	By      string `json:"by"`
	HeadSHA string `json:"head_sha"`
}

// The work stages a runner can run agents for.
const (
	StageTriage = "triage"
	StageBuild  = "build"
	StageReview = "review"
)

// AgentStages are the stages a runner may claim (config `stages`; all three by default). Land is
// a script's job (rollout phase 6), not an agent's.
var AgentStages = []string{StageTriage, StageBuild, StageReview}

// RunInfo is where one run happens.
type RunInfo struct {
	Slug     string
	Base     string
	Branch   string
	Worktree string
	Scratch  string
	Sandbox  string
	// Resumed is how many commits the branch already has over the base (an earlier run's).
	Resumed int
	// Triage are the item's evidence files copied into .loop/triage/ (build); TriageErr says why
	// the copy failed.
	Triage    []TriageFile
	TriageErr string
	// Checkouts are triage's repo checkouts (repos/<key>/): the item's repo, or none when it has
	// no repo yet.
	Checkouts []Checkout
	// Routing: a triage run of an item with no repo, which names the repo first.
	Routing bool
	// RepoGroup is the item's repo's group (build and review).
	RepoGroup string
	// Verify names the runner's verify steps for the repo (build): they run after the agent.
	Verify []string
	// Context are the files the runner wrote for the agent (paths relative to the working
	// directory: .loop/context/..., .loop/review/...), with what each holds.
	Context []TriageFile
	// ContextErrs say what the runner could not fetch (the agent is told; the run goes on).
	ContextErrs []string
	// PR and HeadSHA are the PR a review run reviews, at that commit.
	PR      int
	HeadSHA string
}

// TriageFile is one file the runner gives the agent, and what it is.
type TriageFile struct {
	Name  string
	Label string
}

// Checkout is one repository checked out for a triage run.
type Checkout struct {
	Key  string
	Slug string
	Base string
	Dir  string // relative to the working directory
}

func stageSkill(stage string) string {
	skill := map[string]string{StageTriage: skills.Triage, StageBuild: skills.Build, StageReview: skills.Review,
		pipeline.PersonaProduct: skills.Product, pipeline.PersonaEngineering: skills.Engineering}[stage]
	if strings.HasPrefix(skill, "---\n") {
		if i := strings.Index(skill[4:], "\n---\n"); i >= 0 {
			skill = skill[4+i+5:]
		}
	}
	return strings.TrimSpace(skill)
}

// ItemHeading starts the per-item part of every brief: everything before it is the same for every
// run of a stage (the prompt-cache prefix).
const ItemHeading = "\n## The item\n"

// StaticBrief is the part of a stage's brief that never changes: the stage skill and the result
// contract. It is the start of the prompt, so runs of a stage share it in the prompt cache
// (docs/decisions/2026-10-15-prompt-caching.md). A plan persona's (product, engineering) is the
// start of its session's first prompt, the same for every plan (plan.go).
func StaticBrief(stage string) string {
	var b strings.Builder
	b.WriteString(stageSkill(stage))
	b.WriteString("\n\n## The result contract\n\n")
	switch stage {
	case pipeline.PersonaProduct, pipeline.PersonaEngineering:
		b.WriteString("Every turn, write `out/result.json` as your last step:\n\n```json\n")
		if stage == pipeline.PersonaProduct {
			b.WriteString(`{"reply": "the Slack message: at most 3,000 characters, no logs or transcripts",
 "brief": "brief.md", "ready": false, "repos": ["owner/name"]}`)
			b.WriteString("\n```\n\n")
			b.WriteString("`reply` is required. `brief` names the brief you wrote this turn, a file in `out/` (leave it out when the brief did not change). `ready: true` posts the Approve button for the latest brief. `repos` sets the plan's repos (configured `owner/name` slugs from `.loop/context/repos.md`; leave it out to keep them). No other fields.\n")
		} else {
			b.WriteString(`{"reply": "the Slack message: at most 3,000 characters, no logs or transcripts",
 "blueprint": "blueprint.md", "ready": false}`)
			b.WriteString("\n```\n\n```json\n")
			b.WriteString(`{"reply": "the Slack message: why the brief has to change", "back_to_brief": "what is wrong with the brief, and what a workable one would change"}`)
			b.WriteString("\n```\n\n")
			b.WriteString("`reply` is required. `blueprint` names the blueprint you wrote this turn, a file in `out/` (leave it out when it did not change). `ready: true` posts the Approve button for the latest blueprint. `back_to_brief` sends the plan back to the product agent (never with `ready` or a blueprint). No other fields.\n")
		}
		b.WriteString("\nThe runner checks the result before loopd sees it: a missing reply, a reply over 3,000 characters, a document that is not a plain file in `out/`, an unknown repo, or a blueprint whose pieces do not parse or whose dependencies loop fails the turn with that reason, and the user is asked to reply to try again (your session is kept).\n")
	case StageTriage:
		b.WriteString("Write `.loop/result.json` as your last step, exactly one of:\n\n```json\n")
		b.WriteString(`{"outcome": "reproduced", "repo": "owner/name (only when the item has no repo)",
 "area": "a short area name", "size": "S | M | L", "risk": "normal | hold", "resources": ["gpu", "voice", "emulator", "macos"],
 "summary": "one paragraph: what you found", "notes": "markdown: what to change, where, how to verify, what you could not check",
 "evidence": [{"file": "evidence/repro.log", "label": "the failing test on main"}],
 "needs_qa": false}`)
		b.WriteString("\n```\n\n```json\n")
		b.WriteString(`{"outcome": "not_reproducible", "summary": "duplicate of #N / PR #M, or: works on main because …",
 "notes": "markdown: what you checked and saw", "evidence": [{"file": "evidence/check.log", "label": "…"}]}`)
		b.WriteString("\n```\n\n```json\n{\"outcome\": \"question\", \"question\": \"…\"}\n```\n\n")
		b.WriteString("When the item has no repo yet (no checkouts this run), name it first:\n\n```json\n")
		b.WriteString(`{"outcome": "routed", "repo": "owner/name", "why": "one line: why this repo"}`)
		b.WriteString("\n```\n\n")
		b.WriteString("`resources` lists only what verifying the change needs (often `[]`). The runner checks `repo` against the configured repos and uploads your `notes` (as `triage-notes.md`) and the files in `.loop/evidence/` with the item. After `routed`, the item is triaged again with that repo checked out. Set `needs_qa` to true when only the user can check the change once it is merged (on their phone or hardware): the item then waits in qa after it lands. No other fields.\n")
	case StageReview:
		b.WriteString("Write `.loop/result.json` as your last step, exactly one of:\n\n```json\n")
		b.WriteString(`{"outcome": "approved", "summary": "one paragraph: why it can merge",
 "review": "markdown for the PR comment: verdict, what you checked and how, findings",
 "not_verified": ["…"], "evidence": [{"file": "evidence/check.log", "label": "scripts/check at the head"}],
 "needs_review": false, "needs_qa": false}`)
		b.WriteString("\n```\n\n```json\n")
		b.WriteString(`{"outcome": "bounce", "notes": "the build agent's list: each point specific, with file:line, and what done looks like",
 "review": "markdown for the PR comment", "evidence": [{"file": "evidence/check.log", "label": "…"}]}`)
		b.WriteString("\n```\n\n")
		b.WriteString("The runner posts `review` on the PR as a comment (never a GitHub approval), with the commit you reviewed, uploads `.loop/evidence/`, and reports the outcome for the head commit named below; `head_sha`, if you give it, must be that commit. `needs_review: true` on an approval holds the merge for the user (see \"Content the hold list cannot see\"). `needs_qa: true` on an approval means only the user can check the change once merged (on their phone or hardware): the item then waits in qa after it lands instead of going to done. No other fields.\n")
	default:
		b.WriteString("Commit your work on the item's branch (the runner pushes it and opens the PR; you never push), then write `.loop/result.json`, exactly one of:\n\n```json\n")
		b.WriteString(`{"outcome": "ready_for_pr", "pr_title": "one line, at most 256 characters",
 "pr_body": "markdown, at most 60,000 characters: what changed, the tests, what was not verified; name evidence files by file name",
 "summary": "one paragraph", "not_verified": ["…"], "evidence": [{"file": "evidence/after.png", "label": "after, phone"}],
 "needs_review": false}`)
		b.WriteString("\n```\n\n```json\n{\"outcome\": \"question\", \"question\": \"…\"}\n```\n\n")
		b.WriteString("`ready_for_pr` means everything is committed: the runner refuses a worktree with uncommitted changes, and reports git's own HEAD and changed files. It adds a Verification footer (the evidence files, the run's numbers) to your `pr_body`. Set `needs_review` to true when the change touches something the user should look at before it merges. No other fields.\n")
	}
	return b.String()
}

// Brief is .loop/brief.md for a run of a stage: the static part, then the item and the run. It is
// also the prompt.
func Brief(stage string, it Item, r RunInfo) string {
	var b strings.Builder
	b.WriteString(StaticBrief(stage))
	fmt.Fprintf(&b, "%s\nThis brief is also saved as `.loop/brief.md`.\n\n- Item: #%d: %s\n", ItemHeading, it.ID, it.Title)
	fmt.Fprintf(&b, "- Stage: %s\n- Priority: %s\n", stage, orDash(it.Priority))
	switch stage {
	case StageTriage:
		switch {
		case it.Repo != "":
			fmt.Fprintf(&b, "- Repository: `%s` (set when the item was added or routed; leave `repo` out of the result)\n", r.Slug)
		case r.Routing:
			b.WriteString("- Repository: none yet. **Route it**: pick its repo from `.loop/context/repos.md` (the ask, each repo's group, aliases, description and recent PR titles) and return `routed` with the slug and why. Nothing is checked out this run; the runner checks the repo out for the next triage run. Return `reproduced` with `repo` only if that file alone settles everything (rare); `question` if you cannot tell.\n")
		default:
			b.WriteString("- Repository: none yet. Pick one of the configured repos (`.loop/context/repos.md`) and name its slug in `repo`.\n")
		}
		if len(r.Checkouts) == 0 {
			b.WriteString("- Checkouts: none\n")
		} else {
			b.WriteString("- Checkouts (detached at the base branch):")
			for _, c := range r.Checkouts {
				fmt.Fprintf(&b, " `%s/` (`%s`, `origin/%s`);", c.Dir, c.Slug, c.Base)
			}
			b.WriteString("\n")
		}
	case StageReview:
		fmt.Fprintf(&b, "- Repository: `%s` (base branch `%s`%s)\n- PR: #%d, branch `%s`, head `%s` (review this commit)\n", r.Slug, r.Base, groupNote(r.RepoGroup), r.PR, r.Branch, r.HeadSHA)
		if it.Area != "" || it.Size != "" {
			fmt.Fprintf(&b, "- Triage: area %s, size %s, risk %s\n", orDash(it.Area), orDash(it.Size), orDash(it.Risk))
		}
		if it.HoldReason != "" {
			fmt.Fprintf(&b, "- On the hold list: %s (the user approves the merge after you)\n", it.HoldReason)
		}
		if it.BounceCount > 0 {
			fmt.Fprintf(&b, "- Review bounced it %d time(s) before; check each earlier point was addressed.\n", it.BounceCount)
		}
		if it.Summary != "" {
			fmt.Fprintf(&b, "- The build's summary: %s\n", it.Summary)
		}
	default:
		fmt.Fprintf(&b, "- Repository: `%s` (base branch `%s`%s; the PR's changes are against `origin/%s`)\n", r.Slug, r.Base, groupNote(r.RepoGroup), r.Base)
		if it.Area != "" || it.Size != "" {
			fmt.Fprintf(&b, "- Triage: area %s, size %s, risk %s\n", orDash(it.Area), orDash(it.Size), orDash(it.Risk))
		}
		if it.HoldReason != "" {
			fmt.Fprintf(&b, "- On the hold list: %s (the user approves the merge; build it as asked)\n", it.HoldReason)
		}
		switch {
		case it.BounceCount > 0 && it.PR > 0:
			fmt.Fprintf(&b, "- Sent back %d time(s) (by review, or by the runner's verify); PR #%d is open on this branch. Fix every point in `.loop/review/` on the same branch; the runner updates that PR.\n", it.BounceCount, it.PR)
		case it.BounceCount > 0:
			fmt.Fprintf(&b, "- Sent back %d time(s) by the runner's verify (nothing was pushed). Fix what `.loop/review/verify.md` shows on the same branch.\n", it.BounceCount)
		}
		if len(r.Verify) > 0 {
			b.WriteString("- After you return `ready_for_pr`, the runner runs this repo's verify steps itself (outside your sandbox), before it pushes:")
			for _, v := range r.Verify {
				fmt.Fprintf(&b, " %s;", oneLine(v))
			}
			b.WriteString(" a failure comes back to you in `.loop/review/verify.md`.\n")
		}
		if it.Summary != "" {
			fmt.Fprintf(&b, "- Notes so far: %s\n", it.Summary)
		}
		if len(r.Triage) > 0 {
			b.WriteString("- Triage notes and evidence so far (read them first):")
			for _, f := range r.Triage {
				writeFile(&b, ".loop/triage/"+f.Name, f.Label)
			}
			b.WriteString("\n")
		}
		if r.TriageErr != "" {
			fmt.Fprintf(&b, "- The item's triage evidence could not be copied (%s).\n", clip(r.TriageErr, 200))
		}
	}
	if len(r.Context) > 0 {
		b.WriteString("- Context from the runner (read these):")
		for _, f := range r.Context {
			writeFile(&b, f.Name, f.Label)
		}
		b.WriteString("\n")
	}
	for _, e := range r.ContextErrs {
		fmt.Fprintf(&b, "- Not available this run: %s\n", clip(e, 200))
	}
	ask := strings.TrimSpace(it.Ask)
	if ask == "" {
		ask = it.Title
	}
	fmt.Fprintf(&b, "\nThe ask:\n\n> %s\n", strings.ReplaceAll(ask, "\n", "\n> "))
	b.WriteString("\n## This run\n\n")
	switch stage {
	case StageTriage:
		if len(r.Checkouts) == 0 {
			fmt.Fprintf(&b, "- Working directory (not a repository; nothing is checked out): `%s`\n", r.Worktree)
		} else {
			fmt.Fprintf(&b, "- Working directory (not a repository; the checkouts are under it): `%s`\n", r.Worktree)
		}
	case StageReview:
		fmt.Fprintf(&b, "- Worktree (your working directory, the PR at `%s`, detached; never commit): `%s`\n", short7(r.HeadSHA), r.Worktree)
	default:
		fmt.Fprintf(&b, "- Worktree (your working directory): `%s`\n- Branch: `%s` (checked out; commit on it, the runner pushes it)\n", r.Worktree, r.Branch)
		if r.Resumed > 0 {
			s := "s"
			if r.Resumed == 1 {
				s = ""
			}
			fmt.Fprintf(&b, "- Resumed: this branch already has %d commit%s from a previous run; check them (`git log origin/%s..HEAD`) before redoing work.\n", r.Resumed, s, r.Base)
		}
	}
	fmt.Fprintf(&b, "- Scratch dir (temp files, GOTMPDIR): `%s`\n- Sandbox name for the harness: `%s`\n", r.Scratch, r.Sandbox)
	return b.String()
}

// BuildBrief is the build stage's brief.
func BuildBrief(it Item, r RunInfo) string { return Brief(StageBuild, it, r) }

func groupNote(g string) string {
	if g == "" {
		return ""
	}
	return ", group " + g
}

func writeFile(b *strings.Builder, name, label string) {
	if label != "" {
		fmt.Fprintf(b, " `%s` (%s);", name, strings.Join(strings.Fields(label), " "))
	} else {
		fmt.Fprintf(b, " `%s`;", name)
	}
}

func short7(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
