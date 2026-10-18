package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The failing CI log (docs/decisions/2026-10-16-ci-failure-log.md): agents hold no GitHub
// credentials, so when CI fails the runner fetches the failed jobs' logs itself (gh run view
// --log-failed), cuts them down to the failures and attaches them to the item as ci-failure.log.

// CIFailureLog is the evidence file's name.
const CIFailureLog = "ci-failure.log"

// Limits on the excerpt: lines kept before and after each failure, lines when no failure is
// recognized (the log's tail), and the whole file.
const (
	ciBefore   = 100
	ciAfter    = 100
	ciTail     = 200
	ciMaxBytes = 256 << 10
)

// CIRun is a failed Actions run to fetch the log of: its id and what failed in it (check or
// workflow names).
type CIRun struct {
	ID   int64
	Name string
}

// CIFailure is a failed run's log, cut down for an agent: the failing tests it names, and the
// lines around each failure (or the tail), grouped by job.
type CIFailure struct {
	Tests []string
	Text  string
}

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-Z\\-_]`)
	// gh's --log-failed lines are "job<TAB>step<TAB>timestamp text".
	ghStampRe = regexp.MustCompile(`^\d{4}-\d\d-\d\dT[0-9:.]+Z ?`)
	// A Go test's failure, a scenario's in a transcript, and loop-sandbox run's progress line.
	goFailRe       = regexp.MustCompile(`^\s*--- FAIL: (\S+)`)
	scenarioFailRe = regexp.MustCompile(`Scenario (\S+) \(FAIL\b`)
	sandboxFailRe  = regexp.MustCompile(`^FAIL +(\S+) +[0-9.]+s$`)
	runIDRe        = regexp.MustCompile(`/actions/runs/(\d+)`)
)

// Token-like strings: GitHub, Slack and Anthropic tokens, bearer credentials, key=value secrets,
// and long opaque runs (64+ hex digits; 32+ base64 characters mixing cases and digits). A
// 40-digit commit sha stays.
var (
	tokenRes = []*regexp.Regexp{
		regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,}|xox[abeoprs]-[A-Za-z0-9-]{8,}|xapp-[A-Za-z0-9-]{8,}|sk-[A-Za-z0-9_-]{16,})`),
		regexp.MustCompile(`\b[0-9a-fA-F]{64,}\b`),
	}
	bearerRe = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	secretRe = regexp.MustCompile(`(?i)\b([A-Za-z_]*(token|secret|password|passwd|api_?key)[A-Za-z_]*)(\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s"'(),;\[\]{}]+)`)
	opaqueRe = regexp.MustCompile(`[A-Za-z0-9+_=-]{32,}`)
)

const redacted = "[redacted]"

// RedactTokens replaces anything token-like in s.
func RedactTokens(s string) string {
	for _, re := range tokenRes {
		s = re.ReplaceAllString(s, redacted)
	}
	s = bearerRe.ReplaceAllString(s, "$1 "+redacted)
	s = secretRe.ReplaceAllString(s, "$1$3"+redacted)
	return opaqueRe.ReplaceAllStringFunc(s, func(m string) string {
		if strings.ContainsAny(m, "0123456789") && strings.ToLower(m) != m && strings.ToUpper(m) != m {
			return redacted
		}
		return m
	})
}

// ExtractCIFailure cuts a failed run's log (gh run view --log-failed) down to the failures: ANSI
// stripped, gh's job, step and timestamp prefix dropped, grouped by job; for each job the lines
// around each failing Go test (--- FAIL) or scenario, or its tail when none is recognized; tokens
// redacted; capped in size.
func ExtractCIFailure(raw string) CIFailure {
	var jobs []string
	lines := map[string][]string{}
	for _, l := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		l = ansiRe.ReplaceAllString(l, "")
		job := ""
		if parts := strings.SplitN(l, "\t", 3); len(parts) == 3 && ghStampRe.MatchString(parts[2]) {
			job, l = parts[0], ghStampRe.ReplaceAllString(parts[2], "")
		}
		if _, ok := lines[job]; !ok {
			jobs = append(jobs, job)
		}
		lines[job] = append(lines[job], l)
	}
	var out CIFailure
	seen := map[string]bool{}
	var b strings.Builder
	for _, job := range jobs {
		ls := lines[job]
		for len(ls) > 0 && strings.TrimSpace(ls[len(ls)-1]) == "" {
			ls = ls[:len(ls)-1]
		}
		if len(ls) == 0 {
			continue
		}
		var hits []int
		for i, l := range ls {
			name := ""
			if m := goFailRe.FindStringSubmatch(l); m != nil {
				name = m[1]
			} else if m := scenarioFailRe.FindStringSubmatch(l); m != nil {
				name = m[1]
			} else if m := sandboxFailRe.FindStringSubmatch(l); m != nil {
				name = m[1]
			}
			if name == "" {
				continue
			}
			hits = append(hits, i)
			if !seen[name] {
				seen[name] = true
				out.Tests = append(out.Tests, name)
			}
		}
		if job != "" {
			fmt.Fprintf(&b, "==== job: %s ====\n", job)
		}
		if len(hits) == 0 {
			from := max(0, len(ls)-ciTail)
			if from > 0 {
				fmt.Fprintf(&b, "(no failing test recognized; the last %d of %d lines)\n", len(ls)-from, len(ls))
			}
			writeLines(&b, ls[from:])
			continue
		}
		end := -1 // the end of the last window written
		for _, h := range hits {
			from, to := max(0, h-ciBefore), min(len(ls), h+ciAfter)
			if from < end {
				from = end
			} else if end >= 0 || from > 0 {
				b.WriteString("...\n")
			}
			writeLines(&b, ls[from:to])
			end = max(end, to)
		}
		if end < len(ls) {
			b.WriteString("...\n")
		}
	}
	out.Text = RedactTokens(b.String())
	if len(out.Text) > ciMaxBytes {
		out.Text = out.Text[:ciMaxBytes] + "\n... (cut at " + strconv.Itoa(ciMaxBytes>>10) + " KB)\n"
	}
	return out
}

func writeLines(b *strings.Builder, ls []string) {
	for _, l := range ls {
		b.WriteString(l)
		b.WriteByte('\n')
	}
}

// RunIDFromURL is the Actions run id in a check's or run's URL (…/actions/runs/<id>[/job/<j>]),
// or 0.
func RunIDFromURL(u string) int64 {
	m := runIDRe.FindStringSubmatch(u)
	if m == nil {
		return 0
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	return id
}

// ciRunsFromChecks are the Actions runs behind a PR's failed checks, once each (a workflow's
// jobs share its run), with the failed checks' names.
func ciRunsFromChecks(checks []Check) []CIRun {
	var runs []CIRun
	at := map[int64]int{}
	for _, c := range checks {
		switch c.Bucket {
		case BucketPass, BucketSkipping, BucketPending:
			continue
		}
		id := RunIDFromURL(c.URL)
		if id <= 0 {
			continue
		}
		if i, ok := at[id]; ok {
			runs[i].Name += ", " + c.Name
			continue
		}
		at[id] = len(runs)
		runs = append(runs, CIRun{ID: id, Name: c.Name})
	}
	return runs
}

// failedRuns are main's failed runs (MainResult's red ones) that have an id.
func failedRuns(runs []WorkflowRun) []CIRun {
	var out []CIRun
	for _, r := range runs {
		switch r.Conclusion {
		case "success", "neutral", "skipped", "cancelled", "":
			continue
		}
		if r.ID <= 0 {
			continue
		}
		name := r.Name
		if name == "" {
			name = r.Workflow
		}
		out = append(out, CIRun{ID: r.ID, Name: name})
	}
	return out
}

// MaxCIRuns is how many failed runs' logs the runner fetches for one failure.
const MaxCIRuns = 5

// ciFailureFile fetches the failed jobs' logs of runs and cuts them down into the text of
// ci-failure.log, with its label ("CI failure on <sha7>: <names>"). It fails only when no log
// could be fetched; a run whose log could not be read says so in the file.
func (r *Runner) ciFailureFile(ctx context.Context, slug, sha string, runs []CIRun) (text, label string, err error) {
	if r.Land == nil {
		return "", "", fmt.Errorf("the runner has no GitHub client")
	}
	if len(runs) == 0 {
		return "", "", fmt.Errorf("no failed run with an id")
	}
	if len(runs) > MaxCIRuns {
		runs = runs[:MaxCIRuns]
	}
	var names, tests []string
	var body strings.Builder
	fetched := 0
	var lastErr error
	for _, run := range runs {
		names = append(names, run.Name)
		fmt.Fprintf(&body, "\n######## %s (run %d)\n", run.Name, run.ID)
		raw, err := r.Land.FailedLog(ctx, slug, run.ID)
		if err != nil {
			lastErr = err
			fmt.Fprintf(&body, "(the runner could not fetch this run's log: %s)\n", clip(RedactTokens(err.Error()), 300))
			continue
		}
		fetched++
		f := ExtractCIFailure(raw)
		tests = append(tests, f.Tests...)
		body.WriteString(f.Text)
	}
	if fetched == 0 {
		return "", "", fmt.Errorf("gh run view --log-failed: %w", lastErr)
	}
	label = clip(fmt.Sprintf("CI failure on %s: %s", short7(sha), strings.Join(names, ", ")), 200)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nCommit: %s (%s)\n", label, sha, slug)
	if len(tests) > 0 {
		fmt.Fprintf(&b, "Failing tests: %s\n", RedactTokens(strings.Join(tests, ", ")))
	} else {
		b.WriteString("Failing tests: none recognized (no Go --- FAIL or scenario FAIL line); each job's tail is below.\n")
	}
	b.WriteString("Cut from gh run view --log-failed: the lines around each failure, ANSI and token-like strings removed.\n")
	b.WriteString(body.String())
	text = b.String()
	if len(text) > ciMaxBytes+4096 {
		text = text[:ciMaxBytes] + "\n... (cut)\n"
	}
	return text, label, nil
}

// attachCIFailure fetches, cuts down and uploads the failing CI log as the item's
// ci-failure.log (the runner must hold the item's lease). It writes the file at path (a temp
// file when ""), and returns its label. Callers log a failure and go on: the log helps, it never
// blocks.
func (r *Runner) attachCIFailure(ctx context.Context, id int64, slug, sha string, runs []CIRun, path string) (string, error) {
	text, label, err := r.ciFailureFile(ctx, slug, sha, runs)
	if err != nil {
		return "", err
	}
	if path == "" {
		f, err := os.CreateTemp("", "ci-failure-*.log")
		if err != nil {
			return "", err
		}
		path = f.Name()
		f.Close()
		defer os.Remove(path)
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return "", err
	}
	if err := r.Client.Upload(ctx, id, path, CIFailureLog, label); err != nil {
		return "", fmt.Errorf("uploading %s: %w", CIFailureLog, err)
	}
	return label, nil
}

// mainRedRe reads a main-red P1's ask (watchMain writes it): the repo and the merge commit.
var mainRedRe = regexp.MustCompile("(?m)^main's CI failed on `[0-9a-f]{7}`, the squash merge of (\\S+)#\\d+\\.\\s+Caused by item #\\d+ / merge ([0-9a-f]{40})\\.$")

// MainRedCommit is the merge commit a main-red P1 (filed by the runner: source finding) is about,
// when it is one for slug; "" otherwise.
func MainRedCommit(it Item, slug string) string {
	if it.Source != "finding" || !strings.HasPrefix(it.Title, "main red in ") && !strings.HasPrefix(it.Title, "main red after #") {
		return ""
	}
	m := mainRedRe.FindStringSubmatch(it.Ask)
	if m == nil || m[1] != slug {
		return ""
	}
	return m[2]
}
