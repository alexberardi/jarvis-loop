package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loop-harness finds the session file from any directory inside the worktree, and says why when
// there is none or it is not one.
func TestFind(t *testing.T) {
	wt := t.TempDir()
	deep := filepath.Join(wt, "a", "b")
	os.MkdirAll(deep, 0o755)
	if _, err := Find(deep); err == nil || !strings.Contains(err.Error(), "no .loop/harness.json") {
		t.Fatalf("no file: %v", err)
	}
	os.MkdirAll(filepath.Join(wt, ".loop"), 0o755)
	os.WriteFile(filepath.Join(wt, FileRel), []byte(`{"spool":"/w/.loop/harness"}`), 0o600)
	if _, err := Find(deep); err == nil || !strings.Contains(err.Error(), "not a harness session file") {
		t.Fatalf("no token: %v", err)
	}
	os.WriteFile(filepath.Join(wt, FileRel), []byte(`{"spool":"/w/.loop/harness","token":"abc"}`), 0o600)
	if f, err := Find(deep); err != nil || f.Spool != "/w/.loop/harness" || f.Token != "abc" {
		t.Fatalf("%+v %v", f, err)
	}
}
