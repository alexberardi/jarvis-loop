package loopd

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/alexberardi/jarvis-loop/internal/slack"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

var slugRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// RepoSource is where loopd's repos come from: --repos (Flag, already parsed) and --repos-file
// (File, "" for none), which loopd reads at start and again on SIGHUP.
type RepoSource struct {
	Flag []string
	File string
}

// Load reads the repos and aliases: Flag, then the file's repos not already in it.
func (rs RepoSource) Load() ([]string, map[string]string, error) {
	repos := append([]string(nil), rs.Flag...)
	if rs.File == "" {
		return repos, nil, nil
	}
	b, err := os.ReadFile(rs.File)
	if err != nil {
		return nil, nil, fmt.Errorf("--repos-file: %w", err)
	}
	more, aliases, err := ParseReposFile(string(b))
	if err != nil {
		return nil, nil, fmt.Errorf("--repos-file %s: %w", rs.File, err)
	}
	for _, r := range more {
		if !slices.Contains(repos, r) {
			repos = append(repos, r)
		}
	}
	return repos, aliases, nil
}

// ReloadRepos reads the repos again and gives them to the store (triage's choices, the CLI's
// --repo) and the Slack bot (nil when Slack is off). An invalid file is refused: the repos stay
// as they were.
func ReloadRepos(rs RepoSource, st *store.Store, bot *slack.Bot, log *slog.Logger) error {
	repos, aliases, err := rs.Load()
	if err != nil {
		log.Error("repos reload refused: the repos stay as they were", "err", err)
		return err
	}
	st.SetRepos(repos)
	st.SetAliases(aliases)
	if bot != nil {
		bot.SetRepos(repos, aliases)
	}
	log.Info("repos reloaded", "repos", repos, "aliases", len(aliases))
	return nil
}

// ParseReposFile reads --repos-file: one owner/name slug per line, optionally followed by ':' and
// comma-separated aliases; blank lines and '#' comments are skipped. An alias may name one repo
// only, and never another repo's slug or name.
func ParseReposFile(text string) ([]string, map[string]string, error) {
	var repos []string
	aliases := map[string]string{}
	for n, line := range strings.Split(text, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		slug, rest, _ := strings.Cut(line, ":")
		slug = strings.TrimSpace(slug)
		if !slugRe.MatchString(slug) {
			return nil, nil, fmt.Errorf("line %d: %q is not an owner/name slug", n+1, slug)
		}
		if !slices.Contains(repos, slug) {
			repos = append(repos, slug)
		}
		for _, a := range strings.Split(rest, ",") {
			a = strings.Join(strings.Fields(strings.ToLower(a)), " ")
			if a == "" {
				continue
			}
			if strings.ContainsAny(a, "[]:/") {
				return nil, nil, fmt.Errorf("line %d: alias %q may not contain [ ] : or /", n+1, a)
			}
			if prev, ok := aliases[a]; ok && prev != slug {
				return nil, nil, fmt.Errorf("line %d: alias %q names both %s and %s", n+1, a, prev, slug)
			}
			aliases[a] = slug
		}
	}
	for a := range aliases {
		for _, r := range repos {
			if _, name, _ := strings.Cut(strings.ToLower(r), "/"); a == name {
				return nil, nil, fmt.Errorf("alias %q is the name of %s", a, r)
			}
		}
	}
	return repos, aliases, nil
}
