package slack

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strings"
)

// Tokens are the bot (xoxb) and app (xapp) tokens.
type Tokens struct {
	Bot string
	App string
}

// String never shows the tokens, so a stray %v cannot leak them.
func (Tokens) String() string { return "slack.Tokens{redacted}" }

// GoString is String, for %#v.
func (t Tokens) GoString() string { return t.String() }

// LoadEnv reads SLACK_BOT_TOKEN and SLACK_APP_TOKEN from an env file (KEY=VALUE lines, optional
// `export` and quotes, # comments). It refuses a file that group or others can read or write.
// Errors name lines and keys, never values.
func LoadEnv(path string) (Tokens, error) {
	var t Tokens
	f, err := os.Open(path)
	if err != nil {
		return t, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return t, err
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return t, fmt.Errorf("%s is mode %04o; it holds Slack tokens, so make it 0600", path, fi.Mode().Perm())
	}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return Tokens{}, fmt.Errorf("%s line %d: want KEY=VALUE", path, line)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		switch k {
		case "SLACK_BOT_TOKEN":
			t.Bot = v
		case "SLACK_APP_TOKEN":
			t.App = v
		}
	}
	if err := sc.Err(); err != nil {
		return Tokens{}, err
	}
	if !strings.HasPrefix(t.Bot, "xoxb-") {
		return Tokens{}, fmt.Errorf("%s: SLACK_BOT_TOKEN is missing or is not a bot token (xoxb-…)", path)
	}
	if !strings.HasPrefix(t.App, "xapp-") {
		return Tokens{}, fmt.Errorf("%s: SLACK_APP_TOKEN is missing or is not an app token (xapp-…)", path)
	}
	return t, nil
}
