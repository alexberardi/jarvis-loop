package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// tgz builds a gzipped tar from name -> body; a name ending in "/" is a directory. mode is what
// the header claims, which the unpack must not take.
func tgz(t *testing.T, mode int64, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		h := &tar.Header{Name: name, Mode: mode, Typeflag: tar.TypeReg, Size: int64(len(body))}
		if strings.HasSuffix(name, "/") {
			h = &tar.Header{Name: name, Mode: mode, Typeflag: tar.TypeDir}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeArchive(t *testing.T, dir string, b []byte) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, restoreName)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The data move: an archive on an empty volume is unpacked at 0600/0700 whatever its headers say,
// and renamed aside so the next start does not do it again
// (docs/decisions/2026-10-23-loopd-on-fly.md).
func TestRestoreUnpacksOntoAnEmptyVolume(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	writeArchive(t, dir, tgz(t, 0o666, map[string]string{
		"loop.db":                 "a database",
		"evidence/":               "",
		"evidence/1/after.png":    "not a png",
		"regressions/7/report.md": "a report",
	}))
	if err := restore(dir); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"loop.db":                 "a database",
		"evidence/1/after.png":    "not a png",
		"regressions/7/report.md": "a report",
	} {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(b) != want {
			t.Errorf("%s: %q", name, b)
		}
		if runtime.GOOS != "windows" {
			fi, _ := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("%s: mode %v, want 0600", name, fi.Mode().Perm())
			}
		}
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, "evidence"))
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("evidence/: mode %v, want 0700", fi.Mode().Perm())
		}
	}
	// The archive is renamed aside, not deleted: it is the one copy of the data on this machine.
	if _, err := os.Stat(filepath.Join(dir, restoreName)); !os.IsNotExist(err) {
		t.Errorf("the archive is still %s: %v", restoreName, err)
	}
	done, err := filepath.Glob(filepath.Join(dir, restoreName+".done-*"))
	if err != nil || len(done) != 1 {
		t.Fatalf("renamed to %v, %v", done, err)
	}
	// A second start does nothing, and does not trip over the database it just unpacked.
	if err := restore(dir); err != nil {
		t.Fatalf("second start: %v", err)
	}
}

// `tar czf … -C dir .`, which the runbook's step 5 tells the user to run, names everything
// "./loop.db": the same archive as GET /backup's plain names, written by a different tar.
func TestRestoreAcceptsDotSlashNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	writeArchive(t, dir, tgz(t, 0o600, map[string]string{
		"./":                     "",
		"./loop.db":              "a database",
		"./evidence/1/after.png": "not a png",
	}))
	if err := restore(dir); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"loop.db": "a database", "evidence/1/after.png": "not a png"} {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil || string(b) != want {
			t.Errorf("%s: %q %v", name, b, err)
		}
	}
	// And a second start sees the database it unpacked and leaves it alone.
	if err := restore(dir); err != nil {
		t.Fatalf("second start: %v", err)
	}
}

// Nothing to restore: the usual start.
func TestRestoreWithoutAnArchive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := restore(dir); err != nil {
		t.Fatal(err)
	}
	if err := restore(filepath.Join(t.TempDir(), "not-there")); err != nil {
		t.Fatal(err)
	}
}

// It never unpacks over a database that is already there: that is the one mistake it must not make
// quietly, so it stops the machine instead and says what to do.
func TestRestoreRefusesToOverwriteADatabase(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	writeArchive(t, dir, tgz(t, 0o600, map[string]string{"loop.db": "the archive's database"}))
	live := filepath.Join(dir, "loop.db")
	if err := os.WriteFile(live, []byte("the live database"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := restore(dir)
	if err == nil || !strings.Contains(err.Error(), "loop.db already exists") {
		t.Fatalf("error %v", err)
	}
	if b, _ := os.ReadFile(live); string(b) != "the live database" {
		t.Errorf("loop.db is now %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, restoreName)); err != nil {
		t.Errorf("the archive was moved: %v", err)
	}
}

// An archive that would write outside the data directory, or hold anything but files and
// directories, is refused whole: nothing is unpacked before every name has been read.
func TestRestoreRefusesABadArchive(t *testing.T) {
	cases := map[string]map[string]string{
		"an absolute path": {"loop.db": "x", "/etc/passwd": "root::0:0"},
		"a parent":         {"loop.db": "x", "../escaped": "out"},
		"a deeper parent":  {"loop.db": "x", "evidence/../../escaped": "out"},
		"empty":            {},
	}
	for name, entries := range cases {
		dir := filepath.Join(t.TempDir(), "data")
		writeArchive(t, dir, tgz(t, 0o600, entries))
		if err := restore(dir); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := os.Stat(filepath.Join(dir, "loop.db")); !os.IsNotExist(err) {
			t.Errorf("%s: unpacked loop.db before refusing: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escaped")); !os.IsNotExist(err) {
			t.Errorf("%s: wrote outside the data directory", name)
		}
	}

	// A symlink entry, which a tar can carry and this must not follow.
	dir := filepath.Join(t.TempDir(), "data")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "loop.db", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o600})
	tw.Close()
	gz.Close()
	writeArchive(t, dir, buf.Bytes())
	if err := restore(dir); err == nil || !strings.Contains(err.Error(), "only files and directories") {
		t.Errorf("a symlink entry: %v", err)
	}

	// Not an archive at all.
	dir = filepath.Join(t.TempDir(), "data")
	writeArchive(t, dir, []byte("this is not gzip"))
	if err := restore(dir); err == nil || !strings.Contains(err.Error(), "not gzip") {
		t.Errorf("not gzip: %v", err)
	}
}

// dataDir reads --data off loopd's command line, in flag's own forms.
func TestDataDir(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--addr", "0.0.0.0:7780", "--lan", "--data", "/data"}, "/data"},
		{[]string{"-data", "/data", "--lan"}, "/data"},
		{[]string{"--data=/data"}, "/data"},
		{[]string{"-data=/data"}, "/data"},
		{[]string{"--lan"}, ""},
		{[]string{"--lan", "--data"}, ""}, // no value: loopd will refuse it anyway
		{nil, ""},
	} {
		if got := dataDir(c.args); got != c.want {
			t.Errorf("dataDir(%v) = %q, want %q", c.args, got, c.want)
		}
	}
}
