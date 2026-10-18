package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// The Gemini second opinion (docs/decisions/2026-10-18-gemini-second-opinion.md): after a review
// run approves, the runner asks Gemini, through the Antigravity CLI (agy), for adversarial
// findings on the same diff, and a Claude turn judges each one (secondopinion.go). Any failure of
// agy (quota, rate limit, timeout, bad output) is "unavailable": the review stands.

// Gemini config defaults.
const (
	DefaultGeminiModel   = "gemini-3.8-flash-high"
	DefaultGeminiTimeout = 10 * time.Minute
)

// GeminiConfig is the runner config's gemini block. Absent, there is no second opinion.
type GeminiConfig struct {
	// Agy is the Antigravity CLI (default "agy" on PATH).
	Agy string `json:"agy,omitempty"`
	// Model is the Gemini model agy runs (default gemini-3.8-flash-high).
	Model string `json:"model,omitempty"`
	// Blocking sends a review back to build when Claude agrees with a high finding (at most once
	// per head). False (the default): advisory, the findings are recorded only.
	Blocking bool `json:"blocking,omitempty"`
	// Timeout caps one agy call, a Go duration (default "10m"); the run's own time cap still
	// applies.
	Timeout string `json:"timeout,omitempty"`
	// Home is agy's login and state (default ~/.gemini). The runner never reads it; agy's fence
	// leaves it writable, and every other fenced command and agent gets it masked.
	Home string `json:"home,omitempty"`
	// Fence runs agy under the runner's fence (default true): the credential stores masked, the
	// worktree read-only.
	Fence *bool `json:"fence,omitempty"`

	timeout time.Duration
}

func (g *GeminiConfig) normalize() error {
	if g.Agy == "" {
		g.Agy = "agy"
	}
	if strings.Contains(g.Agy, "/") {
		g.Agy = Expand(g.Agy)
	}
	if g.Model == "" {
		g.Model = DefaultGeminiModel
	}
	if strings.ContainsAny(g.Model, "/ \t\n\"'") {
		return fmt.Errorf("config: gemini.model must be a plain model name, not %q", g.Model)
	}
	g.timeout = DefaultGeminiTimeout
	if g.Timeout != "" {
		d, err := time.ParseDuration(g.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("config: gemini.timeout must be a positive duration like \"10m\", not %q", g.Timeout)
		}
		g.timeout = d
	}
	if g.Home == "" {
		g.Home = "~/.gemini"
	}
	g.Home = Expand(g.Home)
	if g.Fence == nil {
		t := true
		g.Fence = &t
	}
	return nil
}

// TimeoutDur is one agy call's time limit.
func (g *GeminiConfig) TimeoutDur() time.Duration {
	if g.timeout > 0 {
		return g.timeout
	}
	return DefaultGeminiTimeout
}

// AgyArgs are agy's arguments for one headless, read-only call.
func AgyArgs(prompt, model, schema string) []string {
	return []string{"-p", prompt, "--model", model, "--mode", "plan", "--sandbox", "--output-format", "json", "--json-schema", schema}
}

// Finding is one of Gemini's findings, numbered by the runner (ID, from 1).
type Finding struct {
	ID       int    `json:"id"`
	Severity string `json:"severity"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Claim    string `json:"claim"`
	Evidence string `json:"evidence"`
}

// Severities, most severe first.
var Severities = []string{"high", "medium", "low"}

// MaxFindings caps the findings the runner keeps from one call (the rest are counted only).
const MaxFindings = 20

// FindingsSchema is the JSON schema agy's answer must match.
const FindingsSchema = `{
  "type": "object",
  "properties": {
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "severity": {"type": "string", "enum": ["high", "medium", "low"]},
          "file": {"type": "string"},
          "line": {"type": "integer"},
          "claim": {"type": "string"},
          "evidence": {"type": "string"}
        },
        "required": ["severity", "file", "line", "claim", "evidence"]
      }
    }
  },
  "required": ["findings"]
}
`

// Why agy gave no findings: every one means "second opinion unavailable".
const (
	AgyQuota     = "quota"      // a quota or rate limit
	AgyTimeout   = "timeout"    // the call hit gemini.timeout or the run's time
	AgyError     = "error"      // it exited non-zero, or said its status was an error
	AgyBadOutput = "bad_output" // its output was not the JSON asked for
	AgyNoStart   = "no_start"   // it could not start (not found, no fence)
	AgyStopped   = "stopped"    // loopd ended the lease, or the runner is stopping
)

// agyResult is one agy call's answer.
type agyResult struct {
	// Reason is "" when the call answered, else one of the Agy* reasons.
	Reason   string
	Findings []Finding
	// Dropped are the findings past MaxFindings or not well formed.
	Dropped int
	// Usage is agy's usage object as it gave it (clipped), for the record.
	Usage   string
	Seconds int
}

var quotaRe = regexp.MustCompile(`(?i)quota|rate[ _-]?limit|resource[ _]exhausted|too many requests|\b429\b`)

// ParseAgy reads agy's JSON output ({response, status, usage}; response is the schema's JSON, as
// a string or an object). An error means the output is not usable; its reason is AgyBadOutput,
// AgyError or AgyQuota.
func ParseAgy(stdout []byte) (agyResult, string) {
	var o struct {
		Response json.RawMessage `json:"response"`
		Status   string          `json:"status"`
		Usage    json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &o); err != nil {
		return agyResult{}, AgyBadOutput
	}
	res := agyResult{}
	if len(o.Usage) > 0 && string(o.Usage) != "null" {
		res.Usage = clip(string(o.Usage), 300)
	}
	st := strings.ToLower(o.Status)
	failed := strings.Contains(st, "error") || strings.Contains(st, "fail")
	if failed && quotaRe.Match(stdout) {
		return res, AgyQuota
	}
	resp := o.Response
	var s string
	if json.Unmarshal(resp, &s) == nil {
		s = strings.TrimSpace(s)
		// A model may wrap its JSON in a code fence.
		if strings.HasPrefix(s, "```") {
			s = strings.TrimPrefix(strings.TrimPrefix(s, "```json"), "```")
			s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		}
		resp = json.RawMessage(s)
	}
	var doc struct {
		Findings []struct {
			Severity string          `json:"severity"`
			File     string          `json:"file"`
			Line     json.RawMessage `json:"line"`
			Claim    string          `json:"claim"`
			Evidence string          `json:"evidence"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(resp, &doc); err != nil || doc.Findings == nil {
		if failed {
			return res, AgyError
		}
		return res, AgyBadOutput
	}
	for _, f := range doc.Findings {
		sev := strings.ToLower(strings.TrimSpace(f.Severity))
		ok := false
		for _, s := range Severities {
			ok = ok || s == sev
		}
		claim := strings.TrimSpace(f.Claim)
		if !ok || claim == "" || len(res.Findings) == MaxFindings {
			res.Dropped++
			continue
		}
		var line int
		if json.Unmarshal(f.Line, &line) != nil {
			var ls string
			json.Unmarshal(f.Line, &ls)
			fmt.Sscanf(ls, "%d", &line)
		}
		if line < 0 {
			line = 0
		}
		res.Findings = append(res.Findings, Finding{ID: len(res.Findings) + 1, Severity: sev, File: clip(oneLine(f.File), 200),
			Line: line, Claim: clip(strings.TrimSpace(claim), 1000), Evidence: clip(strings.TrimSpace(f.Evidence), 1500)})
	}
	return res, ""
}

// agyFailure names why a call that exited non-zero (or printed nothing usable) failed, from its
// output; the output itself goes nowhere but the run dir.
func agyFailure(stdout, stderr []byte) string {
	if quotaRe.Match(stderr) || quotaRe.Match(stdout) {
		return AgyQuota
	}
	return AgyError
}

// agyCommand is the agy command for a call in the review's worktree: under the fence (the
// worktree read-only, agy's home writable) unless gemini.fence is false.
func (r *Runner) agyCommand(ru *run, args []string) (*exec.Cmd, error) {
	g := ru.cfg.Gemini
	bin := g.Agy
	if !strings.Contains(bin, "/") {
		p, err := exec.LookPath(bin)
		if err != nil {
			return nil, err
		}
		bin = p
	}
	env := r.agentEnv()
	if !*g.Fence {
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = ru.wt, env
		return cmd, nil
	}
	script := "exec " + shellQuote(bin)
	for _, a := range args {
		script += " " + shellQuote(a)
	}
	visible := []string{filepath.Dir(bin)}
	if real, err := filepath.EvalSymlinks(bin); err == nil {
		visible = append(visible, filepath.Dir(real)) // a link to a versioned install
	}
	return r.fenceWith(ru.wt, script, env, fenceOpts{
		Writable: []string{g.Home},
		// The worktree (made writable by default) read-only again: agy only reads.
		ReadOnly: []string{ru.wt},
		Visible:  visible,
	})
}

// runAgy runs one agy call with the prompt, for at most limit, heartbeating the lease meanwhile.
// Its stdout and stderr stay in the run dir (gemini-stdout.json, gemini-stderr.log), never
// uploaded: they can hold anything agy printed.
func (r *Runner) runAgy(ctx context.Context, ru *run, prompt string, limit time.Duration) agyResult {
	started := r.Clock.Now()
	done := func(res agyResult) agyResult {
		res.Seconds = int(r.Clock.Now().Sub(started).Seconds())
		return res
	}
	schema := filepath.Join(ru.dir, "gemini-schema.json")
	if err := os.WriteFile(schema, []byte(FindingsSchema), 0o644); err != nil {
		return done(agyResult{Reason: AgyNoStart})
	}
	cmd, err := r.agyCommand(ru, AgyArgs(prompt, ru.cfg.Gemini.Model, schema))
	if err != nil {
		r.Log.Warn("gemini: agy cannot start", "item", ru.item.ID, "err", err)
		return done(agyResult{Reason: AgyNoStart})
	}
	var stdout bytes.Buffer
	stderr, err := os.OpenFile(filepath.Join(ru.dir, "gemini-stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return done(agyResult{Reason: AgyNoStart})
	}
	defer stderr.Close()
	cmd.Stdout, cmd.Stderr = &limitedBuffer{b: &stdout, max: 4 << 20}, stderr
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		r.Log.Warn("gemini: agy cannot start", "item", ru.item.ID, "err", err)
		return done(agyResult{Reason: AgyNoStart})
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	hb := r.Clock.NewTimer(r.heartbeatEvery())
	capT := r.Clock.NewTimer(limit)
	defer func() { hb.Stop(); capT.Stop() }()
	shutdown := ru.done
	reason := ""
	stop := func(why string) {
		if reason == "" {
			reason = why
		}
		_ = signalGroup(pid, syscall.SIGKILL)
	}
	for {
		select {
		case err := <-exited:
			_ = signalGroup(pid, syscall.SIGKILL)
			os.WriteFile(filepath.Join(ru.dir, "gemini-stdout.json"), stdout.Bytes(), 0o600)
			if reason != "" {
				return done(agyResult{Reason: reason})
			}
			errOut, _ := os.ReadFile(stderr.Name())
			var ee *exec.ExitError
			if err != nil && !errors.As(err, &ee) {
				return done(agyResult{Reason: AgyError})
			}
			if err != nil {
				return done(agyResult{Reason: agyFailure(stdout.Bytes(), errOut)})
			}
			res, why := ParseAgy(stdout.Bytes())
			if why == AgyBadOutput && quotaRe.Match(errOut) {
				why = AgyQuota
			}
			res.Reason = why
			return done(res)
		case <-hb.C():
			stopped, err := r.Client.Heartbeat(ctx, ru.lease, "second opinion: Gemini reviewing", nil)
			var ae *APIError
			if err == nil && stopped || errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 {
				stop(AgyStopped)
			} else {
				hb = r.Clock.NewTimer(r.heartbeatEvery())
			}
		case <-capT.C():
			stop(AgyTimeout)
		case <-shutdown:
			shutdown = nil
			stop(AgyStopped)
		}
	}
}

// limitedBuffer keeps at most max bytes of what is written to it.
type limitedBuffer struct {
	b   *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}
