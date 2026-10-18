package pipeline

import (
	"fmt"
	"strings"
)

// Frozen maps each repo whose main is red to the item that fixes it, its main-red P1
// (docs/decisions/2026-10-18-main-red-freeze.md). Nothing else in a frozen repo is claimed and
// its runs' advances wait; the fix is exempt and goes first.
type Frozen map[string]int64

// Blocks reports whether the freeze holds it: its repo is frozen and it is not the fix.
func (f Frozen) Blocks(it Item) bool {
	fix, ok := f[it.Repo]
	return ok && it.Repo != "" && it.ID != fix
}

// Fixes reports whether it is the main-red item of its frozen repo.
func (f Frozen) Fixes(it Item) bool {
	fix, ok := f[it.Repo]
	return ok && it.Repo != "" && it.ID == fix
}

// MainRedTitle is the title of a repo's main-red P1.
func MainRedTitle(slug string, jobs []string) string {
	t := fmt.Sprintf("main red in %s: %s", slug, strings.Join(jobs, ", "))
	if len(t) > 200 {
		t = t[:197] + "..."
	}
	return t
}

// MainRed is what the runner found on a red main, after one re-run of the failed jobs.
type MainRed struct {
	Slug string
	// PR and Item are the merge the runner watched: the likely culprit.
	PR   int
	Item int64
	// MergeSHA is the first red commit; LastGreen the commit before it, when its CI was green
	// ("" when the runner could not say).
	MergeSHA, LastGreen string
	Failing, URLs       []string
}

// MainRedAsk is the ask of a red main's P1. The runner's MainRedCommit reads the merge commit
// back from its second paragraph, to attach the failing CI log when it triages the P1.
func MainRedAsk(m MainRed) string {
	var b strings.Builder
	fmt.Fprintf(&b, "main's CI failed on `%s`, the squash merge of %s#%d.\n\nCaused by item #%d / merge %s.\n\n",
		short7(m.MergeSHA), m.Slug, m.PR, m.Item, m.MergeSHA)
	fmt.Fprintf(&b, "Failing (still red after one re-run of the failed jobs): %s.\n\n", strings.Join(m.Failing, ", "))
	fmt.Fprintf(&b, "First red commit: %s. ", m.MergeSHA)
	if m.LastGreen != "" {
		fmt.Fprintf(&b, "Last green: %s.\n\n", m.LastGreen)
	} else {
		b.WriteString("Last green: unknown (main's CI before the merge was not green or could not be read).\n\n")
	}
	fmt.Fprintf(&b, "Likely culprit: %s#%d (item #%d), the merge between them. Fix main; if the fix is not obvious, revert that PR.\n\n", m.Slug, m.PR, m.Item)
	fmt.Fprintf(&b, "%s is frozen until main is green: nothing else in it moves stages; this item goes first.\n\n", m.Slug)
	b.WriteString("Runs:\n- " + strings.Join(m.URLs, "\n- ") + "\n")
	return b.String()
}

// MainRedNotes are the notes of the rebuild an item gets when its PR's CI failed while main was
// red: once main is green, its CI is checked again on a rebased head.
const MainRedNotes = "main was red; rebase and re-run"

// ApplyMainRedRebuild sends an item whose PR's CI failed while main was red back to build with
// MainRedNotes, from build (land sent it back) or from needs_you (the red CI was its second
// strike). It is not a bounce.
func ApplyMainRedRebuild(it Item) (Transition, error) {
	if it.Stage != Build && !(it.Stage == NeedsYou && Rebuildable(it)) {
		return Transition{}, errf(CodeWrongStage, "item is %s, not build or a rebuildable needs_you", it.Stage)
	}
	data := map[string]any{"notes": MainRedNotes, "main_red": true}
	if it.Stage == NeedsYou {
		data["question"] = it.Question
	}
	if it.Approval != nil {
		it.Approval = nil
		data["approval_voided"] = true
	}
	from := it.Stage
	it.Stage, it.ReturnStage, it.Question = Build, "", ""
	return Transition{Item: it, From: from, To: Build, Kind: "rebuild", Data: data}, nil
}

func short7(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
