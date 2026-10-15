package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MaxEvidenceBytes is the most one evidence file may hold (20 MB).
const MaxEvidenceBytes = 20 << 20

// Evidence is a file attached to an item.
type Evidence struct {
	ItemID      int64     `json:"item_id"`
	N           int       `json:"n"`
	LeaseID     string    `json:"lease_id"`
	Label       string    `json:"label,omitempty"`
	Name        string    `json:"name"`
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	UploadedBy  string    `json:"uploaded_by"`
	At          time.Time `json:"at"`
	path        string
}

// SanitizeName keeps a file name to [A-Za-z0-9._-], drops any directories and leading dots, and
// caps its length, so it is safe to put in a path.
func SanitizeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	parts := strings.Split(name, "/")
	var keep []string
	for _, p := range parts {
		if p != "" && p != "." && p != ".." {
			keep = append(keep, p)
		}
	}
	name = strings.Join(keep, "_")
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.TrimLeft(b.String(), "._")
	if len(out) > 100 {
		out = out[len(out)-100:]
	}
	if out == "" {
		out = "file"
	}
	return out
}

// AddEvidence stores a file for an item; runnerName must hold the item's live lease. The file is
// streamed to a temp file first (hashed and size-capped on the way), then renamed into place in
// the same transaction that records it.
func (s *Store) AddEvidence(ctx context.Context, runnerName string, id int64, label, name string, body io.Reader) (Evidence, error) {
	actor := "runner:" + runnerName
	var ev Evidence
	// Check before taking the upload, so a runner without the lease is refused cheaply.
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.sweep(tx); err != nil {
			return err
		}
		if _, err := loadItem(tx, id); err != nil {
			return err
		}
		_, err := heldLease(tx, id, runnerName)
		return err
	})
	if err != nil {
		return ev, s.refuse(ctx, actor, id, "evidence", err)
	}

	dir := filepath.Join(s.dir, "evidence", fmt.Sprint(id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ev, err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return ev, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once renamed

	h := sha256.New()
	head := &sniffer{}
	n, err := io.Copy(io.MultiWriter(tmp, h, head), io.LimitReader(body, MaxEvidenceBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return ev, err
	}
	if n > MaxEvidenceBytes {
		return ev, s.refuse(ctx, actor, id, "evidence", errf(CodeTooLarge, "evidence is limited to %d bytes", MaxEvidenceBytes))
	}

	clean := SanitizeName(name)
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.sweep(tx); err != nil {
			return err
		}
		l, err := heldLease(tx, id, runnerName)
		if err != nil {
			return err
		}
		var next int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(n), 0) + 1 FROM evidence WHERE item_id = ?`, id).Scan(&next); err != nil {
			return err
		}
		rel := filepath.Join("evidence", fmt.Sprint(id), fmt.Sprintf("%d-%s", next, clean))
		ev = Evidence{ItemID: id, N: next, LeaseID: l.ID, Label: label, Name: clean, SHA256: hex.EncodeToString(h.Sum(nil)),
			Size: n, ContentType: http.DetectContentType(head.b), UploadedBy: actor, At: s.clock.Now(), path: rel}
		if _, err := tx.Exec(`INSERT INTO evidence (item_id, n, lease_id, label, name, path, sha256, size, content_type, uploaded_by, at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, ev.ItemID, ev.N, ev.LeaseID, ev.Label, ev.Name, rel, ev.SHA256, ev.Size, ev.ContentType,
			ev.UploadedBy, ev.At.UnixNano()); err != nil {
			return err
		}
		if err := s.event(tx, Event{ItemID: id, Actor: actor, Kind: "evidence",
			Data: map[string]any{"n": ev.N, "name": ev.Name, "label": ev.Label, "size": ev.Size, "sha256": ev.SHA256}}); err != nil {
			return err
		}
		return os.Rename(tmpName, filepath.Join(s.dir, rel))
	})
	if err != nil {
		if ev.path != "" {
			os.Remove(filepath.Join(s.dir, ev.path)) // the row did not commit, so neither does the file
		}
		return Evidence{}, s.refuse(ctx, actor, id, "evidence", err)
	}
	return ev, nil
}

// OpenEvidence opens evidence n of an item. The caller closes the file.
func (s *Store) OpenEvidence(ctx context.Context, id int64, n int) (Evidence, *os.File, error) {
	var ev Evidence
	var at int64
	err := s.r.QueryRowContext(ctx, `SELECT item_id, n, lease_id, label, name, path, sha256, size, content_type, uploaded_by, at
		FROM evidence WHERE item_id = ? AND n = ?`, id, n).Scan(&ev.ItemID, &ev.N, &ev.LeaseID, &ev.Label, &ev.Name, &ev.path,
		&ev.SHA256, &ev.Size, &ev.ContentType, &ev.UploadedBy, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return ev, nil, errf(CodeNotFound, "no evidence %d on item %d", n, id)
	}
	if err != nil {
		return ev, nil, err
	}
	ev.At = fromNanos(at)
	f, err := os.Open(filepath.Join(s.dir, ev.path))
	return ev, f, err
}

func listEvidence(tx *sql.Tx, id int64) ([]Evidence, error) {
	rows, err := tx.Query(`SELECT item_id, n, lease_id, label, name, path, sha256, size, content_type, uploaded_by, at
		FROM evidence WHERE item_id = ? ORDER BY n`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Evidence{}
	for rows.Next() {
		var ev Evidence
		var at int64
		if err := rows.Scan(&ev.ItemID, &ev.N, &ev.LeaseID, &ev.Label, &ev.Name, &ev.path, &ev.SHA256, &ev.Size,
			&ev.ContentType, &ev.UploadedBy, &at); err != nil {
			return nil, err
		}
		ev.At = fromNanos(at)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// sniffer keeps the first 512 bytes, for content-type detection.
type sniffer struct{ b []byte }

func (s *sniffer) Write(p []byte) (int, error) {
	if room := 512 - len(s.b); room > 0 {
		s.b = append(s.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
