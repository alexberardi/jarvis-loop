package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// Result is .loop/result.json, what the agent says it did. Nothing in it is trusted for the
// pipeline's rules: the runner reports git's head and paths, pushes the branch and opens the PR
// itself (docs/decisions/2026-10-15-runner-pushes.md).
type Result struct {
	Outcome string `json:"outcome"`
	// PRTitle and PRBody are what the runner opens (or updates) the PR with.
	PRTitle string `json:"pr_title,omitempty"`
	PRBody  string `json:"pr_body,omitempty"`
	// HeadSHA is optional: when given, it must be HEAD (what the agent meant to hand over).
	HeadSHA     string     `json:"head_sha,omitempty"`
	Summary     string     `json:"summary,omitempty"`
	NotVerified []string   `json:"not_verified,omitempty"`
	Evidence    []Evidence `json:"evidence,omitempty"`
	NeedsReview bool       `json:"needs_review,omitempty"`
	Question    string     `json:"question,omitempty"`
}

// Evidence names a file under .loop/evidence and what it shows.
type Evidence struct {
	File  string `json:"file"`
	Label string `json:"label"`
}

// GitFacts are what the runner computed itself.
type GitFacts struct {
	HeadSHA string
	// RemoteHead is the item branch's head on the remote ("" when it is not there).
	RemoteHead string
	// ChangedPaths are `git diff --name-status base...HEAD` lines.
	ChangedPaths []string
	// Dirty are `git status --porcelain` lines: changes in the worktree that are not committed.
	Dirty []string
}

// Expect is what the run allows.
type Expect struct {
	Stage string
}

// Valid is a result the runner will act on, with git's values.
type Valid struct {
	Result
	Paths []string
}

// OutcomeReadyForPR is the build agent's "done": committed on the item's branch, for the runner
// to push and open the PR. The runner then reports loopd's pr_opened.
const OutcomeReadyForPR = "ready_for_pr"

// StageOutcomes are the outcomes an agent may return per stage (the runner itself reports
// pr_opened, failed and usage_limit).
var StageOutcomes = map[string][]string{"build": {OutcomeReadyForPR, "question"}}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Limits on evidence (loopd's per-file cap, and a count) and on the PR text (GitHub's are 256
// and 65,536 characters; the runner adds a footer to the body).
const (
	MaxEvidenceFiles = 20
	MaxEvidenceBytes = 20 << 20
	MaxPRTitle       = 256
	MaxPRBody        = 60000
)

// ValidateResult parses and checks a result against the stage and git. loopDir is the worktree's
// .loop directory.
func ValidateResult(raw []byte, want Expect, facts GitFacts, loopDir string) (Valid, error) {
	var r Result
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Valid{}, fmt.Errorf("result.json is not valid JSON for the contract: %v", err)
	}
	if !slices.Contains(StageOutcomes[want.Stage], r.Outcome) {
		return Valid{}, fmt.Errorf("outcome %q is not an outcome of %s (want one of %s)", r.Outcome, want.Stage, strings.Join(StageOutcomes[want.Stage], ", "))
	}
	if r.Outcome == "question" {
		if strings.TrimSpace(r.Question) == "" {
			return Valid{}, fmt.Errorf("outcome question but the question is empty")
		}
		return Valid{Result: r}, nil
	}
	title := strings.TrimSpace(r.PRTitle)
	switch {
	case title == "" || strings.ContainsAny(title, "\r\n") || utf8.RuneCountInString(title) > MaxPRTitle:
		return Valid{}, fmt.Errorf("pr_title must be one line of 1 to %d characters", MaxPRTitle)
	case strings.TrimSpace(r.PRBody) == "" || utf8.RuneCountInString(r.PRBody) > MaxPRBody:
		return Valid{}, fmt.Errorf("pr_body must be markdown of 1 to %d characters", MaxPRBody)
	case strings.TrimSpace(r.Summary) == "":
		return Valid{}, fmt.Errorf("summary is required")
	case r.HeadSHA != "" && (!shaRe.MatchString(r.HeadSHA) || r.HeadSHA != facts.HeadSHA):
		return Valid{}, fmt.Errorf("head_sha %q disagrees with git (HEAD is %s)", r.HeadSHA, facts.HeadSHA)
	case len(facts.Dirty) > 0:
		return Valid{}, fmt.Errorf("the worktree has uncommitted changes (%s); ready_for_pr means everything is committed, and the runner pushes only commits",
			clip(strings.Join(facts.Dirty, ", "), 400))
	case len(facts.ChangedPaths) == 0:
		return Valid{}, fmt.Errorf("no commits change anything against the base branch")
	}
	r.PRTitle = title
	r.HeadSHA = facts.HeadSHA
	for _, p := range normPaths(facts.ChangedPaths) {
		if strings.Contains(p, "\t.loop/") {
			return Valid{}, fmt.Errorf("the commits contain the runner's .loop/ files (%s)", p)
		}
	}
	files, err := EvidenceFiles(loopDir)
	if err != nil {
		return Valid{}, err
	}
	if len(files) == 0 {
		return Valid{}, fmt.Errorf("ready_for_pr needs at least one evidence file in .loop/evidence")
	}
	for _, e := range r.Evidence {
		c := path.Clean(e.File)
		if !strings.HasPrefix(c, "evidence/") || strings.Count(c, "/") != 1 {
			return Valid{}, fmt.Errorf("evidence entry %q must name a file directly in evidence/", e.File)
		}
	}
	return Valid{Result: r, Paths: facts.ChangedPaths}, nil
}

// normPaths turns name-status lines ("M\tx", "M x", "R100\ta\tb") into a sorted canonical form.
func normPaths(lines []string) []string {
	var out []string
	for _, l := range lines {
		var parts []string
		if strings.Contains(l, "\t") {
			parts = strings.Split(strings.TrimSpace(l), "\t")
		} else {
			parts = strings.Fields(l)
		}
		if len(parts) < 2 || parts[0] == "" {
			out = append(out, "?"+l)
			continue
		}
		parts[0] = strings.ToUpper(parts[0][:1])
		out = append(out, strings.Join(parts, "\t"))
	}
	sort.Strings(out)
	return out
}

// EvidenceFile is one file to upload.
type EvidenceFile struct {
	Name string
	Path string
	Size int64
}

// EvidenceFiles lists .loop/evidence: regular files only (a symlink could point at a secret),
// at most MaxEvidenceFiles, each at most MaxEvidenceBytes.
func EvidenceFiles(loopDir string) ([]EvidenceFile, error) {
	dir := filepath.Join(loopDir, "evidence")
	st, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf(".loop/evidence is not a directory")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	if len(ents) > MaxEvidenceFiles {
		return nil, fmt.Errorf(".loop/evidence has %d entries; at most %d", len(ents), MaxEvidenceFiles)
	}
	var out []EvidenceFile
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("evidence %s is not a regular file", e.Name())
		}
		if fi.Size() > MaxEvidenceBytes {
			return nil, fmt.Errorf("evidence %s is %d bytes; at most 20 MB", e.Name(), fi.Size())
		}
		out = append(out, EvidenceFile{Name: e.Name(), Path: p, Size: fi.Size()})
	}
	return out, nil
}
