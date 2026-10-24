package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// restoreName is the archive the entrypoint unpacks into the data directory, in the format
// GET /backup produces (docs/decisions/2026-10-23-loopd-on-fly.md): loop.db at the top, plus
// evidence/ and regressions/. It is how the data gets onto an empty volume, since `FROM scratch`
// leaves nothing on the machine that could untar one, and how a restore from a nightly backup
// works. docs/runbooks/loopd-on-fly.md has both.
const restoreName = "restore.tar.gz"

// restore unpacks <dir>/restore.tar.gz into dir and renames it aside, so the next start does not
// do it again. It refuses when dir already holds a database: unpacking over a live loop.db is the
// one mistake this must not make quietly, so a deliberate restore deletes it first.
//
// It writes nothing outside dir: an entry naming an absolute path, a parent, or anything but a
// regular file or a directory is an error, and the whole archive is refused before any of it is
// unpacked.
func restore(dir string) error {
	archive := filepath.Join(dir, restoreName)
	if _, err := os.Stat(archive); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "loop.db")); err == nil {
		return fmt.Errorf("%s is here but %s/loop.db already exists: move the archive away, or "+
			"delete loop.db, loop.db-wal and loop.db-shm first if you mean to replace them", archive, dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Read it once to check every name before unpacking any of it.
	names, err := checkArchive(archive)
	if err != nil {
		return fmt.Errorf("%s: %w", archive, err)
	}
	log.Printf("loop-entrypoint: unpacking %s (%d entries) into %s", restoreName, len(names), dir)
	if err := unpack(archive, dir); err != nil {
		return fmt.Errorf("%s: %w", archive, err)
	}
	// Renamed, not deleted: the one copy of the data on this machine is not this entrypoint's to
	// throw away, and the name says it has been used.
	done := archive + ".done-" + time.Now().UTC().Format("20060102T150405Z")
	if err := os.Rename(archive, done); err != nil {
		return err
	}
	log.Printf("loop-entrypoint: unpacked %s; the archive is now %s", restoreName, filepath.Base(done))
	return nil
}

// checkArchive reads the whole archive and returns its entry names, or the first reason it must
// not be unpacked.
func checkArchive(archive string) ([]string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("not gzip: %w", err)
	}
	defer gz.Close()
	var names []string
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if err := checkName(h.Name); err != nil {
			return nil, err
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeDir:
		default:
			return nil, fmt.Errorf("%s: only files and directories, not type %q", h.Name, h.Typeflag)
		}
		names = append(names, h.Name)
	}
	if len(names) == 0 {
		return nil, errors.New("the archive is empty")
	}
	return names, nil
}

// checkName refuses a name that would write outside the data directory.
func checkName(name string) error {
	if name == "" {
		return errors.New("an entry has no name")
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) {
		return fmt.Errorf("%s: an absolute path", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s: leaves the data directory", name)
	}
	return nil
}

// unpack writes the archive's entries under dir. Modes are this entrypoint's, not the archive's:
// 0700 for directories and 0600 for files, which is what store.Open wants anyway and what a file
// holding the loop's data should be whatever a tar header claims.
func unpack(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := checkName(h.Name); err != nil { // checked once already; cheap to mean it
			return err
		}
		path := filepath.Join(dir, filepath.FromSlash(h.Name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			if err := writeEntry(path, tr); err != nil {
				return fmt.Errorf("%s: %w", h.Name, err)
			}
		}
	}
}

func writeEntry(path string, r io.Reader) error {
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// dataDir is the --data loopd was given on the command line, or "" when it was not: the archive is
// looked for there and nowhere else. The forms are flag's own: --data X, -data X, --data=X, -data=X.
func dataDir(args []string) string {
	for i, a := range args {
		for _, name := range []string{"--data", "-data"} {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
			if v, ok := strings.CutPrefix(a, name+"="); ok {
				return v
			}
		}
	}
	return ""
}
