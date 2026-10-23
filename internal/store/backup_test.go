package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/clock"
)

// Backup is a gzipped tar holding a loop.db that opens on its own (VACUUM INTO, not a copy of a
// live WAL database) plus every other file under the data directory, and none of the live database
// files (docs/decisions/2026-10-23-loopd-on-fly.md).
func TestBackup(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(dir, clock.NewFake(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	it, err := s.AddItem(ctx, "user:alex", NewItem{Title: "backed up", Priority: "P2", Ask: "an item to find in the archive"})
	if err != nil {
		t.Fatal(err)
	}
	// An evidence file, so the archive has to carry more than the database.
	evDir := filepath.Join(dir, "evidence", "1")
	if err := os.MkdirAll(evDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evDir, "after.png"), []byte("not really a png"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := s.Backup(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	files := untar(t, buf.Bytes())
	for _, gone := range []string{"loop.db-wal", "loop.db-shm", backupTmp + "/loop.db"} {
		if _, ok := files[gone]; ok {
			t.Errorf("the archive holds %s", gone)
		}
	}
	if got := string(files["evidence/1/after.png"]); got != "not really a png" {
		t.Errorf("evidence/1/after.png: %q", got)
	}
	db, ok := files["loop.db"]
	if !ok {
		t.Fatalf("no loop.db in the archive: %v", keys(files))
	}
	// The snapshot is a whole database: open it and read the item back out of it.
	restored := filepath.Join(t.TempDir(), "loop.db")
	if err := os.WriteFile(restored, db, 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := sql.Open("sqlite", "file:"+restored+"?_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var title string
	if err := h.QueryRowContext(ctx, `SELECT title FROM items WHERE id = ?`, it.ID).Scan(&title); err != nil {
		t.Fatalf("reading the snapshot: %v", err)
	}
	if title != "backed up" {
		t.Errorf("title %q", title)
	}
	// The same schema, so a restore needs no migration to be readable.
	var v int
	if err := h.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	live, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v != live {
		t.Errorf("snapshot schema %d, live %d", v, live)
	}
	// Nothing is left behind on the volume, and the store still writes.
	if _, err := os.Stat(filepath.Join(dir, backupTmp)); !os.IsNotExist(err) {
		t.Errorf("%s survived the backup: %v", backupTmp, err)
	}
	if _, err := s.AddItem(ctx, "user:alex", NewItem{Title: "after", Priority: "P2", Ask: "the store still writes"}); err != nil {
		t.Fatalf("add after a backup: %v", err)
	}
}

// A leftover snapshot from a run that died mid-copy is cleared, not tarred up or tripped over.
func TestBackupClearsALeftoverSnapshot(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(dir, clock.NewFake(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tmp := filepath.Join(dir, backupTmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "loop.db"), []byte("half a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.Backup(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	files := untar(t, buf.Bytes())
	if got := files["loop.db"]; string(got) == "half a database" {
		t.Error("the leftover snapshot was tarred up as the database")
	}
	if _, ok := files[backupTmp+"/loop.db"]; ok {
		t.Error("the snapshot is in the archive under its temp path too")
	}
}

func untar(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	out := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar %s: %v", h.Name, err)
		}
		out[h.Name] = body
	}
	return out
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
