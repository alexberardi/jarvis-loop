// Package runner is the loop runner (docs/phases/04-runner-build.md): it claims build items from
// loopd, prepares a worktree of a bare mirror it owns, runs one headless `claude -p` per item
// inside four boundary layers, validates what the agent claims against git, and reports. The
// agent never holds a loopd credential.
package runner

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Config is the runner's config file (0600: it holds the runner's loopd token).
type Config struct {
	LoopdURL     string   `json:"loopd_url"`
	Token        string   `json:"token"`
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
	// Slots is how many items this runner works at once; loopd's active-agents number still caps
	// the total.
	Slots    int    `json:"slots"`
	WorkRoot string `json:"work_root"`
	// Claude and Guard are the binaries; Guard defaults to loop-guard next to the runner.
	Claude string `json:"claude"`
	Guard  string `json:"guard"`
	// Mode is "auto" (the classifier) or "strict" (dontAsk and an allowlist).
	Mode        string                `json:"mode"`
	Model       string                `json:"model,omitempty"`
	PollSeconds int                   `json:"poll_seconds"`
	Repos       map[string]RepoConfig `json:"repos"`
	// Extra protected paths (never named by a command, never written) and secrets (never read).
	ProtectedPaths []string `json:"protected_paths,omitempty"`
	SecretPaths    []string `json:"secret_paths,omitempty"`
	// LoopdTokens and LoopdData are protected and secret when set (loopd on the same machine).
	LoopdTokens string `json:"loopd_tokens_file,omitempty"`
	LoopdData   string `json:"loopd_data_dir,omitempty"`
	// UserEmulator is the user's own emulator serial, which no command may name.
	UserEmulator string `json:"user_emulator"`
	// AllowedDomains extends the sandbox's network allowlist.
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	// VerifyPR checks the PR on GitHub (gh pr view) against git before reporting. Default true.
	VerifyPR *bool `json:"verify_pr,omitempty"`

	path string
}

// RepoConfig is one repository the runner works on, keyed by the item's repo field.
type RepoConfig struct {
	// Local is the user's clone, used only as a source of git objects; never written.
	Local string `json:"local,omitempty"`
	// Remote is the GitHub URL the mirror fetches from and the agent pushes to (https, so the
	// gh credential helper works through the sandbox proxy).
	Remote string `json:"remote"`
	// Slug is owner/name, what the agent's result must name.
	Slug string `json:"slug"`
	Base string `json:"base"`
	// Prepare commands run by the runner in the fresh worktree before the agent (from the base
	// branch's own files), e.g. fetching the sherpa libs.
	Prepare []string `json:"prepare,omitempty"`
	// Cleanup commands run by the runner when the lease ends, after restoring the harness files
	// they use to the base branch's version; {sandbox} is the item's sandbox name.
	Cleanup        []string `json:"cleanup,omitempty"`
	CleanupRestore []string `json:"cleanup_restore,omitempty"`
	// ExcludedCommands run outside the OS sandbox (sandbox.excludedCommands). Empty by default;
	// see docs/decisions/2026-10-15-runner-choices.md before adding any.
	ExcludedCommands []string `json:"excluded_commands,omitempty"`
	// DenyWrite are worktree-relative paths no tool may change.
	DenyWrite []string `json:"deny_write,omitempty"`
}

// LoadConfig reads and checks the config file. It refuses a file group or others can read.
func LoadConfig(path string) (*Config, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is mode %o: it holds a token, make it 0600", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c.path = abs
	return &c, c.normalize()
}

// Home is the user's home directory.
func Home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "/"
	}
	return h
}

// Expand expands a leading ~ and makes the path absolute.
func Expand(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" {
		return Home()
	}
	if strings.HasPrefix(p, "~/") {
		p = filepath.Join(Home(), p[2:])
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func (c *Config) normalize() error {
	if c.Slots <= 0 {
		c.Slots = 1
	}
	if c.PollSeconds <= 0 {
		c.PollSeconds = 30
	}
	if c.WorkRoot == "" {
		c.WorkRoot = "~/.cache/jarvis-loop/work"
	}
	c.WorkRoot = Expand(c.WorkRoot)
	if c.Claude == "" {
		c.Claude = "claude"
	}
	if c.Guard == "" {
		if exe, err := os.Executable(); err == nil {
			c.Guard = filepath.Join(filepath.Dir(exe), "loop-guard")
		}
	}
	c.Guard = Expand(c.Guard)
	if c.Mode == "" {
		c.Mode = ModeAuto
	}
	if c.UserEmulator == "" {
		c.UserEmulator = "emulator-5554"
	}
	if c.VerifyPR == nil {
		t := true
		c.VerifyPR = &t
	}
	c.LoopdTokens, c.LoopdData = Expand(c.LoopdTokens), Expand(c.LoopdData)
	for i, p := range c.ProtectedPaths {
		c.ProtectedPaths[i] = Expand(p)
	}
	for i, p := range c.SecretPaths {
		c.SecretPaths[i] = Expand(p)
	}
	switch {
	case c.Name == "":
		return fmt.Errorf("config: name is required")
	case len(c.Token) < 16:
		return fmt.Errorf("config: token is missing or too short")
	case c.Mode != ModeAuto && c.Mode != ModeStrict:
		return fmt.Errorf("config: mode must be %q or %q", ModeAuto, ModeStrict)
	case len(c.Repos) == 0:
		return fmt.Errorf("config: no repos")
	}
	u, err := url.Parse(c.LoopdURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("config: loopd_url %q is not an http(s) URL", c.LoopdURL)
	}
	for name, r := range c.Repos {
		if r.Remote == "" || r.Slug == "" || !strings.Contains(r.Slug, "/") {
			return fmt.Errorf("config: repo %s needs remote and slug (owner/name)", name)
		}
		if r.Base == "" {
			r.Base = "main"
		}
		r.Local = Expand(r.Local)
		c.Repos[name] = r
	}
	return nil
}

// Repo finds a repo by its config key or by its slug (loopd keeps the PR's owner/name on the
// item once a PR exists).
func (c *Config) Repo(name string) (string, RepoConfig, bool) {
	if r, ok := c.Repos[name]; ok {
		return name, r, true
	}
	for k, r := range c.Repos {
		if r.Slug == name {
			return k, r, true
		}
	}
	return "", RepoConfig{}, false
}

// Path is the config file's own path.
func (c *Config) Path() string { return c.path }

// Protected lists what no command may name and no tool may write: the user's checkouts, the
// credential stores, loopd's files and this config.
func (c *Config) Protected() []string {
	h := Home()
	out := []string{filepath.Join(h, ".config", "jarvis-harness"), filepath.Join(h, ".config", "jarvis-loop"),
		filepath.Join(h, ".ssh"), filepath.Join(h, ".claude", ".credentials.json")}
	for _, r := range c.Repos {
		if r.Local != "" {
			out = append(out, r.Local)
		}
	}
	out = append(out, c.ProtectedPaths...)
	return uniq(append(out, c.ownSecrets()...))
}

// Secrets lists what neither the file tools nor sandboxed commands may read.
func (c *Config) Secrets() []string {
	h := Home()
	out := []string{filepath.Join(h, ".config", "jarvis-harness"), filepath.Join(h, ".config", "jarvis-loop"),
		filepath.Join(h, ".ssh"), filepath.Join(h, ".claude", ".credentials.json")}
	out = append(out, c.SecretPaths...)
	return uniq(append(out, c.ownSecrets()...))
}

func (c *Config) ownSecrets() []string {
	var out []string
	for _, p := range []string{c.path, c.LoopdTokens, c.LoopdData} {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// LoopdAddrs are the host:port spellings of loopd's address an agent could use.
func (c *Config) LoopdAddrs() []string {
	u, err := url.Parse(c.LoopdURL)
	if err != nil {
		return nil
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	hosts := []string{host}
	if ip := net.ParseIP(host); (ip != nil && ip.IsLoopback()) || host == "localhost" {
		hosts = []string{"127.0.0.1", "localhost", "[::1]", "0.0.0.0"}
	}
	var out []string
	for _, h := range hosts {
		out = append(out, net.JoinHostPort(strings.Trim(h, "[]"), port))
	}
	return out
}

func uniq(in []string) []string {
	var out []string
	for _, s := range in {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}
