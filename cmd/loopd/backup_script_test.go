package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/api"
	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/store"
	"github.com/alexberardi/jarvis-loop/internal/web"
)

// scripts/loopd-backup against a real loopd handler: the nightly pull off the machine
// (docs/decisions/2026-10-23-loopd-on-fly.md). It keeps an archive it has checked, at 0600, and
// prunes the old ones.
//
// The script is not a Go file, so `go test` caches this result across changes to it: run it with
// -count=1 after editing scripts/loopd-backup.
func TestBackupScript(t *testing.T) {
	for _, bin := range []string{"bash", "curl", "tar", "gzip", "find"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("no %s", bin)
		}
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "loopd-backup"))
	if err != nil {
		t.Fatal(err)
	}

	data := t.TempDir()
	st, err := store.Open(data, clock.NewFake(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.AddItem(t.Context(), "user:alex", store.NewItem{
		Title: "in the backup", Priority: "P2", Ask: "something to find in the archive"}); err != nil {
		t.Fatal(err)
	}
	// An evidence file, so the archive is more than the database.
	if err := os.MkdirAll(filepath.Join(data, "evidence", "1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "evidence", "1", "after.png"), []byte("not a png"), 0o600); err != nil {
		t.Fatal(err)
	}

	const userTok = "user-token-0123456789abcdef"
	toks, err := api.ParseTokens(strings.NewReader(userTok + "\tuser\talex\n"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler(st, toks, slog.New(slog.DiscardHandler), web.BoardConfig{}, nil))
	defer srv.Close()

	home := t.TempDir()
	out := filepath.Join(home, "backups")
	// An old archive the prune must take, and a recent one it must leave.
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(out, "loop-backup-20260101T000000Z.tar.gz")
	recent := filepath.Join(out, "loop-backup-20261022T000000Z.tar.gz")
	for _, p := range []string{old, recent} {
		if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	long := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(old, long, long); err != nil {
		t.Fatal(err)
	}

	run := func(env ...string) (string, error) {
		t.Helper()
		cmd := exec.Command("bash", script)
		cmd.Env = append(os.Environ(), append([]string{
			"HOME=" + home, "LOOP_URL=" + srv.URL, "LOOP_BACKUP_DIR=" + out, "LOOP_BACKUP_KEEP=30",
		}, env...)...)
		b, err := cmd.CombinedOutput()
		return string(b), err
	}

	// The token from a file, as the timer passes it: it must not reach the process's argv.
	tokFile := filepath.Join(home, "token")
	if err := os.WriteFile(tokFile, []byte(userTok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := run("LOOP_TOKEN_FILE=" + tokFile)
	if err != nil {
		t.Fatalf("loopd-backup: %v\n%s", err, log)
	}
	if strings.Contains(log, userTok) {
		t.Error("the log carries the token")
	}
	entries, err := filepath.Glob(filepath.Join(out, "loop-backup-*.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	var fresh string
	for _, e := range entries {
		if e != old && e != recent {
			fresh = e
		}
	}
	if fresh == "" {
		t.Fatalf("no new archive in %v\n%s", entries, log)
	}
	if fi, err := os.Stat(fresh); err != nil || fi.Mode().Perm() != 0o600 || fi.Size() == 0 {
		t.Fatalf("%s: %v %v", fresh, fi.Mode().Perm(), err)
	}
	// It checked the archive before keeping it; check the same things here.
	if b, err := exec.Command("tar", "--list", "--file", fresh).Output(); err != nil {
		t.Fatalf("tar: %v", err)
	} else if names := string(b); !strings.Contains(names, "loop.db") || !strings.Contains(names, "evidence/1/after.png") {
		t.Errorf("archive holds:\n%s", names)
	}
	// Pruning: the 60-day-old one is gone, the recent one stays, nothing else leaves.
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the old archive survived: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("the recent archive was pruned: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the new archive was pruned: %v", err)
	}

	// No token, a wrong token and no URL: each fails, keeps nothing, and says what is missing.
	for _, c := range []struct {
		name string
		env  []string
		want string
	}{
		{"no token", nil, "LOOP_TOKEN"},
		{"a token that is not the user's", []string{"LOOP_TOKEN=nope-0123456789abcdef"}, "failed"},
		{"no URL", []string{"LOOP_TOKEN=" + userTok, "LOOP_URL="}, "LOOP_URL"},
	} {
		before, _ := filepath.Glob(filepath.Join(out, "loop-backup-*"))
		log, err := run(c.env...)
		if err == nil {
			t.Errorf("%s: loopd-backup succeeded\n%s", c.name, log)
		}
		if !strings.Contains(log, c.want) {
			t.Errorf("%s: log does not say %q:\n%s", c.name, c.want, log)
		}
		after, _ := filepath.Glob(filepath.Join(out, "loop-backup-*"))
		if len(after) != len(before) {
			t.Errorf("%s: left %d archives, had %d", c.name, len(after), len(before))
		}
	}

	// A 200 carrying a bad archive: loopd cannot change the status once the body has begun
	// (internal/api/backup.go), so a Store.Backup that fails mid-stream reaches curl as a clean,
	// complete 200. Only the script's own checks stand between that and a kept, corrupt backup, so
	// each case is one of them, and asserts its message: with a check removed, the case is either
	// kept or refused by a later check for a different reason, and fails either way.
	var good bytes.Buffer
	if err := st.Backup(t.Context(), &good); err != nil {
		t.Fatal(err)
	}
	gz := func(b []byte) []byte {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(b); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	var noDB bytes.Buffer
	tw := tar.NewWriter(&noDB)
	body := []byte("not a png")
	if err := tw.WriteHeader(&tar.Header{Name: "evidence/1/after.png", Mode: 0o600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	listing := func() []string {
		t.Helper()
		es, err := os.ReadDir(out)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range es {
			names = append(names, e.Name())
		}
		return names
	}
	for _, c := range []struct {
		name  string
		serve []byte
		want  string
	}{
		{"half an archive", good.Bytes()[:good.Len()/2], "not valid gzip"},
		{"gzip that is not a tar", gz(bytes.Repeat([]byte("not a tar "), 200)), "not a readable tar"},
		{"a tar with no loop.db", gz(noDB.Bytes()), "has no loop.db"},
	} {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(c.serve)
		}))
		before := listing()
		log, err := run("LOOP_TOKEN="+userTok, "LOOP_URL="+bad.URL)
		bad.Close()
		if err == nil {
			t.Errorf("%s: loopd-backup kept it\n%s", c.name, log)
		}
		if !strings.Contains(log, c.want) {
			t.Errorf("%s: log does not say %q:\n%s", c.name, c.want, log)
		}
		if after := listing(); strings.Join(after, "\n") != strings.Join(before, "\n") {
			t.Errorf("%s: the backup directory changed: %v, was %v", c.name, after, before)
		}
	}
}
