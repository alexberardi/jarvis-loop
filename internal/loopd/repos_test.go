package loopd

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// --repos-file: the long list of phase 6b, one slug per line with optional aliases.
func TestParseReposFile(t *testing.T) {
	repos, aliases, err := ParseReposFile(`# the loop's repos
alexberardi/jarvis-server: server
alexberardi/jarvis-node-mobile: Mobile, node  app   # the household's app

alexberardi/jarvis-recipes-mobile: recipes app
alexberardi/jarvis-device-nest
alexberardi/jarvis-server
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alexberardi/jarvis-server", "alexberardi/jarvis-node-mobile", "alexberardi/jarvis-recipes-mobile", "alexberardi/jarvis-device-nest"}
	if strings.Join(repos, ",") != strings.Join(want, ",") {
		t.Fatalf("repos %v", repos)
	}
	if fmt.Sprint(aliases) != "map[mobile:alexberardi/jarvis-node-mobile node app:alexberardi/jarvis-node-mobile recipes app:alexberardi/jarvis-recipes-mobile server:alexberardi/jarvis-server]" {
		t.Fatalf("aliases %v", aliases)
	}
	for name, text := range map[string]string{
		"not a slug":         "jarvis-server: server",
		"alias on two repos": "a/one: x\na/two: x",
		"alias with a slash": "a/one: b/c",
		"alias is a name":    "a/one: two\na/two",
	} {
		if _, _, err := ParseReposFile(text); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// SIGHUP reads --repos-file again: a new repo and alias resolve at once; an invalid file is
// refused and the repos stay.
func TestReloadRepos(t *testing.T) {
	st, err := store.Open(t.TempDir(), clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	file := filepath.Join(t.TempDir(), "repos")
	os.WriteFile(file, []byte("alexberardi/jarvis-server: server\n"), 0o644)
	rs := RepoSource{Flag: []string{"alexberardi/jarvis-loop"}, File: file}
	log := slog.New(slog.DiscardHandler)
	if err := ReloadRepos(rs, st, nil, log); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ResolveRepo("recipes app"); err == nil {
		t.Fatal("an alias not in the file resolved")
	}
	os.WriteFile(file, []byte("alexberardi/jarvis-server: server\nalexberardi/jarvis-recipes-mobile: recipes app\n"), 0o644)
	if err := ReloadRepos(rs, st, nil, log); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ResolveRepo("recipes app"); err != nil || got != "alexberardi/jarvis-recipes-mobile" {
		t.Fatalf("the new alias: %q %v", got, err)
	}
	want := []string{"alexberardi/jarvis-loop", "alexberardi/jarvis-server", "alexberardi/jarvis-recipes-mobile"}
	if !slices.Equal(st.Repos(), want) {
		t.Fatalf("repos %v", st.Repos())
	}
	os.WriteFile(file, []byte("not a slug\n"), 0o644)
	if err := ReloadRepos(rs, st, nil, log); err == nil {
		t.Fatal("an invalid file was taken")
	}
	if !slices.Equal(st.Repos(), want) {
		t.Fatalf("a refused reload changed the repos: %v", st.Repos())
	}
	if _, err := st.ResolveRepo("recipes app"); err != nil {
		t.Fatalf("a refused reload dropped an alias: %v", err)
	}
}
