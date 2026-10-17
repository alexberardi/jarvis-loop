package sandbox

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RunOptions configure a run of scenarios.
type RunOptions struct {
	// Out is the run's directory: one subdirectory per scenario with its stack, result.json,
	// slack.json, events.json, prs.json and transcript.md; results.md for the lot.
	Out string
	Bin Binaries
	// Log receives the stacks' logs (loopd, Slack layer, runner); nil discards them, but each
	// scenario's log is always written to its stack.log.
	Log io.Writer
}

// RunScenario plays one scenario on a fresh stack in dir and writes what it recorded.
func RunScenario(sc *Scenario, dir string, bin Binaries) Result {
	start := time.Now()
	res := Result{Name: sc.Name, Steps: len(sc.Steps), Dir: dir}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		res.Failures = []string{err.Error()}
		return res
	}
	logf, _ := os.Create(filepath.Join(dir, "stack.log"))
	if logf != nil {
		defer logf.Close()
	}
	var logw io.Writer = io.Discard
	if logf != nil {
		logw = logf
	}
	s, err := StartStack(Options{Dir: filepath.Join(dir, "stack"), Bin: bin, Log: logw, Stages: sc.Stages, AgentSandbox: sc.AgentSandbox})
	if err != nil {
		res.Failures = []string{"starting the stack: " + err.Error()}
		res.Seconds = time.Since(start).Seconds()
		return res
	}
	res = Play(sc, s)
	res.Dir = dir
	res.Seconds = time.Since(start).Seconds()
	writeRecord(dir, sc, s, res)
	s.Close()
	return res
}

func writeJSON(path string, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	os.WriteFile(path, append(b, '\n'), 0o644)
}

func writeRecord(dir string, sc *Scenario, s *Stack, res Result) {
	msgs := s.Slack.Messages()
	writeJSON(filepath.Join(dir, "slack.json"), msgs)
	writeJSON(filepath.Join(dir, "prs.json"), s.GitHub.PRs())
	writeJSON(filepath.Join(dir, "result.json"), res)
	if evs, err := s.Events(); err == nil {
		writeJSON(filepath.Join(dir, "events.json"), evs)
	}
	md := TranscriptMD(sc.Name, sc.About, res, Render(msgs, "")) + PRsMD(s.GitHub.PRs())
	os.WriteFile(filepath.Join(dir, "transcript.md"), []byte(md), 0o644)
}

// TranscriptMD is a scenario's transcript as Markdown, for a PR description.
func TranscriptMD(name, about string, res Result, rendered string) string {
	var b strings.Builder
	status := "pass"
	if !res.Pass {
		status = "FAIL"
	}
	fmt.Fprintf(&b, "# Scenario %s (%s, %.1fs)\n\n", name, status, res.Seconds)
	if about != "" {
		fmt.Fprintf(&b, "%s\n\n", about)
	}
	for _, f := range res.Failures {
		fmt.Fprintf(&b, "- **%s**\n", strings.ReplaceAll(f, "\n", "\n  "))
	}
	if len(res.Failures) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("Slack, as it would have looked (fake Slack; the clock is the sandbox's):\n\n```text\n")
	b.WriteString(rendered)
	b.WriteString("```\n")
	return b.String()
}

// RunAll plays scenarios one after another, each on its own stack, and writes results.md.
func RunAll(scs []*Scenario, o RunOptions, progress io.Writer) ([]Result, error) {
	if err := os.MkdirAll(o.Out, 0o700); err != nil {
		return nil, err
	}
	if progress == nil {
		progress = io.Discard
	}
	var results []Result
	for _, sc := range scs {
		res := RunScenario(sc, filepath.Join(o.Out, sc.Name), o.Bin)
		status := "ok  "
		if !res.Pass {
			status = "FAIL"
		}
		fmt.Fprintf(progress, "%s %-22s %5.1fs\n", status, sc.Name, res.Seconds)
		for _, f := range res.Failures {
			fmt.Fprintf(progress, "     %s\n", strings.ReplaceAll(f, "\n", "\n     "))
		}
		results = append(results, res)
	}
	os.WriteFile(filepath.Join(o.Out, "results.md"), []byte(ResultsMD(results)), 0o644)
	writeJSON(filepath.Join(o.Out, "results.json"), results)
	return results, nil
}

// ResultsMD is the results table for a PR description.
func ResultsMD(results []Result) string {
	var b strings.Builder
	b.WriteString("| Scenario | Result | Time |\n| --- | --- | --- |\n")
	total := 0.0
	for _, r := range results {
		st := "pass"
		if !r.Pass {
			st = "**FAIL**"
		}
		total += r.Seconds
		fmt.Fprintf(&b, "| %s | %s | %.1fs |\n", r.Name, st, r.Seconds)
	}
	fmt.Fprintf(&b, "| all | %d of %d pass | %.1fs |\n", passed(results), len(results), total)
	return b.String()
}

func passed(rs []Result) int {
	n := 0
	for _, r := range rs {
		if r.Pass {
			n++
		}
	}
	return n
}

// LoadScenarios loads scenario files (a directory means its *.json, in name order).
func LoadScenarios(paths []string) ([]*Scenario, error) {
	var files []string
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if fi.IsDir() {
			m, _ := filepath.Glob(filepath.Join(p, "*.json"))
			files = append(files, m...)
			continue
		}
		files = append(files, p)
	}
	var out []*Scenario
	seen := map[string]bool{}
	for _, f := range files {
		sc, err := LoadScenario(f)
		if err != nil {
			return nil, err
		}
		if seen[sc.Name] {
			return nil, fmt.Errorf("two scenarios are called %q", sc.Name)
		}
		seen[sc.Name] = true
		out = append(out, sc)
	}
	return out, nil
}

// PRsMD lists the PRs the runner opened on the fake GitHub, with their bodies.
func PRsMD(prs []PR) string {
	if len(prs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nPRs the runner opened on the fake GitHub:\n")
	for _, p := range prs {
		fmt.Fprintf(&b, "\n%s#%d %s (%s → %s, head `%.7s`", p.Repo, p.Number, p.Title, p.Head, p.Base, p.HeadSHA)
		if p.Edits > 0 {
			fmt.Fprintf(&b, ", updated %d×", p.Edits)
		}
		fmt.Fprintf(&b, ")\n\n```markdown\n%s\n```\n", strings.TrimRight(p.Body, "\n"))
		for i, c := range p.Comments {
			fmt.Fprintf(&b, "\nComment %d by the runner:\n\n```markdown\n%s\n```\n", i+1, strings.TrimRight(c, "\n"))
		}
	}
	return b.String()
}
