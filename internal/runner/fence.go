package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The fence (docs/decisions/2026-10-15-all-repos.md, "The fence"): setup, prepare, cleanup and
// verify commands run the agent's code (npm scripts, tests, a jarvisd built from the PR) outside
// the agent's OS sandbox, so the runner runs them under bubblewrap (sandbox-exec on macOS,
// docs/decisions/2026-10-17-mac-runners.md): the host's filesystem and network as they are (the
// harness, the emulator and the LLM tap need loopback), with the credential stores masked, no
// secrets in the environment and a git config with no credential helper. Without a working fence
// they do not run at all.

// DefaultVerifyMask are the paths under home masked by default: credentials (ssh, gh, gpg, git,
// netrc, docker, claude's login, macOS's keychain files), the user's OpenCode, loopd's tokens and
// data, and the Slack tokens. The harness's
// ~/.config/jarvis-harness/installed.env stays readable: jarvis-server's sandbox needs it for
// the LLM tap, and it is an app client for the local jarvisd only.
func DefaultVerifyMask(home string) []string {
	var out []string
	for _, p := range []string{".ssh", ".config/gh", ".config/jarvis-loop", ".config/jarvis-harness/slack.env",
		".config/jarvis-harness/orchestrator-slack.env", ".local/share/jarvis-loop", ".gnupg", ".netrc", ".git-credentials", ".config/git/credentials", ".docker/config.json",
		// Claude's login, and the user's own OpenCode (its auth, config and sessions): a qwen run
		// gets its own (opencode.go).
		".claude/.credentials.json", ".config/opencode", ".local/share/opencode", ".local/state/opencode", ".cache/opencode",
		// macOS: the keychains' files (sandbox-exec also refuses the keychain's services).
		"Library/Keychains"} {
		out = append(out, filepath.Join(home, p))
	}
	return out
}

// DefaultVerifyWritable are the paths under home fenced commands may write besides their own
// working dirs, the runner's fence caches and a private /tmp: the harness's cache (models, the
// mobile build dir and the emulator's AVD) and ~/.android (adb's key and the emulator's state).
// Toolchain caches are not shared with the user's: Go, npm, pip and Gradle get the runner's own
// (fenceCacheEnv), so fenced code cannot poison what the user's unfenced builds read.
func DefaultVerifyWritable(home string) []string {
	return []string{filepath.Join(home, ".cache", "jarvis-harness"), filepath.Join(home, ".android")}
}

// fenceCaches are the toolchain caches fenced commands get, under <work_root>/fence-cache/.
var fenceCaches = [][2]string{{"GOCACHE", "go-build"}, {"GOMODCACHE", "gomod"}, {"npm_config_cache", "npm"},
	{"PIP_CACHE_DIR", "pip"}, {"GRADLE_USER_HOME", "gradle"}}

// fencedEnv is src without credentials: GitHub tokens, git and ssh credential plumbing, and any
// *_TOKEN, *_KEY or *_SECRET (no exceptions: nothing a setup or verify step needs is a secret of
// the user's). Git gets gitconfig as its only global config, no system config and no prompts.
func fencedEnv(src []string, gitconfig string) []string {
	drop := map[string]bool{"GH_TOKEN": true, "GITHUB_TOKEN": true, "GH_ENTERPRISE_TOKEN": true, "GITHUB_ENTERPRISE_TOKEN": true,
		"GIT_ASKPASS": true, "SSH_ASKPASS": true, "SSH_AUTH_SOCK": true, "GIT_CONFIG_GLOBAL": true, "GIT_CONFIG_SYSTEM": true,
		"GIT_CONFIG_NOSYSTEM": true, "GIT_TERMINAL_PROMPT": true, "LOOP_TOKEN": true, "LOOP_URL": true}
	var out []string
	for _, kv := range src {
		k, _, _ := strings.Cut(kv, "=")
		u := strings.ToUpper(k)
		if drop[u] || u == "TMPDIR" || u == "GOTMPDIR" || u == "GOFLAGS" || isFenceCache(k) || strings.HasPrefix(u, "SLACK_") || strings.HasSuffix(u, "_TOKEN") || strings.HasSuffix(u, "_KEY") ||
			strings.HasSuffix(u, "_SECRET") || strings.HasPrefix(u, "GIT_CONFIG_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GIT_CONFIG_GLOBAL="+gitconfig, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
}

func isFenceCache(k string) bool {
	for _, c := range fenceCaches {
		if strings.EqualFold(c[0], k) {
			return true
		}
	}
	return false
}

// gitAdminDir is a linked worktree's admin dir in its repository (from its .git file), "" for
// anything else: git in the worktree writes its index and HEAD there.
func gitAdminDir(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return ""
	}
	p, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !ok {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p)
}

// worktreePlumbing are the files of a linked worktree that tell git where its repository and
// config are: its .git file, and its admin dir's commondir, gitdir and config.worktree (created
// empty when missing, so it cannot be grown). Fenced code may commit (index, HEAD, logs and refs
// stay writable) but not repoint the runner's own unfenced git at a repository it made.
func worktreePlumbing(dir, admin string) []string {
	if admin == "" {
		return nil
	}
	cw := filepath.Join(admin, "config.worktree")
	if f, err := os.OpenFile(cw, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Close()
	}
	return []string{filepath.Join(dir, ".git"), filepath.Join(admin, "commondir"), filepath.Join(admin, "gitdir"), cw}
}

// envHome is HOME as the command will see it.
func envHome(env []string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], "HOME="); ok && v != "" {
			return v
		}
	}
	return Home()
}

// fencedGitConfig is the only git config fenced commands see: an identity, no credential helper
// (so nothing can push with the user's login), no signing, no hooks.
const fencedGitConfig = "[user]\n\tname = jarvis-loop verify\n\temail = jarvis-loop@localhost\n" +
	"[credential]\n\thelper =\n[commit]\n\tgpgsign = false\n[tag]\n\tgpgsign = false\n[core]\n\thooksPath = /dev/null\n"

// ProbeFence checks once that the fence (fenceName: bubblewrap on Linux, sandbox-exec on macOS)
// runs here; the answer is kept. loop-runner calls it at start; setup and verify refuse to run
// without it. It first clears what killed fenced commands left in the runner's fence-tmp.
func (r *Runner) ProbeFence() error {
	r.fenceOnce.Do(func() {
		os.RemoveAll(filepath.Join(r.Cfg.WorkRoot, "fence-tmp"))
		cmd, err := r.fenced(Home(), "true", nil, nil)
		if err == nil {
			if out, e := cmd.CombinedOutput(); e != nil {
				err = fmt.Errorf("%s: %v: %s", r.fenceBinary(), e, clip(strings.TrimSpace(string(out)), 300))
			}
		}
		if err != nil {
			r.fenceErr = fmt.Errorf("%s (the fence for setup and verify commands) does not work: %w", fenceName, err)
		}
	})
	return r.fenceErr
}

// fenced builds the command for script under the fence, cwd dir, with env (already the agent's
// environment) cleaned. HOME is read-only except dir (and, for a linked worktree, its admin dir in
// the mirror: git writes its index there; the mirror's config and hooks stay read-only), the
// extra writable paths, the configured verify_writable and the runner's fence caches; the temp
// dir is private. It does not check the probe.
func (r *Runner) fenced(dir, script string, env, extraWritable []string) (*exec.Cmd, error) {
	return r.fencedWith(dir, script, env, fenceOpts{Writable: extraWritable})
}

// fenceOpts are what a fenced command gets besides the defaults: more writable paths, paths made
// read-only again inside writable ones, and paths bound read-only before the writable ones (so
// they are visible at all under the private /tmp, or read-only around writable dirs inside them).
type fenceOpts struct {
	Writable, ReadOnly, Visible []string
}

// fenceSpec is a fenced command as the OS's fence builds it (fence_linux.go, fence_darwin.go).
type fenceSpec struct {
	dir, script, home, cacheRoot string
	env                          []string
	o                            fenceOpts
}

// fencedWith is fenced with options.
func (r *Runner) fencedWith(dir, script string, env []string, o fenceOpts) (*exec.Cmd, error) {
	if _, err := os.Stat(r.fenceBinary()); err != nil {
		return nil, err
	}
	gc := filepath.Join(r.Cfg.WorkRoot, "fenced.gitconfig")
	if b, err := os.ReadFile(gc); err != nil || string(b) != fencedGitConfig {
		if err := writeAtomic(gc, []byte(fencedGitConfig)); err != nil {
			return nil, err
		}
	}
	env = fencedEnv(env, gc)
	cacheRoot := filepath.Join(r.Cfg.WorkRoot, "fence-cache")
	for _, c := range fenceCaches {
		p := filepath.Join(cacheRoot, c[1])
		if err := os.MkdirAll(p, 0o700); err != nil {
			return nil, err
		}
		env = append(env, c[0]+"="+p)
	}
	env = append(env, "GOFLAGS=-modcacherw")
	return r.fenceCommand(fenceSpec{dir: dir, script: script, home: envHome(env), cacheRoot: cacheRoot, env: env, o: o})
}

// fence is fenced after the probe: an error says why the command cannot run (fail closed).
func (r *Runner) fence(dir, script string, env, extraWritable []string) (*exec.Cmd, error) {
	return r.fenceWith(dir, script, env, fenceOpts{Writable: extraWritable})
}

// fenceWith is fencedWith after the probe.
func (r *Runner) fenceWith(dir, script string, env []string, o fenceOpts) (*exec.Cmd, error) {
	if err := r.ProbeFence(); err != nil {
		return nil, err
	}
	return r.fencedWith(dir, script, env, o)
}
