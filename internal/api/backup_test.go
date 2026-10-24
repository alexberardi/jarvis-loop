package api

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"
)

// GET /backup is the nightly copy off the machine (docs/decisions/2026-10-23-loopd-on-fly.md):
// the user's token only, because it hands over the whole database and every uploaded file at once.
func TestBackupRoute(t *testing.T) {
	e := newEnv(t)
	var it struct{ ID int64 }
	e.do(userTok, "POST", "/items", map[string]string{"title": "in the backup", "priority": "P2", "ask": "something to find in the archive"}, 201, &it)

	req, _ := http.NewRequest("GET", e.srv.URL+"/backup", nil)
	req.Header.Set("Authorization", "Bearer "+userTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /backup: %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "loop-backup-") || !strings.HasSuffix(cd, `.tar.gz"`) {
		t.Errorf("Content-Disposition %q", cd)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q", cc)
	}
	// The body is a readable archive with the database in it.
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		names = append(names, h.Name)
		if _, err := io.Copy(io.Discard, tr); err != nil {
			t.Fatalf("tar %s: %v", h.Name, err)
		}
	}
	found := false
	for _, n := range names {
		if n == "loop.db" {
			found = true
		}
		if strings.HasSuffix(n, ".db-wal") || strings.HasSuffix(n, ".db-shm") {
			t.Errorf("the archive holds %s", n)
		}
	}
	if !found {
		t.Errorf("no loop.db in the archive: %v", names)
	}

	// A runner's and the orchestrator's tokens cannot have it, and no token is unauthorized.
	e.refused(runnerTok, "GET", "/backup", nil, 403, CodeForbidden)
	e.refused(orchTok, "GET", "/backup", nil, 403, CodeForbidden)
	e.do("", "GET", "/backup", nil, 401, nil)
}
