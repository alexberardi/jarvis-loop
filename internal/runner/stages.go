package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// MergedPRsForTriage is how many recently merged PRs of a repo triage sees for dedupe.
const MergedPRsForTriage = 30

// MaxDiffBytes caps the diff a review run is given; a bigger diff is cut, and the brief's file says
// so (the agent can run git diff itself).
const MaxDiffBytes = 4 << 20

// writeContext writes a context file (relative to the working directory) and lists it for the
// brief.
func writeContext(ru *run, info *RunInfo, rel, label, content string) {
	p := filepath.Join(ru.wt, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		info.ContextErrs = append(info.ContextErrs, rel+": "+err.Error())
		return
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		info.ContextErrs = append(info.ContextErrs, rel+": "+err.Error())
		return
	}
	info.Context = append(info.Context, TriageFile{Name: rel, Label: label})
}

// prepareTriage makes triage's working directory: a plain directory with the item's repo (or,
// when it has none, every configured repo) checked out detached at the base branch under
// repos/<key>/, and the context: the configured repos, the open items, the recently merged PRs
// and the item's evidence.
func (r *Runner) prepareTriage(ctx context.Context, ru *run, info *RunInfo) error {
	os.RemoveAll(ru.wt)
	if err := os.MkdirAll(ru.wt, 0o755); err != nil {
		return err
	}
	keys := []string{ru.repoKey}
	if ru.repoKey == "" {
		keys = keys[:0]
		for k := range r.Cfg.Repos {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	for _, k := range keys {
		rc := r.Cfg.Repos[k]
		mirror := r.mirrorPath(k)
		dir := "repos/" + Slug(k)
		unlock := r.lockMirror(mirror)
		err := r.Git.EnsureMirror(ctx, mirror, rc)
		if err == nil {
			err = r.Git.AddDetached(ctx, mirror, filepath.Join(ru.wt, dir), "refs/remotes/origin/"+rc.Base)
		}
		unlock()
		if err != nil {
			return fmt.Errorf("checking out %s: %w", k, err)
		}
		c := Checkout{Key: k, Slug: rc.Slug, Base: rc.Base, Dir: dir}
		ru.checkouts = append(ru.checkouts, c)
		info.Checkouts = append(info.Checkouts, c)
	}
	if ru.repoKey != "" {
		ru.repoDir = filepath.Join(ru.wt, ru.checkouts[0].Dir)
	}

	var repos strings.Builder
	repos.WriteString("# The repos the loop works on\n\n| key | slug | base | what it is |\n| --- | --- | --- | --- |\n")
	all := make([]string, 0, len(r.Cfg.Repos))
	for k := range r.Cfg.Repos {
		all = append(all, k)
	}
	sort.Strings(all)
	for _, k := range all {
		rc := r.Cfg.Repos[k]
		fmt.Fprintf(&repos, "| %s | %s | %s | %s |\n", k, rc.Slug, rc.Base, cell(rc.Description))
	}
	writeContext(ru, info, ".loop/context/repos.md", "the configured repos", repos.String())

	if rows, err := r.Client.OpenItems(ctx); err != nil {
		info.ContextErrs = append(info.ContextErrs, "the open items: "+err.Error())
	} else {
		var b strings.Builder
		b.WriteString("# Open loop items\n\n| id | title | stage | repo | PR |\n| --- | --- | --- | --- | --- |\n")
		for _, it := range rows {
			title := cell(it.Title)
			if it.ID == ru.item.ID {
				title += " (this item)"
			}
			pr := ""
			if it.PR > 0 {
				pr = "#" + strconv.Itoa(it.PR)
			}
			fmt.Fprintf(&b, "| #%d | %s | %s | %s | %s |\n", it.ID, title, it.Display, cell(it.Repo), pr)
		}
		writeContext(ru, info, ".loop/context/open-items.md", "open loop items, for duplicates", b.String())
	}

	for _, c := range ru.checkouts {
		prs, err := r.PRs.MergedPRs(ctx, c.Slug, MergedPRsForTriage)
		if err != nil {
			info.ContextErrs = append(info.ContextErrs, "merged PRs of "+c.Slug+": "+err.Error())
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "# The last %d merged PRs of %s\n\n", MergedPRsForTriage, c.Slug)
		if len(prs) == 0 {
			b.WriteString("None.\n")
		}
		for _, p := range prs {
			files := p.Files
			more := ""
			if len(files) > 40 {
				files, more = files[:40], fmt.Sprintf(", and %d more", len(p.Files)-40)
			}
			fmt.Fprintf(&b, "- #%d %s (merged %s): %s%s\n", p.Number, oneLine(p.Title), p.MergedAt, strings.Join(files, ", "), more)
		}
		writeContext(ru, info, ".loop/context/merged-prs-"+Slug(c.Key)+".md", "recently merged PRs of "+c.Slug, b.String())
	}

	files, errText := r.fetchEvidence(ctx, ru, filepath.Join(".loop", "context", "evidence"))
	for _, f := range files {
		info.Context = append(info.Context, TriageFile{Name: ".loop/context/evidence/" + f.Name, Label: f.Label})
	}
	if errText != "" {
		info.ContextErrs = append(info.ContextErrs, "the item's evidence: "+errText)
	}
	return nil
}

// prepareReview checks out the PR's head (the commit loopd recorded at pr_opened), detached, and
// gives the agent the PR, the diff, the item's evidence and any earlier review notes.
func (r *Runner) prepareReview(ctx context.Context, ru *run, info *RunInfo) error {
	it := ru.item
	if it.PR <= 0 || !shaRe.MatchString(it.HeadSHA) {
		return fmt.Errorf("the item has no PR head to review (pr %d, head %q)", it.PR, it.HeadSHA)
	}
	unlock := r.lockMirror(ru.mirror)
	err := r.Git.EnsureMirror(ctx, ru.mirror, ru.repo)
	if err == nil {
		err = r.Git.AddDetached(ctx, ru.mirror, ru.wt, it.HeadSHA)
	}
	var diff string
	if err == nil {
		diff, err = r.Git.run(ctx, ru.wt, "diff", "refs/remotes/origin/"+ru.repo.Base+"..."+it.HeadSHA)
	}
	unlock()
	if err != nil {
		return err
	}
	ru.repoDir = ru.wt
	info.PR, info.HeadSHA = it.PR, it.HeadSHA
	if len(diff) > MaxDiffBytes {
		diff = diff[:MaxDiffBytes] + fmt.Sprintf("\n\n[cut at %d bytes: run `git diff origin/%s...HEAD` for the rest]\n", MaxDiffBytes, ru.repo.Base)
	}
	if v, err := r.PRs.View(ctx, ru.repo.Slug, it.PR); err != nil {
		info.ContextErrs = append(info.ContextErrs, fmt.Sprintf("PR #%d from GitHub: %v", it.PR, err))
	} else {
		var b strings.Builder
		fmt.Fprintf(&b, "# PR #%d: %s\n\n- Branch `%s` into `%s`, %s\n- Reviewed commit: `%s` (the head loopd recorded)\n",
			it.PR, oneLine(v.Title), v.HeadRefName, v.BaseRefName, strings.ToLower(v.State), it.HeadSHA)
		if v.HeadRefOid != "" && v.HeadRefOid != it.HeadSHA {
			fmt.Fprintf(&b, "- Note: the PR's head on GitHub is now `%s`; review `%s` anyway.\n", v.HeadRefOid, it.HeadSHA)
		}
		fmt.Fprintf(&b, "\n## Body\n\n%s\n", strings.TrimSpace(v.Body))
		writeContext(ru, info, ".loop/context/pr.md", "the PR: title, body, head", b.String())
	}
	writeContext(ru, info, ".loop/context/diff.patch", fmt.Sprintf("git diff origin/%s...%s", ru.repo.Base, short7(it.HeadSHA)), diff)
	files, errText := r.fetchEvidence(ctx, ru, filepath.Join(".loop", "context", "evidence"))
	for _, f := range files {
		info.Context = append(info.Context, TriageFile{Name: ".loop/context/evidence/" + f.Name, Label: f.Label})
	}
	if errText != "" {
		info.ContextErrs = append(info.ContextErrs, "the item's evidence: "+errText)
	}
	if it.BounceCount > 0 {
		r.reviewContext(ctx, ru, info, filepath.Join(".loop", "context", "previous-reviews.md"), "")
	}
	return nil
}

// reviewContext writes the review notes loopd recorded on the item's bounces to notesRel, and the
// PR's comments (fetched by the runner, which holds the GitHub credentials) to commentsRel when
// it is set.
func (r *Runner) reviewContext(ctx context.Context, ru *run, info *RunInfo, notesRel, commentsRel string) {
	d, err := r.Client.Detail(ctx, ru.item.ID)
	if err != nil {
		info.ContextErrs = append(info.ContextErrs, "the review notes from loopd: "+err.Error())
	} else {
		var b strings.Builder
		b.WriteString("# Review notes (from loopd)\n")
		n := 0
		for _, e := range d.Events {
			if e.Outcome != "bounce" {
				continue
			}
			n++
			notes, _ := e.Data["notes"].(string)
			fmt.Fprintf(&b, "\n## Bounce %d (%s)\n\n%s\n", n, e.At.UTC().Format("2006-01-02 15:04 UTC"), strings.TrimSpace(notes))
		}
		if n == 0 {
			b.WriteString("\nNone recorded.\n")
		}
		writeContext(ru, info, notesRel, "the review notes, newest last", b.String())
	}
	if commentsRel == "" {
		return
	}
	if ru.item.PR <= 0 {
		return
	}
	cs, err := r.PRs.Comments(ctx, ru.repo.Slug, ru.item.PR)
	if err != nil {
		info.ContextErrs = append(info.ContextErrs, fmt.Sprintf("PR #%d's comments: %v", ru.item.PR, err))
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Comments on PR #%d (oldest first)\n", ru.item.PR)
	if len(cs) == 0 {
		b.WriteString("\nNone.\n")
	}
	for _, c := range cs {
		where := ""
		if c.Path != "" {
			where = " on `" + c.Path
			if c.Line > 0 {
				where += ":" + strconv.Itoa(c.Line)
			}
			where += "`"
		}
		state := ""
		if c.State != "" {
			state = " (" + strings.ToLower(strings.ReplaceAll(c.State, "_", " ")) + ")"
		}
		fmt.Fprintf(&b, "\n## %s, %s%s%s\n\n%s\n", c.Author, c.At, where, state, strings.TrimSpace(c.Body))
	}
	writeContext(ru, info, commentsRel, fmt.Sprintf("PR #%d's review comments", ru.item.PR), b.String())
}

// reportTriage checks a triage result and reports it: the notes and evidence go to the item, and
// the outcome (with the repo triage picked, already checked against the configured repos).
func (r *Runner) reportTriage(ctx context.Context, ru *run, out outcome, raw []byte) {
	v, err := ValidateTriage(raw, TriageExpect{ItemRepo: ru.item.Repo, Resolve: func(name string) (string, bool) {
		_, rc, ok := r.Cfg.Repo(name)
		return rc.Slug, ok
	}})
	if err != nil {
		r.fail(ctx, ru, out, "result rejected: "+err.Error())
		return
	}
	if v.Outcome == "question" {
		r.advance(ctx, ru, "question", r.metrics(ru, out, map[string]string{"question": clip(v.Question, 2000)}), nil)
		return
	}
	if !r.uploadNotesAndEvidence(ctx, ru, out, v, "triage-notes.md", "triage notes") {
		return
	}
	fields := map[string]string{"summary": clip(v.Summary, 2000)}
	if v.Outcome == "reproduced" {
		fields["area"], fields["size"], fields["risk"] = v.Area, v.Size, v.Risk
		fields["resources"] = strings.Join(v.Resources, ",")
		if v.Repo != "" {
			fields["repo"] = v.Repo
		}
	}
	r.advance(ctx, ru, v.Outcome, r.metrics(ru, out, fields), nil)
}

// uploadNotesAndEvidence uploads the agent's notes (written by the runner into the run dir, as
// name) and its evidence files. It reports failure itself.
func (r *Runner) uploadNotesAndEvidence(ctx context.Context, ru *run, out outcome, v Result, name, label string) bool {
	files, err := EvidenceFiles(filepath.Join(ru.wt, ".loop"))
	if err != nil {
		r.fail(ctx, ru, out, "evidence: "+err.Error())
		return false
	}
	if strings.TrimSpace(v.Notes) != "" {
		p := filepath.Join(ru.dir, name)
		if err := os.WriteFile(p, []byte(strings.TrimSpace(v.Notes)+"\n"), 0o600); err != nil {
			r.fail(ctx, ru, out, "writing the notes: "+err.Error())
			return false
		}
		files = append([]EvidenceFile{{Name: name, Path: p}}, files...)
	}
	labels := evidenceLabels(v)
	labels[name] = label
	for _, f := range files {
		if err := r.Client.Upload(ctx, ru.item.ID, f.Path, f.Name, labels[f.Name]); err != nil {
			r.fail(ctx, ru, out, "uploading evidence "+f.Name+": "+err.Error())
			return false
		}
	}
	return true
}

// reportReview checks a review result, posts the review on the PR as a comment (never an
// approval) and reports approved or bounce for the head the run reviewed.
func (r *Runner) reportReview(ctx context.Context, ru *run, out outcome, raw []byte) {
	it := ru.item
	v, err := ValidateReview(raw, it.HeadSHA)
	if err != nil {
		r.fail(ctx, ru, out, "result rejected: "+err.Error())
		return
	}
	if r.VerifyPR != nil {
		info, err := r.VerifyPR(ctx, ru.repo.Slug, it.PR)
		if err != nil {
			r.fail(ctx, ru, out, fmt.Sprintf("could not check PR #%d on GitHub: %v", it.PR, err))
			return
		}
		if info.State != "OPEN" || info.HeadRefOid != it.HeadSHA {
			r.fail(ctx, ru, out, fmt.Sprintf("PR #%d is %s at %s, but the review is of %s; not posted", it.PR, info.State, info.HeadRefOid, it.HeadSHA))
			return
		}
	}
	files, err := EvidenceFiles(filepath.Join(ru.wt, ".loop"))
	if err != nil {
		r.fail(ctx, ru, out, "evidence: "+err.Error())
		return
	}
	body := filepath.Join(ru.dir, "review-comment.md")
	if err := os.WriteFile(body, []byte(ReviewComment(v, it, files, evidenceLabels(v), r.runStats(ru, out))), 0o600); err != nil {
		r.fail(ctx, ru, out, err.Error())
		return
	}
	if err := r.PRs.Comment(ctx, ru.repo.Slug, it.PR, body); err != nil {
		r.fail(ctx, ru, out, fmt.Sprintf("could not post the review on PR #%d: %v", it.PR, err))
		return
	}
	// A bounce's notes go to loopd as the outcome's field (and are in the comment), not as a file.
	ev := v
	ev.Notes = ""
	if !r.uploadNotesAndEvidence(ctx, ru, out, ev, "", "") {
		return
	}
	fields := map[string]string{"pr_comment": "posted"}
	if v.Outcome == "approved" {
		fields["summary"], fields["head_sha"] = clip(v.Summary, 2000), it.HeadSHA
		if len(v.NotVerified) > 0 {
			fields["not_verified"] = clip(strings.Join(v.NotVerified, "; "), 1000)
		}
	} else {
		fields["notes"] = clip(v.Notes, MaxNotes)
	}
	r.advance(ctx, ru, v.Outcome, r.metrics(ru, out, fields), nil)
}

// ReviewComment is the PR comment a review run posts: the verdict, the agent's review, and a
// footer with the reviewed commit, the evidence and the run's numbers.
func ReviewComment(v Result, it Item, evidence []EvidenceFile, labels map[string]string, st RunStats) string {
	var b strings.Builder
	verdict := "approved"
	if v.Outcome == "bounce" {
		verdict = "changes requested"
	}
	fmt.Fprintf(&b, "## Loop review: %s\n\n%s\n", verdict, strings.TrimSpace(v.Review))
	if v.Outcome == "bounce" && strings.TrimSpace(v.Notes) != strings.TrimSpace(v.Review) {
		fmt.Fprintf(&b, "\n### What to change\n\n%s\n", strings.TrimSpace(v.Notes))
	}
	if len(v.NotVerified) > 0 {
		b.WriteString("\n**Not verified:** " + oneLine(strings.Join(v.NotVerified, "; ")) + "\n")
	}
	fmt.Fprintf(&b, "\n---\n\nReviewed `%s` for loop item #%d by a jarvis-loop review agent, not the one that built it. This is a comment, not a GitHub approval.\n", it.HeadSHA, it.ID)
	if len(evidence) > 0 {
		fmt.Fprintf(&b, "\nEvidence files (on loop item #%d):\n\n", it.ID)
		for _, e := range evidence {
			if l := labels[e.Name]; l != "" {
				fmt.Fprintf(&b, "- `%s`: %s\n", e.Name, oneLine(l))
			} else {
				fmt.Fprintf(&b, "- `%s`\n", e.Name)
			}
		}
	}
	if st.HasResult {
		fmt.Fprintf(&b, "\nRun: %d turns, prompt-cache hit %.0f%%, %s, about $%.2f (estimate).\n", st.Turns, st.CacheHit*100, duration(st.Seconds), st.CostUSD)
	}
	return b.String()
}

// cell makes text safe for one Markdown table cell.
func cell(s string) string {
	s = oneLine(s)
	if s == "" {
		return "-"
	}
	return strings.ReplaceAll(s, "|", "\\|")
}
