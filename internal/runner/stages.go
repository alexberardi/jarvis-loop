package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// MergedPRsForTriage is how many recently merged PRs of a repo triage sees for dedupe.
const MergedPRsForTriage = 30

// MaxDiffBytes caps the diff a review run is given; a bigger diff is cut, and the brief's file says
// so (the agent can run git diff itself).
const MaxDiffBytes = 4 << 20

// writeContext writes a context file (relative to the working directory) and lists it for the
// brief. It reports whether the file was written.
func writeContext(ru *run, info *RunInfo, rel, label, content string) bool {
	p := filepath.Join(ru.wt, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		info.ContextErrs = append(info.ContextErrs, rel+": "+err.Error())
		return false
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		info.ContextErrs = append(info.ContextErrs, rel+": "+err.Error())
		return false
	}
	info.Context = append(info.Context, TriageFile{Name: rel, Label: label})
	return true
}

// mainRedLog gives triage of a red main's P1 the failing CI log, as .loop/context/ci-failure.log:
// the one already attached to the item, or (first triage) fetched from GitHub and attached now,
// under triage's lease (the runner held none on the P1 when it filed it).
func (r *Runner) mainRedLog(ctx context.Context, ru *run, info *RunInfo, sha string) {
	rel := filepath.Join(".loop", "context", CIFailureLog)
	dst := filepath.Join(ru.wt, rel)
	fail := func(err error) { info.ContextErrs = append(info.ContextErrs, "the failing CI log: "+err.Error()) }
	evs, err := r.Client.Evidence(ctx, ru.item.ID)
	if err != nil {
		fail(err)
		return
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if e := evs[i]; e.Name == CIFailureLog {
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				fail(err)
			} else if err := r.Client.DownloadEvidence(ctx, ru.item.ID, e.N, dst); err != nil {
				fail(err)
			} else {
				info.CILog = &TriageFile{Name: rel, Label: e.Label}
			}
			return
		}
	}
	if r.Land == nil {
		fail(fmt.Errorf("the runner has no GitHub client"))
		return
	}
	runs, err := r.Land.CommitRuns(ctx, ru.repo.Slug, sha)
	if err != nil {
		fail(err)
		return
	}
	label, err := r.attachCIFailure(ctx, ru.item.ID, ru.repo.Slug, sha, failedRuns(runs), dst)
	if err != nil {
		fail(err)
		return
	}
	info.CILog = &TriageFile{Name: rel, Label: label}
}

// RecentTitlesForRouting is how many recently merged PR titles per repo a routing run sees, and
// RoutingFetchTimeout how long the runner waits for them (all repos at once) before it notes the
// ones it could not get.
const (
	RecentTitlesForRouting = 5
	RoutingFetchTimeout    = 20 * time.Second
	routingFetchParallel   = 6
)

// prepareTriage makes triage's working directory and context: the configured repos, the open
// items and the item's evidence. An item with a repo gets that repo checked out detached at its
// base under repos/<key>/ and its recently merged PRs. An item with none is routed first
// (docs/phases/06b-all-repos.md): no checkouts, and repos.md carries each repo's group, aliases,
// description and last merged PR titles, so the agent can name the repo from that file alone.
func (r *Runner) prepareTriage(ctx context.Context, ru *run, info *RunInfo) error {
	os.RemoveAll(ru.wt)
	if err := os.MkdirAll(ru.wt, 0o755); err != nil {
		return err
	}
	if ru.repoKey != "" {
		k := ru.repoKey
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
		ru.repoDir = filepath.Join(ru.wt, dir)
	}

	if sha := MainRedCommit(ru.item, ru.repo.Slug); sha != "" && ru.repoKey != "" {
		r.mainRedLog(ctx, ru, info, sha)
	}
	r.answersContext(ctx, ru, info, filepath.Join(".loop", "context", "answers.md"))
	r.planContext(ctx, ru, info)

	all := make([]string, 0, len(r.Cfg.Repos))
	for k := range r.Cfg.Repos {
		all = append(all, k)
	}
	sort.Strings(all)
	var repos strings.Builder
	if ru.repoKey != "" {
		repos.WriteString("# The repos the loop works on\n\n| key | slug | group | base | what it is |\n| --- | --- | --- | --- | --- |\n")
		for _, k := range all {
			rc := r.Cfg.Repos[k]
			fmt.Fprintf(&repos, "| %s | %s | %s | %s | %s |\n", k, rc.Slug, cell(rc.Group), rc.Base, cell(rc.Description))
		}
		writeContext(ru, info, ".loop/context/repos.md", "the configured repos", repos.String())
	} else {
		info.Routing = true
		titles, errs := r.recentTitles(ctx, all)
		fmt.Fprintf(&repos, "# The repos the loop works on\n\nPick the one this item belongs to. \"Recent\" are the last %d merged PR titles.\n\n", RecentTitlesForRouting)
		repos.WriteString("| key | slug | group | also called | what it is | recent |\n| --- | --- | --- | --- | --- | --- |\n")
		for _, k := range all {
			rc := r.Cfg.Repos[k]
			recent := strings.Join(titles[k], "; ")
			if e := errs[k]; e != "" {
				recent = "(could not fetch: " + e + ")"
			} else if recent == "" {
				recent = "(none)"
			}
			fmt.Fprintf(&repos, "| %s | %s | %s | %s | %s | %s |\n", k, rc.Slug, cell(rc.Group), cell(strings.Join(rc.Aliases, ", ")),
				cell(rc.Description), cell(recent))
			if e := errs[k]; e != "" {
				info.ContextErrs = append(info.ContextErrs, "recent PR titles of "+rc.Slug+": "+e)
			}
		}
		writeContext(ru, info, ".loop/context/repos.md", "the configured repos: group, aliases, what each is, recent PR titles", repos.String())
	}

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

// recentTitles fetches each repo's last merged PR titles, a few at a time, within
// RoutingFetchTimeout. A repo whose titles could not be fetched has an error instead.
func (r *Runner) recentTitles(ctx context.Context, keys []string) (map[string][]string, map[string]string) {
	ctx, cancel := context.WithTimeout(ctx, RoutingFetchTimeout)
	defer cancel()
	titles, errs := map[string][]string{}, map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, routingFetchParallel)
	for _, k := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				errs[k] = "timed out"
				mu.Unlock()
				return
			}
			prs, err := r.PRs.MergedPRs(ctx, r.Cfg.Repos[k].Slug, RecentTitlesForRouting)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if ctx.Err() != nil {
					errs[k] = "timed out"
				} else {
					errs[k] = clip(oneLine(err.Error()), 120)
				}
				return
			}
			for i, p := range prs {
				if i == RecentTitlesForRouting {
					break
				}
				titles[k] = append(titles[k], fmt.Sprintf("#%d %s", p.Number, oneLine(p.Title)))
			}
		}()
	}
	wg.Wait()
	return titles, errs
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
	r.answersContext(ctx, ru, info, filepath.Join(".loop", "context", "answers.md"))
	r.planContext(ctx, ru, info)
	return nil
}

// answersContext writes to rel every question an agent asked on the item and the user's answer,
// so the next run acts on them instead of asking again (the answer is recorded on loopd, but a run
// starts fresh).
func (r *Runner) answersContext(ctx context.Context, ru *run, info *RunInfo, rel string) {
	d, err := r.Client.Detail(ctx, ru.item.ID)
	if err != nil {
		info.ContextErrs = append(info.ContextErrs, "the earlier answers from loopd: "+err.Error())
		return
	}
	var b strings.Builder
	n := 0
	for _, e := range d.Events {
		if e.Kind != "answered" {
			continue
		}
		n++
		q, _ := e.Data["question"].(string)
		a, _ := e.Data["answer"].(string)
		fmt.Fprintf(&b, "\n## Answer %d (%s, %s stage)\n\nQuestion:\n\n> %s\n\nAnswer:\n\n> %s\n", n, e.At.UTC().Format("2006-01-02 15:04 UTC"), e.To,
			strings.ReplaceAll(strings.TrimSpace(q), "\n", "\n> "), strings.ReplaceAll(strings.TrimSpace(a), "\n", "\n> "))
	}
	if n == 0 {
		return
	}
	writeContext(ru, info, rel, "earlier questions on this item and the user's answers, newest last", "# Earlier questions and answers (from loopd)\n"+b.String())
	info.Answers = rel
}

// planContext gives an item filed from a plan the plan's approved brief and blueprint, the
// versions the user approved (not a later draft), in .loop/context/: its piece is in the ask, but
// pieces point at other sections of the blueprint (a table, the rules, the names).
func (r *Runner) planContext(ctx context.Context, ru *run, info *RunInfo) {
	it := ru.item
	if it.PlanID <= 0 {
		return
	}
	p, err := r.Client.Plan(ctx, it.PlanID)
	if err != nil {
		info.ContextErrs = append(info.ContextErrs, fmt.Sprintf("plan P%d from loopd: %v", it.PlanID, err))
		return
	}
	info.PlanID, info.PlanPiece = it.PlanID, it.PlanPiece
	for _, x := range []struct {
		kind, what string
		v          int
	}{{pipeline.DocBlueprint, "blueprint", p.BlueprintApproved}, {pipeline.DocBrief, "product brief", p.BriefApproved}} {
		if x.v <= 0 {
			continue
		}
		d, err := r.Client.PlanDocVersion(ctx, it.PlanID, x.kind, x.v)
		switch {
		case err != nil:
			info.ContextErrs = append(info.ContextErrs, fmt.Sprintf("plan P%d's approved %s (v%d): %v", it.PlanID, x.what, x.v, err))
		case d == nil:
			info.ContextErrs = append(info.ContextErrs, fmt.Sprintf("plan P%d's approved %s (v%d): not found", it.PlanID, x.what, x.v))
		case pipeline.DocSHA256(d.Markdown) != d.SHA256:
			info.ContextErrs = append(info.ContextErrs, fmt.Sprintf("plan P%d's approved %s (v%d): its text does not match the approved sha256", it.PlanID, x.what, x.v))
		default:
			rel := ".loop/context/" + x.kind + ".md"
			if writeContext(ru, info, rel, fmt.Sprintf("plan P%d's approved %s, v%d, sha256 %s", it.PlanID, x.what, d.Version, short7(d.SHA256)), d.Markdown) {
				info.PlanDocs = append(info.PlanDocs, rel)
			}
		}
	}
}

// heldNotes writes to rel why a held item came back to build (pipeline.ApplyRebuild): the user's
// reply in the held thread, or the conflict the runner found, since the build's last PR update.
func (r *Runner) heldNotes(ctx context.Context, ru *run, info *RunInfo, rel string) {
	d, err := r.Client.Detail(ctx, ru.item.ID)
	if err != nil {
		info.ContextErrs = append(info.ContextErrs, "the held notes from loopd: "+err.Error())
		return
	}
	var notes []string
	for _, e := range d.Events {
		switch {
		case e.Outcome == "pr_opened":
			notes = nil // addressed by that build
		case e.Kind == "rebuild":
			by := "the user"
			if strings.HasPrefix(e.Actor, "runner:") {
				by = "the runner (land's held watch)"
			}
			n, _ := e.Data["notes"].(string)
			notes = append(notes, fmt.Sprintf("\n## Sent back by %s (%s)\n\n%s\n", by, e.At.UTC().Format("2006-01-02 15:04 UTC"), strings.TrimSpace(n)))
		}
	}
	if len(notes) == 0 {
		return
	}
	writeContext(ru, info, rel, "why the held PR came back to build, newest last", "# Sent back from held (from loopd)\n"+strings.Join(notes, ""))
	info.HeldNotes = rel
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
		if commentsRel != "" {
			// A build after the runner's verify failed: what failed, with the end of its output.
			var v strings.Builder
			v.WriteString("# The runner's verify (failed; nothing was pushed)\n")
			n := 0
			for _, e := range d.Events {
				if e.Outcome != "verify_failed" {
					continue
				}
				n++
				reason, _ := e.Data["reason"].(string)
				head, _ := e.Data["head_sha"].(string)
				logTail, _ := e.Data["log_tail"].(string)
				fmt.Fprintf(&v, "\n## Failure %d (%s, on %s)\n\n%s. The end of its output:\n\n```\n%s\n```\n", n, e.At.UTC().Format("2006-01-02 15:04 UTC"),
					short7(head), strings.TrimSpace(reason), strings.TrimRight(logTail, "\n"))
			}
			if n > 0 {
				v.WriteString("\nThe full logs and anything the steps wrote are the item's evidence (`verify-<n>-…` files in `.loop/triage/`).\n")
				writeContext(ru, info, filepath.Join(filepath.Dir(notesRel), "verify.md"), "the runner's verify output, newest last", v.String())
			}
		}
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
		r.reject(ctx, ru, out, err.Error())
		return
	}
	switch v.Outcome {
	case "question":
		r.advance(ctx, ru, "question", r.metrics(ru, out, map[string]string{"question": clip(v.Question, 2000)}), nil)
		return
	case OutcomeRouted:
		// The repo is set and the item stays in triage; the next run checks out that repo.
		r.advance(ctx, ru, OutcomeRouted, r.metrics(ru, out, map[string]string{"repo": v.Repo, "why": clip(v.Why, MaxWhy)}), nil)
		return
	}
	files, err := r.evidence(ru, v)
	if err != nil {
		r.reject(ctx, ru, out, "evidence: "+err.Error())
		return
	}
	if !r.uploadNotesAndEvidence(ctx, ru, out, v, files, "triage-notes.md", "triage notes") {
		return
	}
	fields := map[string]string{"summary": clip(v.Summary, 2000)}
	if v.Outcome == "reproduced" {
		fields["area"], fields["size"], fields["risk"] = v.Area, v.Size, v.Risk
		fields["resources"] = strings.Join(v.Resources, ",")
		if v.Repo != "" {
			fields["repo"] = v.Repo
		}
		if v.NeedsQA {
			fields["needs_qa"] = "true"
		}
	}
	r.advance(ctx, ru, v.Outcome, r.metrics(ru, out, fields), nil)
}

// uploadNotesAndEvidence uploads the agent's notes (written by the runner into the run dir, as
// name) and its evidence files. It reports failure itself.
func (r *Runner) uploadNotesAndEvidence(ctx context.Context, ru *run, out outcome, v Result, files []EvidenceFile, name, label string) bool {
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
		r.reject(ctx, ru, out, err.Error())
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
	files, err := r.evidence(ru, v)
	if err != nil {
		r.reject(ctx, ru, out, "evidence: "+err.Error())
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
	if !r.uploadNotesAndEvidence(ctx, ru, out, ev, files, "", "") {
		return
	}
	fields := map[string]string{"pr_comment": "posted"}
	if v.Outcome == "approved" {
		fields["summary"], fields["head_sha"] = clip(v.Summary, 2000), it.HeadSHA
		if v.NeedsReview {
			fields["needs_review"] = "true"
		}
		if v.NeedsQA {
			fields["needs_qa"] = "true"
		}
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
	if v.Outcome == "approved" && v.NeedsReview {
		b.WriteString("\n**Marked needs review:** the merge waits for the user's approval.\n")
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
