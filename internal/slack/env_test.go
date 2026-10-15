package slack

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeEnv(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "slack.env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	fakeBot = "xoxb-1111-2222-secretbotvalue"
	fakeApp = "xapp-1-A111-3333-secretappvalue"
)

func TestLoadEnv(t *testing.T) {
	p := writeEnv(t, "# tokens\nexport SLACK_BOT_TOKEN=\""+fakeBot+"\"\n\nSLACK_APP_TOKEN='"+fakeApp+"'\nOTHER=x\n", 0o600)
	toks, err := LoadEnv(p)
	if err != nil || toks.Bot != fakeBot || toks.App != fakeApp {
		t.Fatalf("%+v %v", toks, err)
	}
	if s := toks.String(); strings.Contains(s, "secret") {
		t.Fatalf("tokens print themselves: %s", s)
	}
}

func TestLoadEnvRefuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	cases := map[string]struct {
		body string
		mode os.FileMode
	}{
		"readable by others": {"SLACK_BOT_TOKEN=" + fakeBot + "\nSLACK_APP_TOKEN=" + fakeApp + "\n", 0o644},
		"group bits":         {"SLACK_BOT_TOKEN=" + fakeBot + "\nSLACK_APP_TOKEN=" + fakeApp + "\n", 0o640},
		"no app token":       {"SLACK_BOT_TOKEN=" + fakeBot + "\n", 0o600},
		"swapped":            {"SLACK_BOT_TOKEN=" + fakeApp + "\nSLACK_APP_TOKEN=" + fakeBot + "\n", 0o600},
		"bad line":           {"SLACK_BOT_TOKEN " + fakeBot + "\n", 0o600},
	}
	for name, c := range cases {
		_, err := LoadEnv(writeEnv(t, c.body, c.mode))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: the error shows a token: %v", name, err)
		}
	}
}
