// Package fakeagy is a stand-in for the Antigravity CLI's headless mode (`agy -p <prompt> --model
// <m> --mode plan --sandbox --output-format json --json-schema <file>`), the runner's Gemini
// second opinion (docs/decisions/2026-10-18-gemini-second-opinion.md), for the runner's tests and
// the loop's harness. It prints the JSON agy prints ({response, status, usage}), never talks to a
// model, and plays the scenario in <FAKE_CLAUDE_SCRIPTS>/agy.json ({"play": "..."}; default ok).
// Each call appends its arguments, its working directory and whether it could write there to
// <FAKE_CLAUDE_SCRIPTS>/agy.calls, and a hanging one writes <FAKE_CLAUDE_SCRIPTS>/agy.hanging, so
// a test knows it is waiting.
package fakeagy

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Plays.
const (
	OK      = "ok"      // two findings on the diff's first file: one high, one low
	None    = "none"    // no findings
	Quota   = "quota"   // exit 1 with a quota error on stderr, as a free plan's limit
	Garbage = "garbage" // exit 0, but stdout is not agy's JSON
	Failed  = "failed"  // exit 0, status "error" and no response
	Hang    = "hang"    // wait (up to two minutes), as a call stuck behind a rate limit
)

// Main runs the fake with agy's arguments and returns its exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	dir := os.Getenv("FAKE_CLAUDE_SCRIPTS")
	cwd, _ := os.Getwd()
	play := OK
	if dir != "" {
		var s struct {
			Play string `json:"play"`
		}
		if b, err := os.ReadFile(filepath.Join(dir, "agy.json")); err == nil && json.Unmarshal(b, &s) == nil && s.Play != "" {
			play = s.Play
		}
		// Whether it could write its working directory (the runner's fence makes it read-only).
		probe := filepath.Join(cwd, ".agy-write-probe")
		writable := os.WriteFile(probe, nil, 0o644) == nil
		os.Remove(probe)
		if f, err := os.OpenFile(filepath.Join(dir, "agy.calls"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			b, _ := json.Marshal(map[string]any{"args": args, "cwd": cwd, "cwd_writable": writable})
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
	var schema string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--json-schema" {
			schema = args[i+1]
		}
	}
	if _, err := os.Stat(schema); err != nil {
		fmt.Fprintf(stderr, "Error: --json-schema: %v\n", err)
		return 2
	}
	switch play {
	case Quota:
		fmt.Fprintln(stderr, "Error: [429] RESOURCE_EXHAUSTED: You have exhausted your daily quota for gemini-3.8-flash-high. Please try again later.")
		return 1
	case Garbage:
		fmt.Fprintln(stdout, "Thinking… here are some thoughts, not JSON")
		return 0
	case Failed:
		b, _ := json.Marshal(map[string]any{"status": "error", "response": nil, "error": "the model returned nothing"})
		fmt.Fprintln(stdout, string(b))
		return 0
	case Hang:
		if dir != "" {
			os.WriteFile(filepath.Join(dir, "agy.hanging"), []byte("hanging\n"), 0o644)
		}
		time.Sleep(2 * time.Minute)
		return 1
	}
	findings := []any{}
	if play == OK {
		diff, _ := os.ReadFile(filepath.Join(cwd, ".loop", "context", "diff.patch"))
		file := "feature.txt"
		if m := regexp.MustCompile(`(?m)^\+\+\+ b/(.+)$`).FindSubmatch(diff); m != nil {
			file = string(m[1])
		}
		findings = []any{
			map[string]any{"severity": "high", "file": file, "line": 1, "claim": "The empty input is not handled: it panics.",
				"evidence": "Line 1 indexes the first element without a length check."},
			map[string]any{"severity": "low", "file": file, "line": 2, "claim": "The name is unclear.",
				"evidence": "`x` says nothing about what it holds."},
		}
	}
	resp, _ := json.Marshal(map[string]any{"findings": findings})
	b, _ := json.Marshal(map[string]any{"response": string(resp), "status": "success",
		"usage": map[string]any{"input_tokens": 5200, "output_tokens": 410, "total_tokens": 5610}})
	fmt.Fprintln(stdout, string(b))
	return 0
}
