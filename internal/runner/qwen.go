package runner

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// The Qwen builder (docs/decisions/2026-10-16-qwen-builder.md): build runs of items whose builder
// is qwen run OpenCode against jarvisd's background slot instead of claude. The runner holds the
// app credentials and adds them in a proxy of its own (qwenproxy.go); OpenCode runs in the fence
// (opencode.go).

// DefaultQwenMinutes caps a qwen build run when qwen.max_minutes is not set.
const DefaultQwenMinutes = 45

// QwenConfig is the runner config's qwen block. Absent, items with builder qwen build with claude.
type QwenConfig struct {
	// BaseURL is jarvisd's OpenAI-compatible route, up to the version (".../v1"): OpenCode's
	// requests to <proxy>/v1/chat/completions go to <base_url>/chat/completions.
	BaseURL string `json:"base_url"`
	// CredentialsFile is an env file (0600) with PROD_APP_ID and PROD_APP_KEY, read by the runner
	// for each run; fenced commands and the agents never see it.
	CredentialsFile string `json:"credentials_file"`
	// Model is the model name jarvisd serves (default "background").
	Model string `json:"model,omitempty"`
	// MaxMinutes caps one qwen build run (default 45); loopd's max_run_minutes still applies.
	MaxMinutes int `json:"max_minutes,omitempty"`
	// OpenCode is the opencode binary (default "opencode" on PATH; prefer the real binary over a
	// mise shim, which wants to write its own state).
	OpenCode string `json:"opencode,omitempty"`
}

func (q *QwenConfig) normalize() error {
	q.BaseURL = strings.TrimRight(strings.TrimSpace(q.BaseURL), "/")
	u, err := url.Parse(q.BaseURL)
	if q.BaseURL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("config: qwen.base_url must be an http(s) URL with no query, like \"http://10.0.0.107:7704/v1\", not %q", q.BaseURL)
	}
	if q.CredentialsFile == "" {
		return fmt.Errorf("config: qwen.credentials_file is required (an env file with PROD_APP_ID and PROD_APP_KEY)")
	}
	q.CredentialsFile = Expand(q.CredentialsFile)
	st, err := os.Stat(q.CredentialsFile)
	if err != nil {
		return fmt.Errorf("config: qwen.credentials_file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("config: qwen.credentials_file %s is not a file", q.CredentialsFile)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("config: qwen.credentials_file %s is mode %o: it holds an app key, make it 0600", q.CredentialsFile, st.Mode().Perm())
	}
	if _, _, err := q.Credentials(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if q.Model == "" {
		q.Model = "background"
	}
	if strings.ContainsAny(q.Model, "/ \t\n\"") {
		return fmt.Errorf("config: qwen.model must be a plain model name, not %q", q.Model)
	}
	switch {
	case q.MaxMinutes < 0:
		return fmt.Errorf("config: qwen.max_minutes must be positive, not %d", q.MaxMinutes)
	case q.MaxMinutes == 0:
		q.MaxMinutes = DefaultQwenMinutes
	}
	if q.OpenCode == "" {
		q.OpenCode = "opencode"
	}
	if strings.Contains(q.OpenCode, "/") {
		q.OpenCode = Expand(q.OpenCode)
	}
	return nil
}

// Credentials reads PROD_APP_ID and PROD_APP_KEY from the credentials file. Errors never quote
// the file's values.
func (q *QwenConfig) Credentials() (id, key string, err error) {
	f, err := os.Open(q.CredentialsFile)
	if err != nil {
		return "", "", fmt.Errorf("qwen.credentials_file: %w", err)
	}
	defer f.Close()
	vals := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		vals[strings.TrimSpace(k)] = v
	}
	if err := sc.Err(); err != nil {
		return "", "", fmt.Errorf("qwen.credentials_file: %w", err)
	}
	for _, k := range []string{"PROD_APP_ID", "PROD_APP_KEY"} {
		if vals[k] == "" {
			return "", "", fmt.Errorf("qwen.credentials_file %s has no %s", filepath.Base(q.CredentialsFile), k)
		}
	}
	return vals["PROD_APP_ID"], vals["PROD_APP_KEY"], nil
}
