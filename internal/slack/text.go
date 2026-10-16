// Package slack is loopd's Slack layer (docs/phases/03-slack.md): a small Web API client, a Socket
// Mode client, and the bot that turns pipeline events into item threads and Slack actions into
// pipeline calls. Only the approver's actions count; everyone else is refused and recorded.
package slack

import (
	"regexp"
	"sort"
	"strconv"
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

// refTitleMax is the most characters of an item's title that ref keeps.
const refTitleMax = 80

// ref names an item as "#N <title>" for a ping or a one-line status reply, so a push notification
// says which item it is. The title is cut to refTitleMax characters with a plain "…" (Clip's
// "full text on loopd" hint is too long for a notification), then escaped.
func ref(id int64, title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return "#" + strconv.FormatInt(id, 10)
	}
	if r := []rune(title); len(r) > refTitleMax {
		title = strings.TrimRight(string(r[:refTitleMax-1]), " \n") + "…"
	}
	return "#" + strconv.FormatInt(id, 10) + " " + Escape(title)
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
// anywhere; or by an alias (docs/phases/06b-all-repos.md) only as a leading "[<alias>]" or
// "<alias>:", or a trailing "in <alias>" (nothing but priority words after it), never in the middle
// of the sentence. Aliases are the configured ones (alias -> slug: "mobile", "node app") and each
// repo's name without its "jarvis-", "jarvis-cmd-" or "jarvis-device-" prefix ("nest",
// "docs"), when only one repo has it; such a derived alias counts only as a leading "[x]" or "x:".
// It returns the repo's slug and the text without those words, or "" and the text as it was. The
// first marker wins. A name two configured repos share counts only as a full slug.
func ParseRepo(text string, repos []string, aliases map[string]string) (repo, rest string) {
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
	explicit := map[string]string{} // alias -> slug
	for a, slug := range aliases {
		a = normAlias(a)
		if a == "" || slug == "" {
			continue
		}
		if _, isName := names[a]; isName {
			continue // a name or slug means its own repo
		}
		explicit[a] = slug
	}
	derived := map[string]string{} // short alias -> slug ("" when ambiguous)
	for _, slug := range repos {
		_, name, _ := strings.Cut(strings.ToLower(slug), "/")
		for _, prefix := range []string{"jarvis-cmd-", "jarvis-device-", "jarvis-"} {
			if short, ok := strings.CutPrefix(name, prefix); ok && short != "" {
				if prev, seen := derived[short]; seen && prev != slug {
					derived[short] = ""
				} else {
					derived[short] = slug
				}
				break
			}
		}
	}
	for a := range derived {
		if _, isName := names[a]; isName {
			delete(derived, a)
		} else if _, isAlias := explicit[a]; isAlias {
			delete(derived, a)
		}
	}

	type hit struct {
		start, end int
		slug       string
	}
	var best *hit
	consider := func(h hit) {
		if h.slug != "" && (best == nil || h.start < best.start) {
			best = &h
		}
	}
	lookup := func(m map[string]string, word string) string {
		return m[normAlias(word)]
	}

	if group := altGroup(names); group != "" {
		const edge = `(?:$|[^\w/'-])`
		for _, re := range []*regexp.Regexp{
			regexp.MustCompile(`(?i)(\[\s*` + group + `\s*\])`),
			regexp.MustCompile(`(?i)^\s*(` + group + `\s*:)`),
			regexp.MustCompile(`(?i)(?:^|[^\w/'-])(in\s+` + group + `)` + edge),
		} {
			if m := re.FindStringSubmatchIndex(text); m != nil {
				consider(hit{m[2], m[3], lookup(names, text[m[4]:m[5]])})
			}
		}
	}
	leading := map[string]string{}
	for a, slug := range explicit {
		leading[a] = slug
	}
	for a, slug := range derived {
		leading[a] = slug
	}
	if group := altGroup(leading); group != "" {
		for _, re := range []*regexp.Regexp{
			regexp.MustCompile(`(?i)^\s*(\[\s*` + group + `\s*\])`),
			regexp.MustCompile(`(?i)^\s*(` + group + `\s*:)`),
		} {
			if m := re.FindStringSubmatchIndex(text); m != nil {
				consider(hit{m[2], m[3], lookup(leading, text[m[4]:m[5]])})
			}
		}
	}
	if group := altGroup(explicit); group != "" {
		re := regexp.MustCompile(`(?i)(?:^|[^\w/'-])(in\s+` + group + `)(?:$|[^\w/'-])`)
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			// Trailing: nothing but punctuation and priority words after it.
			if _, after := ParsePriority(text[m[3]:]); strings.Trim(after, " .!?,;:-–—") == "" {
				consider(hit{m[2], m[3], lookup(explicit, text[m[4]:m[5]])})
			}
		}
	}
	if best == nil {
		return "", text
	}
	out := text[:best.start] + " " + text[best.end:]
	out = strings.Join(strings.Fields(out), " ")
	out = strings.Replace(out, " ,", ",", -1)
	if strings.HasSuffix(out, " .") || strings.HasSuffix(out, " !") || strings.HasSuffix(out, " ?") {
		out = out[:len(out)-2] + out[len(out)-1:]
	}
	out = strings.Trim(out, " ,:;-–—")
	return best.slug, out
}

// normAlias is an alias as matched: lower case, single spaces.
func normAlias(a string) string { return strings.Join(strings.Fields(strings.ToLower(a)), " ") }

// altGroup is a regexp group of the keys with a slug, longest first (a slug wins over its own
// short name), spaces matching any run of whitespace; "" when there are none.
func altGroup(m map[string]string) string {
	var alts []string
	for n, slug := range m {
		if slug != "" && n != "" {
			alts = append(alts, n)
		}
	}
	if len(alts) == 0 {
		return ""
	}
	sort.Slice(alts, func(i, j int) bool {
		return len(alts[i]) > len(alts[j]) || len(alts[i]) == len(alts[j]) && alts[i] < alts[j]
	})
	for i, a := range alts {
		alts[i] = strings.ReplaceAll(regexp.QuoteMeta(a), " ", `\s+`)
	}
	return "(" + strings.Join(alts, "|") + ")"
}

var afterMarker = regexp.MustCompile(`(?i)\bafter\s+(#\d+(?:\s*(?:,|and|&)\s*#\d+)*)\s*:?`)
var itemRef = regexp.MustCompile(`#(\d+)`)

// ParseAfter reads the dependencies of an add request, "after #12" or "after #12, #13" (also "and"),
// and returns them with the text without those words. The "#" is required, so "retry after 5
// minutes" stays as it is.
func ParseAfter(text string) (after []int64, rest string) {
	m := afterMarker.FindStringSubmatchIndex(text)
	if m == nil {
		return nil, text
	}
	for _, r := range itemRef.FindAllStringSubmatch(text[m[2]:m[3]], -1) {
		id, err := strconv.ParseInt(r[1], 10, 64)
		if err != nil {
			return nil, text
		}
		after = append(after, id)
	}
	out := text[:m[0]] + " " + text[m[1]:]
	out = emptyParens.ReplaceAllString(out, " ")
	out = strings.Join(strings.Fields(out), " ")
	out = strings.ReplaceAll(out, " ,", ",")
	out = strings.Trim(out, " ,:;-–—")
	return after, out
}
