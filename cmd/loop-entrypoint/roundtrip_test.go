package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// The data move, end to end: what GET /backup produces (Store.Backup) is what the entrypoint
// unpacks, and the store opens on the result with the items and the evidence intact
// (docs/decisions/2026-10-23-loopd-on-fly.md, docs/runbooks/loopd-on-fly.md step 5). The two
// formats are one format, and this is what says so.
func TestBackupArchiveRestoresOntoAVolume(t *testing.T) {
	ctx := t.Context()
	clk := clock.NewFake(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC))

	// The old box: a store with an item and an evidence file.
	old := filepath.Join(t.TempDir(), "prod")
	src, err := store.Open(old, clk)
	if err != nil {
		t.Fatal(err)
	}
	it, err := src.AddItem(ctx, "user:alex", store.NewItem{
		Title: "moved to fly", Priority: "P2", Ask: "the item that has to survive the move"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(old, "evidence", "1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "evidence", "1", "after.png"), []byte("not a png"), 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := src.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The archive, written where the volume would have it.
	vol := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(vol, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(vol, restoreName), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Backup(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}

	// The machine starting on the fresh volume.
	if err := restore(vol); err != nil {
		t.Fatalf("restore: %v", err)
	}
	dst, err := store.Open(vol, clk)
	if err != nil {
		t.Fatalf("the restored data does not open: %v", err)
	}
	defer dst.Close()
	got, err := dst.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("schema version %d, want %d", got, want)
	}
	d, err := dst.GetItem(ctx, it.ID)
	if err != nil {
		t.Fatalf("the item did not survive: %v", err)
	}
	if d.Item.Title != "moved to fly" || d.Item.Stage != it.Stage {
		t.Errorf("item %+v", d.Item)
	}
	if b, err := os.ReadFile(filepath.Join(vol, "evidence", "1", "after.png")); err != nil || string(b) != "not a png" {
		t.Errorf("evidence: %q %v", b, err)
	}
	// And it writes: the restored database is not read-only or half-migrated.
	if _, err := dst.AddItem(ctx, "user:alex", store.NewItem{
		Title: "after the move", Priority: "P2", Ask: "the restored store still writes"}); err != nil {
		t.Fatalf("add after the restore: %v", err)
	}
}
