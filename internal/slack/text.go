// Package slack is loopd's Slack layer (docs/phases/03-slack.md): a small Web API client, a Socket
// Mode client, and the bot that turns pipeline events into item threads and Slack actions into
// pipeline calls. Only the approver's actions count; everyone else is refused and recorded.
package slack

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// MaxChars is the most characters any message the bot sends may have.
const MaxChars = 2000

// Escape makes text safe to put in a Slack message: user and agent text can never become a
// mention, a channel ping or a link.
func Escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// Clip cuts s to at most max characters, ending it with "… (full text on loopd[: hint])".
func Clip(s string, max int, hint string) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	suffix := "… (full text on loopd)"
	if hint != "" {
		suffix = "… (full text on loopd: " + hint + ")"
	}
	keep := max - utf8.RuneCountInString(suffix)
	if keep < 0 {
		keep = 0
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:keep]), " \n") + suffix
}

var mentionRe = regexp.MustCompile(`<[@!#][^>]*>`)

// stripMentions removes Slack's <@U…>, <!…> and <#C…> tokens from a message's text.
func stripMentions(s string) string {
	return strings.TrimSpace(mentionRe.ReplaceAllString(s, " "))
}

var wordRe = regexp.MustCompile(`[a-z0-9']+(?:-[a-z0-9']+)*`)

func words(s string) []string {
	s = strings.ToLower(strings.ReplaceAll(s, "’", "'"))
	return wordRe.FindAllString(s, -1)
}

var tierOf = map[string]string{
	"highest": "P1", "urgent": "P1", "high": "P2", "medium": "P3", "low": "P4", "lowest": "P5", "someday": "P5",
}

var priorityMarkers = []*regexp.Regexp{
	// "high priority", "low-priority", "medium prio"
	regexp.MustCompile(`(?i)\b(highest|high|medium|lowest|low)[- ]?(?:priority|prio|pri)\b`),
	// unambiguous anywhere
	regexp.MustCompile(`(?i)\b(urgent|someday)\b`),
	// "P2", "(p1)"
	regexp.MustCompile(`(?i)\b(p[1-5])\b`),
	// "low: rename the flag"
	regexp.MustCompile(`(?i)^\s*(highest|high|medium|lowest|low)\s*:`),
	// "rename the flag, low" / "timers drift - high"
	regexp.MustCompile(`(?i)\s*[,\-–—]\s*(highest|high|medium|lowest|low)\s*$`),
}

var emptyParens = regexp.MustCompile(`\(\s*\)|\[\s*\]`)

// ParsePriority reads a priority from the words of an add request and returns it with the title
// (the text without those words). A tier word counts when it is unambiguous (urgent, someday, P1–P5)
// or clearly about priority ("high priority", "low: …", "…, low"), so "fix the low battery
// warning" stays P3 with its title intact. The first marker wins; the default is P3.
func ParsePriority(text string) (priority, title string) {
	type hit struct {
		start, end int
		tier       string
	}
	var hits []hit
	for _, re := range priorityMarkers {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			w := strings.ToLower(text[m[2]:m[3]])
			tier, ok := tierOf[w]
			if !ok {
				tier = strings.ToUpper(w) // p1..p5
			}
			hits = append(hits, hit{m[0], m[1], tier})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].start < hits[j].start })
	priority = "P3"
	if len(hits) > 0 {
		priority = hits[0].tier
	}
	// Remove the markers, rightmost first so earlier offsets stay valid; skip overlaps.
	out := text
	end := len(text) + 1
	for i := len(hits) - 1; i >= 0; i-- {
		h := hits[i]
		if h.end > end {
			continue
		}
		out = out[:h.start] + " " + out[h.end:]
		end = h.start
	}
	out = emptyParens.ReplaceAllString(out, " ")
	out = strings.Join(strings.Fields(out), " ")
	out = strings.Trim(out, " ,:;-–—")
	return priority, out
}

// ParseRepo reads the repo words of an add request: a configured repo (repos are owner/name slugs)
// named by its name or slug as "in <name>" anywhere, "<name>:" at the start, or "[<name>]"
// anywhere. It returns the repo's slug and the text without those words, or "" and the text as it
// was. The first marker wins. A name two configured repos share counts only as a full slug.
func ParseRepo(text string, repos []string) (repo, rest string) {
	names := map[string]string{} // lower-case name or slug -> slug ("" when ambiguous)
	for _, slug := range repos {
		names[strings.ToLower(slug)] = slug
		if i := strings.LastIndex(slug, "/"); i >= 0 {
			short := strings.ToLower(slug[i+1:])
			if prev, seen := names[short]; seen && prev != slug {
				names[short] = ""
			} else {
				names[short] = slug
			}
		}
	}
	var alts []string
	for n, slug := range names {
		if slug != "" && n != "" {
			alts = append(alts, regexp.QuoteMeta(n))
		}
	}
	if len(alts) == 0 {
		return "", text
	}
	// Longest first, so a slug wins over its own short name.
	sort.Slice(alts, func(i, j int) bool {
		return len(alts[i]) > len(alts[j]) || len(alts[i]) == len(alts[j]) && alts[i] < alts[j]
	})
	group := "(" + strings.Join(alts, "|") + ")"
	const edge = `(?:$|[^\w/'-])`
	markers := []*regexp.Regexp{
		regexp.MustCompile(`(?i)(\[\s*` + group + `\s*\])`),
		regexp.MustCompile(`(?i)^\s*(` + group + `\s*:)`),
		regexp.MustCompile(`(?i)(?:^|[^\w/'-])(in\s+` + group + `)` + edge),
	}
	best, bestEnd, bestName := -1, -1, ""
	for _, re := range markers {
		m := re.FindStringSubmatchIndex(text)
		if m == nil {
			continue
		}
		if best < 0 || m[2] < best {
			best, bestEnd, bestName = m[2], m[3], strings.ToLower(text[m[4]:m[5]])
		}
	}
	if best < 0 {
		return "", text
	}
	out := text[:best] + " " + text[bestEnd:]
	out = strings.Join(strings.Fields(out), " ")
	out = strings.Replace(out, " ,", ",", -1)
	out = strings.Trim(out, " ,:;-–—")
	return names[bestName], out
}
