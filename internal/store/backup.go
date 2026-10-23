package store

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// backupTmp is where Backup puts the database snapshot while it streams it: inside the data
// directory, so it is on the volume with the space for it, and so a crash leaves the leftover
// somewhere the next Backup clears rather than in the image's ephemeral /tmp.
const backupTmp = "backup.tmp"

// Backup writes a gzipped tar of everything worth keeping to w: loop.db, and every other file
// under the data directory (evidence/, regressions/).
//
// The database comes from `VACUUM INTO`, not from copying loop.db: loopd runs in WAL mode with a
// live writer, so the three files on disk are only consistent together and only between writes,
// which is exactly what a nightly copy cannot promise (and what a volume snapshot does not give
// either). VACUUM INTO reads one transaction and writes a whole, already-compact database, so the
// result opens on its own. It holds the single writer connection for its duration, which blocks
// writes for as long as the copy takes; for this database that is well under a second.
//
// Nothing is written into w until the snapshot exists, so a failure there is still an error the
// caller can turn into a status. A failure after that ends the stream mid-file, which is why the
// reader must check the archive (scripts/loopd-backup does).
func (s *Store) Backup(ctx context.Context, w io.Writer) error {
	s.bkMu.Lock()
	defer s.bkMu.Unlock()
	tmp := filepath.Join(s.dir, backupTmp)
	if err := os.RemoveAll(tmp); err != nil { // a leftover from a run that died mid-copy
		return err
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	snap := filepath.Join(tmp, "loop.db")
	// The filename is an expression in SQLite's grammar, so it binds like any other value and
	// needs no quoting of its own.
	if _, err := s.w.ExecContext(ctx, `VACUUM INTO ?`, snap); err != nil {
		return fmt.Errorf("vacuum into %s: %w", snap, err)
	}
	if err := os.Chmod(snap, 0o600); err != nil {
		return err
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err := addFile(tw, snap, "loop.db"); err != nil {
		return err
	}
	// Everything else under the data directory, by its path relative to it: the evidence and
	// regression files the database's rows point at, and anything added later. The live database
	// files are left out: loop.db is the snapshot above, and its -wal and -shm say nothing
	// without the writer that owns them.
	skip := map[string]bool{"loop.db": true, "loop.db-wal": true, "loop.db-shm": true, backupTmp: true}
	err := filepath.WalkDir(s.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(s.dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if skip[rel] {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case d.IsDir():
			return addDir(tw, path, rel)
		case d.Type().IsRegular():
			return addFile(tw, path, rel)
		default:
			return nil // sockets, symlinks: the store makes none, and a backup wants no links
		}
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// addDir writes a directory entry, so an empty evidence/<id> survives the round trip.
func addDir(tw *tar.Writer, path, name string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	h, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return err
	}
	h.Name = toSlash(name) + "/"
	return tw.WriteHeader(h)
}

// addFile writes one regular file, streamed: an evidence file may be large.
func addFile(tw *tar.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	h, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return err
	}
	h.Name = toSlash(name)
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	n, err := io.Copy(tw, f)
	if err != nil {
		return err
	}
	if n != fi.Size() {
		// The header already said Size, so a short read would make the archive unreadable.
		return fmt.Errorf("%s: read %d bytes of %d", name, n, fi.Size())
	}
	return nil
}

func toSlash(name string) string { return strings.ReplaceAll(name, string(filepath.Separator), "/") }
