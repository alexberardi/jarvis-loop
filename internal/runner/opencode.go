package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// A qwen build run (docs/decisions/2026-10-16-qwen-builder.md): OpenCode, not claude, works the
// same build brief to the same result contract. It runs in the fence (HOME read-only, the
// credential stores masked, the environment scrubbed), with its config written by the runner in
// the run dir (read-only to it) and its sessions in a dir of the item's own, kept across the
// item's runs so a bounce resumes the session. It talks only to the runner's proxy, which adds
// jarvisd's app credentials.

// pickBuilder decides who builds: OpenCode for a build run of an item whose builder is qwen, on a
// runner with a qwen config; claude otherwise. A qwen item on a runner without one builds with
// claude this run (a builder_fallback event says so; loopd's builder stays qwen).
func (r *Runner) pickBuilder(ctx context.Context, ru *run) {
	ru.builder = pipeline.BuilderClaude
	if ru.stage != StageBuild || ru.item.Builder != pipeline.BuilderQwen {
		return
	}
	if r.Cfg.Qwen == nil {
		r.Log.Warn("a qwen item, but this runner has no qwen config: building with claude", "item", ru.item.ID)
		if err := r.Client.Event(ctx, ru.item.ID, "builder_fallback", map[string]any{"from": pipeline.BuilderQwen, "to": pipeline.BuilderClaude,
			"reason": "runner " + r.Cfg.Name + " has no qwen config", "this_run_only": true}); err != nil {
			r.Log.Warn("builder_fallback event", "item", ru.item.ID, "err", err)
		}
		return
	}
	ru.builder = pipeline.BuilderQwen
	ru.maxRun = time.Duration(r.Cfg.Qwen.MaxMinutes) * time.Minute
	// Resume the item's last qwen session when its store is still here (another runner, or a
	// cleared work root, starts a new one).
	if s := ru.item.BuildSession; s != "" {
		if _, err := os.Stat(filepath.Join(r.openCodeHome(ru), "data", "opencode", "opencode.db")); err == nil {
			ru.resume = s
		} else {
			r.Log.Warn("the qwen session to resume is not on this runner; starting a new one", "item", ru.item.ID, "session", s)
		}
	}
}

// openCodeHome is the item's OpenCode data, state and cache (XDG_*), kept across its runs.
func (r *Runner) openCodeHome(ru *run) string {
	return filepath.Join(r.Cfg.WorkRoot, "opencode", strconv.FormatInt(ru.item.ID, 10))
}

// OpenCodeArgs is opencode's argv (after the binary) for a run: the brief comes on stdin.
func OpenCodeArgs(model, resume string) []string {
	args := []string{"run", "--pure", "--format", "json", "-m", "qwen/" + model}
	if resume != "" {
		args = append(args, "-s", resume)
	}
	return args
}

// openCodeConfig is the opencode.json the runner writes for a run: one OpenAI-compatible
// provider, the runner's proxy, with the run's token as its key; edits and bash allowed, web
// fetches denied; no sharing, no updates.
func openCodeConfig(proxyURL, token, model string) ([]byte, error) {
	return json.MarshalIndent(map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{"qwen": map[string]any{
			"npm": "@ai-sdk/openai-compatible", "name": "Qwen (jarvisd)",
			"options": map[string]any{"baseURL": proxyURL, "apiKey": token},
			"models":  map[string]any{model: map[string]any{"name": model}},
		}},
		"model": "qwen/" + model, "small_model": "qwen/" + model,
		"permission": map[string]any{"edit": "allow", "bash": "allow", "webfetch": "deny"},
		"share":      "disabled", "autoupdate": false,
	}, "", "  ")
}

// openCodeEnv are the variables OpenCode gets on top of the agent's environment.
func openCodeEnv(cfgDir, cfgFile, home string) []string {
	return []string{
		"XDG_CONFIG_HOME=" + cfgDir, "XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"), "XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"OPENCODE_CONFIG=" + cfgFile,
		// The repo's own opencode.json must not loosen the permissions; nothing is fetched or
		// shared.
		"OPENCODE_DISABLE_PROJECT_CONFIG=1", "OPENCODE_DISABLE_AUTOUPDATE=1", "OPENCODE_DISABLE_MODELS_FETCH=1",
		"OPENCODE_DISABLE_SHARE=1", "OPENCODE_DISABLE_LSP_DOWNLOAD=1",
	}
}

// openCodeCommand starts the run's proxy and builds the fenced opencode command; stop ends the
// proxy once the run is over.
func (r *Runner) openCodeCommand(ru *run) (cmd *exec.Cmd, stop func(), err error) {
	q := r.Cfg.Qwen
	bin := q.OpenCode
	if !strings.Contains(bin, "/") {
		if bin, err = exec.LookPath(bin); err != nil {
			return nil, nil, fmt.Errorf("opencode: %w", err)
		}
	}
	token := randHex(24)
	proxy, err := startQwenProxy(q, token)
	if err != nil {
		return nil, nil, err
	}
	stop = func() {
		ru.qwenRequests += proxy.Requests()
		proxy.Close()
	}
	fail := func(e error) (*exec.Cmd, func(), error) { stop(); return nil, nil, e }
	// The config dir is in the item's OpenCode home, which the fence leaves writable: OpenCode
	// writes a .gitignore there at start and installs its provider package there (it exits with
	// "FileSystem.writeFile" otherwise). The runner rewrites opencode.json before every run, so
	// what the agent may change there lasts one run at most.
	home := r.openCodeHome(ru)
	cfgDir := filepath.Join(home, "config")
	cfgFile := filepath.Join(cfgDir, "opencode", "opencode.json")
	b, err := openCodeConfig(proxy.URL(), token, q.Model)
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(filepath.Dir(cfgFile), 0o700); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(cfgFile, b, 0o600); err != nil {
		return fail(err)
	}
	for _, d := range []string{"data", "state", "cache"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			return fail(err)
		}
	}
	args := OpenCodeArgs(q.Model, ru.resume)
	argv, _ := json.Marshal(args)
	if err := os.WriteFile(filepath.Join(ru.dir, "argv.json"), argv, 0o600); err != nil {
		return fail(err)
	}
	script := "exec " + shellQuote(bin)
	for _, a := range args {
		script += " " + shellQuote(a)
	}
	env := append(r.agentEnv(), openCodeEnv(cfgDir, cfgFile, home)...)
	fc, err := r.fenceWith(ru.wt, script, env, fenceOpts{
		// Its sessions, and of the mirror only what a commit in the worktree writes: objects, refs
		// and logs (the worktree's admin dir the fence binds last). The mirror is shared with the
		// runner's other lanes (land, plans, other builds), so its root and worktrees/ stay
		// read-only: no commondir, config or another worktree's plumbing can be planted there.
		Writable: append([]string{home}, mirrorWritable(ru.mirror)...),
		ReadOnly: mirrorReadOnly(ru.mirror),
		// The binary's own dir, which may sit under the /tmp the fence makes private; and the
		// mirror, bound read-only before the writable paths inside it.
		Visible: []string{filepath.Dir(bin), ru.mirror},
	})
	if err != nil {
		return fail(err)
	}
	return fc, stop, nil
}

// gitPlumbing is what the runner's own (unfenced) git reads to find a worktree's repository and
// its config, file by file ("" for a missing file): the files the fence keeps read-only, and
// those it cannot, as they need not exist (the mirror's commondir).
func gitPlumbing(wt, mirror string) map[string]string {
	paths := []string{filepath.Join(mirror, "commondir"), filepath.Join(mirror, "config"), filepath.Join(mirror, "config.worktree")}
	paths = append(paths, worktreePlumbing(wt, gitAdminDir(wt))...)
	out := map[string]string{}
	for _, p := range paths {
		b, _ := os.ReadFile(p)
		out[p] = string(b)
	}
	return out
}

// restorePlumbing puts back what a run changed in the git plumbing (removing what it added) and
// names the files; nil when nothing changed. The runner's git must not read a single one of
// them as the run left it.
func restorePlumbing(before map[string]string) (changed []string, err error) {
	for p, was := range before {
		b, rerr := os.ReadFile(p)
		if rerr == nil && string(b) == was || errors.Is(rerr, fs.ErrNotExist) && was == "" {
			continue
		}
		changed = append(changed, p)
		if was == "" {
			err = errors.Join(err, os.RemoveAll(p))
		} else {
			err = errors.Join(err, os.WriteFile(p, []byte(was), 0o644))
		}
	}
	slices.Sort(changed)
	return changed, err
}

// mirrorWritable are the dirs of a bare mirror a commit in one of its worktrees writes (logs
// created when missing); the rest of the mirror stays read-only.
func mirrorWritable(mirror string) []string {
	if mirror == "" {
		return nil
	}
	os.MkdirAll(filepath.Join(mirror, "logs"), 0o755)
	return []string{filepath.Join(mirror, "objects"), filepath.Join(mirror, "refs"), filepath.Join(mirror, "logs")}
}

// mirrorReadOnly are the paths of a bare mirror that run code in the runner's own git calls; they
// are created when missing, so a writable mirror cannot grow them.
func mirrorReadOnly(mirror string) []string {
	if mirror == "" {
		return nil
	}
	out := []string{filepath.Join(mirror, "config")}
	cw := filepath.Join(mirror, "config.worktree")
	if f, err := os.OpenFile(cw, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Close()
		out = append(out, cw)
	}
	for _, d := range []string{"hooks", "info"} {
		p := filepath.Join(mirror, d)
		os.MkdirAll(p, 0o755)
		out = append(out, p)
	}
	return out
}

// handleOpenCode reads one line of `opencode run --format json`: the session id (on every event),
// the last text, tool calls and the tokens of each step.
func (r *Runner) handleOpenCode(st *streamStats, line []byte) {
	var ev struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionID"`
		Part      struct {
			Text   string `json:"text"`
			Tokens *struct {
				Input  int64 `json:"input"`
				Output int64 `json:"output"`
			} `json:"tokens"`
		} `json:"part"`
		Error *struct {
			Name string `json:"name"`
			Data struct {
				Message string `json:"message"`
			} `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	if st.sessionID == "" && ev.SessionID != "" {
		st.sessionID = ev.SessionID
	}
	switch ev.Type {
	case "text":
		if t := strings.TrimSpace(ev.Part.Text); t != "" {
			st.lastText = clip(t, 1500)
		}
	case "tool_use":
		st.toolResults++
	case "step_finish":
		if ev.Part.Tokens != nil {
			st.qwenInput += ev.Part.Tokens.Input
			st.qwenOutput += ev.Part.Tokens.Output
		}
	case "error":
		if ev.Error != nil {
			st.lastText = clip(strings.TrimSpace("OpenCode error: "+ev.Error.Name+": "+ev.Error.Data.Message), 1500)
		}
	}
}
