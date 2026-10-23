package runner

import (
	"fmt"
	"slices"
	"strings"
	"time"

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
	MergeSHA    string   `json:"merge_sha,omitempty"`
	Summary     string   `json:"summary"`
	// Source is who filed it (finding: the loop itself, as a red main's P1).
	Source string `json:"source"`
	// Approval is the user's approval of a held merge, for the head they saw (land reads it).
	Approval *ItemApproval `json:"approval,omitempty"`
	// Trust is verify's refusal of a trusted-file change and the approver's OK for that commit
	// (pipeline.Trust): verify runs that one commit's change once.
	Trust *ItemTrust `json:"trust,omitempty"`
	// Builder is who builds it (pipeline.BuilderQwen: OpenCode, opencode.go; else claude), and
	// BuildSession the OpenCode session its last qwen build left, which the next one resumes.
	Builder      string `json:"builder,omitempty"`
	BuildSession string `json:"build_session,omitempty"`
	// PlanID and PlanPiece link an item filed from a plan to its piece (0: not from a plan).
	PlanID    int64 `json:"plan_id,omitempty"`
	PlanPiece int   `json:"plan_piece,omitempty"`
	// Proof is how the loop checks it once merged (the runner runs its command checks).
	Proof *pipeline.Proof `json:"proof,omitempty"`
	// CIPendingSince and CIPendingSHA are when CI started pending on that head, across land runs
	// that ended in ci_pending (land reads them for land_ci_give_up).
	CIPendingSince time.Time `json:"ci_pending_since,omitzero"`
	CIPendingSHA   string    `json:"ci_pending_sha,omitempty"`
}

// ItemTrust is the part of a trust OK the runner needs.
type ItemTrust struct {
	HeadSHA string `json:"head_sha"`
	By      string `json:"by"`
	Used    bool   `json:"used"`
	// Paths and DiffSHA256 are the trusted files and their diff's digest the approver OK'd: a new
	// head with the same ones carries the OK (carriedTrust).
	Paths      []string `json:"paths"`
	DiffSHA256 string   `json:"diff_sha256"`
}

// allows reports whether the approver's OK lets verify run head's change to trusted files.
func (t *ItemTrust) allows(head string) bool {
	return t != nil && t.By != "" && !t.Used && head != "" && t.HeadSHA == head
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
	// FollowedBy are the open items filed after this one (they depend on it): the remaining parts
	// of its ask, if it was split.
	FollowedBy []int64
	Slug       string
	Base       string
	Branch     string
	Worktree   string
	Scratch    string
	Sandbox    string
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
	// Verify names the runner's verify steps for the repo (build): they run after the agent. A
	// step with when_changed globs is named with them, since it runs only when a path the agent
	// changes matches one, and VerifyConditional is then true: such a step is no route on its own
	// (#546).
	Verify            []string
	VerifyConditional bool
	// Context are the files the runner wrote for the agent (paths relative to the working
	// directory: .loop/context/..., .loop/review/...), with what each holds.
	Context []TriageFile
	// HeldNotes is the file saying why a held item came back to build ("" when it did not).
	HeldNotes string
	// HeldFrom is the stage the newest of those notes sent it back from (held or needs_you).
	HeldFrom string
	// HeldMainRed: the newest of those notes is loopd's, for a PR whose CI failed while main was
	// red (pipeline.ApplyMainRedRebuild).
	HeldMainRed bool
	// Answers is the file with the questions agents asked on the item and the user's answers (""
	// when there were none).
	Answers string
	// PlanID and PlanPiece are the plan an item was filed from and its piece (0: none), and
	// PlanDocs the plan's approved documents the runner wrote (.loop/context/blueprint.md,
	// brief.md).
	PlanID    int64
	PlanPiece int
	PlanDocs  []string
	// PreMerge is the runner's merge of the base into the branch before the build (nil: none).
	PreMerge *PreMerge
	// ContextErrs say what the runner could not fetch (the agent is told; the run goes on).
	ContextErrs []string
	// CILog is the failing CI log the runner fetched (triage of a red main's P1; build after land
	// sent the item back for red CI), relative to the working directory.
	CILog *TriageFile
	// Harness is the repo's harness when the agent has a session (build, review), and HarnessBin
	// the loop-harness on its PATH. HarnessDir and HarnessRepo are the checkout its commands run
	// in and that repo's slug, when the harness names a repo of its own (harness.repo); "" when
	// they run in the agent's worktree.
	Harness     *HarnessConfig
	HarnessBin  string
	HarnessDir  string
	HarnessRepo string
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
		pipeline.PersonaProduct: skills.Product, pipeline.PersonaEngineering: skills.Engineering, StageOrchestrate: skills.Orchestrator}[stage]
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
			b.WriteString("`reply` is required. `brief` names the brief you wrote this turn, a plain file directly in `out/`, such as `brief.md` (leave it out when the brief did not change). `ready: true` posts the Approve button for the latest brief. `repos` sets the plan's repos (configured `owner/name` slugs from `.loop/context/repos.md`; leave it out to keep them). No other fields.\n")
		} else {
			b.WriteString(`{"reply": "the Slack message: at most 3,000 characters, no logs or transcripts",
 "blueprint": "blueprint.md", "ready": false}`)
			b.WriteString("\n```\n\n```json\n")
			b.WriteString(`{"reply": "the Slack message: why the brief has to change", "back_to_brief": "what is wrong with the brief, and what a workable one would change"}`)
			b.WriteString("\n```\n\n")
			b.WriteString("`reply` is required. `blueprint` names the blueprint you wrote this turn, a plain file directly in `out/`, such as `blueprint.md` (leave it out when it did not change). `ready: true` posts the Approve button for the latest blueprint. `back_to_brief` sends the plan back to the product agent (never with `ready` or a blueprint). No other fields.\n")
		}
		b.WriteString("\nThe runner checks the result before loopd sees it: a missing reply, a reply over 3,000 characters, a document that is not a plain file directly in `out/` (no subdirectories, no symlinks), an unknown repo, or a blueprint whose pieces do not parse or whose dependencies loop fails the turn with that reason, and the user is asked to reply to try again (your session is kept).\n")
	case StageTriage:
		b.WriteString("Write `.loop/result.json` as your last step, exactly one of:\n\n```json\n")
		b.WriteString(`{"outcome": "reproduced", "repo": "owner/name (only when the item has no repo)",
 "area": "a short area name", "size": "S | M | L", "risk": "normal | hold", "resources": ["gpu", "voice", "emulator", "macos"],
 "change_kind": "code | docs | test", "builder": "qwen | claude (optional)",
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
		b.WriteString("`resources` lists only what verifying the change needs (often `[]`). The runner checks `repo` against the configured repos and uploads your `notes` (as `triage-notes.md`) and the files in `.loop/evidence/` with the item. After `routed`, the item is triaged again with that repo checked out. Set `needs_qa` to true when only the user can check the change once it is merged (on their phone or hardware): the item then waits in qa after it lands. `change_kind` is `docs` or `test` when the change touches only docs or only tests, else `code` (left out: code). `builder` is optional: `claude` when the build needs judgment the Qwen builder lacks (a design call, many areas), `qwen` when every step is mechanical; left out, loopd decides by priority, size and change_kind. Closing an item as a duplicate means its ask still stands, tracked elsewhere: start the summary with `duplicate of #N` (loopd reads that id, and a main-red P1 closed as a duplicate hands its repo's freeze to #N instead of deadlocking it). Any other `not_reproducible` summary says the ask itself does not stand. No other fields.\n")
	case StageReview:
		b.WriteString("Write `.loop/result.json` as your last step, exactly one of:\n\n```json\n")
		b.WriteString(`{"outcome": "approved", "summary": "one paragraph: why it can merge",
 "review": "markdown for the PR comment: verdict, what you checked and how, findings",
 "not_verified": ["…"], "evidence": [{"file": "evidence/check.log", "label": "scripts/check at the head"}],
 "needs_review": false, "needs_qa": false,
 "ask_steps": [{"step": "1. the ask's first step", "status": "done"},
  {"step": "2. …", "status": "not_done", "reason": "why", "follow_up": 12},
  {"step": "3. …", "status": "out_of_scope", "reason": "why"}]}`)
		b.WriteString("\n```\n\n```json\n")
		b.WriteString(`{"outcome": "bounce", "notes": "the build agent's list: each point specific, with file:line, and what done looks like",
 "review": "markdown for the PR comment", "evidence": [{"file": "evidence/check.log", "label": "…"}],
 "ask_steps": [{"step": "…", "status": "not_done", "reason": "why"}]}`)
		b.WriteString("\n```\n\n```json\n{\"outcome\": \"wait\", \"on\": [472], \"prs\": [\"owner/name#444\"], \"reason\": \"one line: what must be done first, and why\"}\n```\n\n")
		b.WriteString("The runner posts `review` on the PR as a comment (never a GitHub approval), with the commit you reviewed, uploads `.loop/evidence/`, and reports the outcome for the head commit named below; `head_sha`, if you give it, must be that commit. `needs_review: true` on an approval holds the merge for the user (see \"Content the hold list cannot see\"). `needs_qa: true` on an approval means only the user can check the change once merged (on their phone or hardware): the item then waits in qa after it lands instead of going to done. `ask_steps` lists each step of the item's ask, judged against the diff: `done`, `not_done` or `out_of_scope`, with a `reason` for any step not done; `follow_up` is the loop item filed for a step not done (it must be an open item filed after this one, `loop add --after`). An approval with a step `not_done` and no such follow-up is a bounce: the runner turns it into one. `wait` (see below) parks the item when the PR cannot pass review until other work is done. No other fields.\n\n" + waitNote)
	default:
		b.WriteString("Commit your work on the item's branch (the runner pushes it and opens the PR; you never push), then write `.loop/result.json`, exactly one of:\n\n```json\n")
		b.WriteString(`{"outcome": "ready_for_pr", "pr_title": "one line, at most 256 characters",
 "pr_body": "markdown, at most 60,000 characters: what changed, the tests, what was not verified; name evidence files by file name",
 "summary": "one paragraph", "not_verified": ["…"], "evidence": [{"file": "evidence/after.png", "label": "after, phone"}],
 "needs_review": false,
 "proof": [{"kind": "command", "run": "go test ./internal/web -run TestBoardQA"}, {"kind": "manual", "look": "the QA column on a phone"}]}`)
		b.WriteString("\n```\n\n```json\n{\"outcome\": \"question\", \"question\": \"…\"}\n```\n\n```json\n{\"outcome\": \"wait\", \"on\": [472], \"prs\": [\"owner/name#444\"], \"reason\": \"one line: what must be done first, and why\"}\n```\n\n")
		b.WriteString("`ready_for_pr` means everything is committed: the runner refuses a worktree with uncommitted changes, and reports git's own HEAD and changed files. It adds a Verification footer (the evidence files, the run's numbers, your proof) to your `pr_body`. Set `needs_review` to true when the change touches something the user should look at before it merges. No other fields.\n\n")
		b.WriteString("`proof` (optional, at most 10 checks) is how the loop can tell, once merged, that the change works; an item that waits in qa is checked with it. Kinds: `usage` (`event`: a loopd event kind seen since the merge, with optional `actor` and `match` lists on its data fields: `{\"kind\": \"usage\", \"event\": \"claimed\", \"actor\": [\"runner:mbp\"]}`); `command` (`run`: one shell line the runner runs fenced in a checkout of main, exit 0 passes); `pi` (`run`, for the test Pi; it waits until the loop has one); `e2e` (`tests`: E2E specs or flows, as jarvis-server's e2e/coverage.json lists them for the pages and screens an app change touches; the next regression run that runs them decides it); `manual` (`look`: one line, what only the user can judge). Every automatic check passing verifies the item without the user; a command failing twice in a row, or an E2E test failing, reopens it. So make each check really show the change works, and use `manual` only for looks, feel or a product call.\n\n")
		b.WriteString(waitNote)
	}
	return b.String()
}

// waitNote is the build and review contracts' wait outcome (docs/decisions/2026-10-22-waiting-stage.md).
const waitNote = "`wait` parks the item instead of asking the user when its next step needs other work done first: " +
	"`on` the loop items (ids) that must be merged, `prs` the PRs that are not loop items (owner/name#444) that must merge on GitHub, " +
	"`reason` one line. loopd moves it back to this stage by itself once they are all merged; one that is dropped, or a wait that would " +
	"make a cycle, goes to the user. Use it only when that work exists already (an open item or PR); otherwise ask. A build's work so far " +
	"stays on the branch. No other fields.\n"

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
		b.WriteString(followedByLine(r.FollowedBy))
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
		switch {
		case r.HeldNotes != "" && r.HeldMainRed:
			fmt.Fprintf(&b, "- CI failed on PR #%d while `%s` was red; `%s` is green now. Merge `%s` into this branch (the runner never force-pushes, so no rebase) and fix what CI still shows, on the same branch; the runner updates that PR. loopd's note is in `%s`.\n", it.PR, r.Base, r.Base, r.Base, r.HeldNotes)
		case r.HeldNotes != "" && r.HeldFrom == string(pipeline.NeedsYou) && it.PR > 0:
			fmt.Fprintf(&b, "- Sent back to build by the user instead of an answer to its question; PR #%d is open on this branch. Do what `%s` asks on the same branch; the runner updates that PR.\n", it.PR, r.HeldNotes)
		case r.HeldNotes != "" && r.HeldFrom == string(pipeline.NeedsYou):
			fmt.Fprintf(&b, "- Sent back to build by the user instead of an answer to its question. Do what `%s` asks.\n", r.HeldNotes)
		case r.HeldNotes != "":
			fmt.Fprintf(&b, "- Sent back from held; PR #%d is open on this branch. Do what `%s` asks (the user's note, or a conflict with `%s`) on the same branch; the runner updates that PR, and the user approves the new head.\n", it.PR, r.HeldNotes, r.Base)
		}
		if pm := r.PreMerge; pm != nil {
			taken := ""
			if len(pm.Taken) > 0 {
				taken = fmt.Sprintf(", and took `%s`'s side of the conflicts under `.claude/` (%s)", r.Base, "`"+strings.Join(pm.Taken, "`, `")+"`")
			}
			if pm.Committed {
				fmt.Fprintf(&b, "- **The runner merged `origin/%s`** (at `%s`) into this branch before you started (the sandbox keeps `.claude/` read-only for you, so your own merge would fail)%s. Do not merge `%s` again.\n", r.Base, short7(pm.SHA), taken, r.Base)
			} else {
				fmt.Fprintf(&b, "- **A merge of `origin/%s` (at `%s`) is in progress**: the runner started it (the sandbox keeps `.claude/` read-only for you)%s. Resolve only these conflicts: %s; `git add` them and `git commit --no-edit`. Do not abort the merge or start another, and leave `.claude/` as it is.\n", r.Base, short7(pm.SHA), taken, "`"+strings.Join(pm.Conflicts, "`, `")+"`")
			}
		}
		if it.Trust != nil && it.Trust.allows(it.Trust.HeadSHA) {
			// Only when the runner kept no result for the OK'd head (trusted.go): else no agent runs.
			fmt.Fprintf(&b, "- The user OK'd verify running commit `%s`, which changes files the runner trusts (deny_write). If that commit is this item's work, add no commit: return `ready_for_pr` for it as it is. Any new commit needs a new OK from the user.\n", it.Trust.HeadSHA)
		}
		if len(r.Verify) > 0 {
			b.WriteString("- After you return `ready_for_pr`, the runner runs this repo's verify steps itself (outside your sandbox), before it pushes:")
			for _, v := range r.Verify {
				fmt.Fprintf(&b, " %s;", oneLine(v))
			}
			b.WriteString(" a failure comes back to you in `.loop/review/verify.md`, which is all there is of it (the harness it ran on is the user's; do not look for it).\n")
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
	if r.CILog != nil {
		fmt.Fprintf(&b, "- **CI failed; the runner fetched its log**: `%s` (%s): the failing tests, then the lines around each failure (or each job's tail), from GitHub Actions. You cannot open the CI run yourself, and the user cannot reproduce it for you. A test that passes locally but fails in CI is usually a race or a timing assumption that a slower machine exposes: look for the race in the code (in Go, `go test -race -count=20 -run <test>`), don't ask the user.\n",
			r.CILog.Name, strings.Join(strings.Fields(r.CILog.Label), " "))
	}
	if r.Answers != "" {
		fmt.Fprintf(&b, "- **The user already answered questions on this item**: `%s`. Read it first. An answer overrides the ask where they conflict (for example, a step it says to skip or list under not verified), and the newest answer wins. Do not ask a question that file already answers; act on the answer.\n", r.Answers)
	}
	if r.PlanID > 0 && len(r.PlanDocs) > 0 {
		docs := make([]string, len(r.PlanDocs))
		for i, f := range r.PlanDocs {
			docs[i] = "`" + f + "`"
		}
		fmt.Fprintf(&b, "- **This item is piece %d of plan P%d.** Your piece is in the ask; the plan's whole approved documents are in %s, for the sections your piece points at (a table, the rules, the names). Read them there; do not ask the user for what they contain.\n", r.PlanPiece, r.PlanID, strings.Join(docs, " and "))
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
	if len(it.Resources) > 0 {
		fmt.Fprintf(&b, "- What this item needs from a runner (triage set it; this runner was claimed for it): %s\n", codeList(it.Resources))
	}
	if slices.Contains(it.Resources, pipeline.ResourceEmulator) {
		b.WriteString(emulatorNote(stage, r))
	}
	if r.Harness != nil {
		b.WriteString(harnessNote(stage, r))
	}
	return b.String()
}

// codeList is the strings as a comma-separated list in backticks.
func codeList(v []string) string {
	out := make([]string, len(v))
	for i, s := range v {
		out[i] = "`" + s + "`"
	}
	return strings.Join(out, ", ")
}

// emulatorNote tells an agent of an item that needs the emulator what it can reach. Its own
// sandbox never can: no /dev/kvm, no Android tooling, and adb is denied outright. The routes are
// the harness through the runner and the runner's own verify steps, both in the fence, which has
// the host's devices (#546).
func emulatorNote(stage string, r RunInfo) string {
	b := strings.Builder{}
	b.WriteString("- **The emulator**: this item needs it, and your own sandbox cannot reach it — no `/dev/kvm`, no `emulator`, `maestro` or `adb` on PATH, and `adb` is refused outright (emulator work goes through the harness scripts, which pass the serial themselves). ")
	switch {
	case r.Harness != nil:
		b.WriteString("Your way to it is `loop-harness` (just below): what it runs happens in the runner's fence, which has the host's devices, the Android SDK and the emulator.\n")
	case stage == StageTriage:
		b.WriteString("A triage run has no way to it at all (the harness through the runner is build and review only), so do not try: say in your notes what could not be reproduced and name the commands a build would have to run.\n")
	case len(r.Verify) > 0:
		b.WriteString("Nothing you run reaches it, but the runner's verify steps (above) run in that fence after you: make the change and the flows, and say in the PR body that the emulator run is the runner's, not yours.")
		if r.VerifyConditional {
			b.WriteString(" A step named `only when a path you change matches ...` is no route on its own: if nothing you commit matches its paths, it is skipped and nothing runs on the emulator this time — say that in `not_verified`.")
		}
		b.WriteString("\n")
	default:
		b.WriteString("Nothing in this run reaches it: this repo has no harness through the runner and no verify step that would run after you. Say that in `not_verified` (or return `question`) rather than claiming a run you could not do.\n")
	}
	return b.String()
}

// harnessNote tells a build or review agent how to use the repo's harness through the runner.
func harnessNote(stage string, r RunInfo) string {
	var b strings.Builder
	b.WriteString("\n## The harness, through the runner\n\n")
	fmt.Fprintf(&b, "Your sandbox cannot run this repo's harness (no loopback, no harness credentials, no `/dev/kvm`, and what a command starts dies with it), so the runner runs it for you with `loop-harness` (on your PATH; `%s`), in the fence its own verify steps use: the host's network, the harness's credentials (the user's other credentials stay hidden), this worktree writable. Run it from this worktree:\n\n", r.HarnessBin)
	if r.HarnessDir != "" {
		fmt.Fprintf(&b, "Its commands do not run here: the harness scripts are in `%s`, which the runner checked out fresh at its base branch for this run (`%s`, also `$LOOP_HARNESS_DIR`). That checkout is the working directory of every `loop-harness` command; your own worktree is writable beside it and its path is `$LOOP_WORKTREE`, so pass it to a script that needs your change (as the regression run passes `--mobile-repo`). Call `loop-harness` itself from this worktree.\n\n", r.HarnessRepo, r.HarnessDir)
	}
	fmt.Fprintf(&b, "- `loop-harness up`: runs `%s` and keeps what it starts running until your run ends.\n", oneLine(r.Harness.Up))
	// Where a command's files land is the one thing that differs: in the worktree they are simply
	// here, while the checkout of another repo is removed with the run, so what is kept goes
	// through $LOOP_EVIDENCE_DIR.
	where, writes := "this worktree", "files it writes here (screenshots) you can Read"
	if r.HarnessDir != "" {
		where = "that checkout"
		writes = "files it writes under `$LOOP_EVIDENCE_DIR` (screenshots) you can Read, and that is where screenshots have to go: the checkout itself is removed when your run ends"
	}
	fmt.Fprintf(&b, "- `loop-harness run '<command>'`: one command in %s, next to the running harness", where)
	fmt.Fprintf(&b, " (e.g. `go run ./tools/pipeline say ...`, `scripts/mobile-shot --sandbox $LOOP_SANDBOX ...`). Its output streams back and its exit code is `loop-harness`'s; %s. Without `up` it runs alone in a fresh fence.\n", writes)
	fmt.Fprintf(&b, "- `loop-harness down`: runs `%s`. `loop-harness status`: is it up.\n", oneLine(r.Harness.Down))
	fmt.Fprintf(&b, "\nEach command may take %s; `$LOOP_SANDBOX` is `%s`. Every command's full log is in `.loop/scratch/harness/<n>.log`. The runner ends the harness when your run ends", r.Harness.Timeout(), r.Sandbox)
	if stage == StageBuild {
		b.WriteString(", and its verify steps still run after you, on their own harness. Copy what you looked at (screenshots, the log of the run that shows the change) into `.loop/evidence/` and name it in the PR body; what the harness showed is verified, so leave it out of `not_verified`.\n")
	} else {
		b.WriteString(". Name in your review what you ran and what it showed.\n")
	}
	return b.String()
}

// RetryHeading opens the section RetryBrief adds.
const RetryHeading = "## Your previous result was refused"

// RetryBrief is the brief for the one retry of a stage whose result the runner refused: the same
// brief (the prompt cache keeps its prefix), with why at the end.
func RetryBrief(brief, why string) string {
	return strings.TrimRight(brief, "\n") + "\n\n" + RetryHeading + "\n\n" +
		"An earlier run of this stage ended, but the runner could not accept its `.loop/result.json`:\n\n> " +
		oneLine(why) + "\n\n" +
		"Your work so far is still here (the worktree, `.loop/evidence/`). Fix what the runner refused, then write " +
		"`.loop/result.json` again. This is the one retry: a second refusal goes to the user as a runner error.\n"
}

// NoResultHeading starts NoResultNote.
const NoResultHeading = "# Your run ended without a result\n"

// NoResultNote is the prompt of a session resumed because its run exited with no
// .loop/result.json (#208): the session has the brief already.
const NoResultNote = NoResultHeading + "\nYour last turn ended without `.loop/result.json`, most likely while a command ran in the background " +
	"(`run_in_background`, Monitor or `&`). This is a one-shot run: it ends when you stop, a background task never reports back, " +
	"and its process is gone now. This is the same session, resumed once, in the same worktree.\n\n" +
	"Re-run in the foreground what you were waiting for (raise the Bash `timeout`, up to 600000 ms, or split a long check), " +
	"then finish with `.loop/result.json` as the brief (still in `.loop/brief.md`) says. If this run ends without it too, the item goes to the user.\n"

// followedByLine names the items filed after this one, for ask_steps' follow_up and a PR's
// remaining parts ("" when none).
func followedByLine(ids []int64) string {
	if len(ids) == 0 {
		return "- Filed after this item: none (a step of the ask left undone has no follow-up item yet).\n"
	}
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = fmt.Sprintf("#%d", id)
	}
	return "- Filed after this item (they wait for it; a step left undone may name one as its `follow_up`): " + strings.Join(s, ", ") + "\n"
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
