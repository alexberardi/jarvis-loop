package web

import (
	"html"
	"html/template"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// markdown renders a plan's brief or blueprint for the board (docs/phases/07-planning.md, "The
// board"). It is the page's only trust boundary, so it is an allow-list, not a sanitizer: every
// character of the input is escaped, and the only markup that comes out is what it writes itself.
// Headings, paragraphs, lists (nested), GFM pipe tables, fenced code, inline code, emphasis, links,
// block quotes and rules. Raw HTML is text; images are their alt text (the CSP serves images from
// the board only); a link is a link only to http, https or a relative address.
func markdown(src string) template.HTML {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	var b strings.Builder
	blocks(&b, strings.Split(src, "\n"), false)
	return template.HTML(b.String())
}

var (
	atxHeading = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$`)
	fence      = regexp.MustCompile("^ {0,3}(```+|~~~+)")
	rule       = regexp.MustCompile(`^ {0,3}(?:(?:\*[ \t]*){3,}|(?:-[ \t]*){3,}|(?:_[ \t]*){3,})$`)
	listItem   = regexp.MustCompile(`^( *)([-*+]|\d{1,9}[.)])(?:[ \t]+(.*))?$`)
	quoteLine  = regexp.MustCompile(`^ {0,3}> ?(.*)$`)
	delimCell  = regexp.MustCompile(`^[ \t]*:?-+:?[ \t]*$`)
)

func blank(l string) bool { return strings.TrimSpace(l) == "" }

// startsBlock is whether a line begins a block other than a paragraph (so it ends one).
func startsBlock(l string) bool {
	return atxHeading.MatchString(l) || fence.MatchString(l) || rule.MatchString(l) ||
		listItem.MatchString(l) || quoteLine.MatchString(l)
}

// blocks renders lines as block elements. tight leaves a paragraph's <p> out (a list item's text).
func blocks(b *strings.Builder, lines []string, tight bool) {
	for i := 0; i < len(lines); {
		l := lines[i]
		switch {
		case blank(l):
			i++
		case fence.MatchString(l):
			mark := fence.FindStringSubmatch(l)[1]
			b.WriteString("<pre><code>")
			for i++; i < len(lines); i++ {
				if c := strings.TrimSpace(lines[i]); strings.HasPrefix(c, mark[:3]) && strings.Trim(c, mark[:1]) == "" && len(c) >= len(mark) {
					i++
					break
				}
				b.WriteString(html.EscapeString(lines[i]) + "\n")
			}
			b.WriteString("</code></pre>\n")
		case atxHeading.MatchString(l):
			m := atxHeading.FindStringSubmatch(l)
			n := strconv.Itoa(len(m[1]))
			b.WriteString("<h" + n + ">" + inline(strings.TrimSpace(m[2]), true) + "</h" + n + ">\n")
			i++
		case rule.MatchString(l):
			b.WriteString("<hr>\n")
			i++
		case quoteLine.MatchString(l):
			var inner []string
			for ; i < len(lines) && quoteLine.MatchString(lines[i]); i++ {
				inner = append(inner, quoteLine.FindStringSubmatch(lines[i])[1])
			}
			b.WriteString("<blockquote>\n")
			blocks(b, inner, false)
			b.WriteString("</blockquote>\n")
		case listItem.MatchString(l):
			i = list(b, lines, i)
		case i+1 < len(lines) && strings.Contains(l, "|") && delimiterRow(lines[i+1], len(cells(l))):
			i = table(b, lines, i)
		default:
			start := i
			for i++; i < len(lines) && !blank(lines[i]) && !startsBlock(lines[i]); i++ {
			}
			var text []string
			for _, p := range lines[start:i] {
				text = append(text, strings.TrimSpace(p))
			}
			body := inline(strings.Join(text, "\n"), true)
			if tight {
				b.WriteString(body + "\n")
			} else {
				b.WriteString("<p>" + body + "</p>\n")
			}
		}
	}
}

// list renders the list starting at lines[i] and returns the index after it. Its items are the
// lines with the same indent and the same kind of marker; a line indented further, or a lazy line
// of text, belongs to the item above it.
func list(b *strings.Builder, lines []string, i int) int {
	m := listItem.FindStringSubmatch(lines[i])
	indent, marker := len(m[1]), m[2]
	ordered := marker[0] >= '0' && marker[0] <= '9'
	kind := marker[len(marker)-1:]
	tag := "ul"
	if ordered {
		tag = "ol"
		if n, _ := strconv.Atoi(marker[:len(marker)-1]); n != 1 {
			b.WriteString(`<ol start="` + strconv.Itoa(n) + `">` + "\n")
		} else {
			b.WriteString("<ol>\n")
		}
	} else {
		b.WriteString("<ul>\n")
	}
	for i < len(lines) {
		m := listItem.FindStringSubmatch(lines[i])
		if m == nil || len(m[1]) != indent || m[2][len(m[2])-1:] != kind || (m[2][0] >= '0' && m[2][0] <= '9') != ordered {
			break
		}
		offset := len(m[1]) + len(m[2]) + 1
		item := []string{m[3]}
		for i++; i < len(lines); i++ {
			l := lines[i]
			if blank(l) {
				// A blank line ends the item unless the next line is indented under it.
				if i+1 < len(lines) && indentOf(lines[i+1]) > indent && !blank(lines[i+1]) {
					item = append(item, "")
					continue
				}
				break
			}
			if indentOf(l) > indent {
				item = append(item, l[min(indentOf(l), offset):])
				continue
			}
			if startsBlock(l) {
				break
			}
			item = append(item, l)
		}
		var inner strings.Builder
		blocks(&inner, item, true)
		b.WriteString("<li>" + strings.TrimSuffix(inner.String(), "\n") + "</li>\n")
		// A blank line between items keeps the list going.
		if i+1 < len(lines) && blank(lines[i]) {
			if n := listItem.FindStringSubmatch(lines[i+1]); n != nil && len(n[1]) == indent {
				i++
			}
		}
	}
	b.WriteString("</" + tag + ">\n")
	return i
}

func indentOf(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }

// cells splits a table row on the pipes that are not escaped.
func cells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	if strings.HasSuffix(row, "|") && !strings.HasSuffix(row, `\|`) {
		row = row[:len(row)-1]
	}
	var out []string
	var cur strings.Builder
	for i := 0; i < len(row); i++ {
		switch {
		case row[i] == '\\' && i+1 < len(row) && row[i+1] == '|':
			cur.WriteByte('|')
			i++
		case row[i] == '|':
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(row[i])
		}
	}
	return append(out, strings.TrimSpace(cur.String()))
}

func delimiterRow(l string, n int) bool {
	if !strings.Contains(l, "-") {
		return false
	}
	cs := cells(l)
	if len(cs) != n {
		return false
	}
	for _, c := range cs {
		if !delimCell.MatchString(c) {
			return false
		}
	}
	return true
}

// table renders the GFM table at lines[i] (its header and delimiter row checked) and returns the
// index after it. Rows have the header's number of cells; alignment is not kept.
func table(b *strings.Builder, lines []string, i int) int {
	head := cells(lines[i])
	row := func(tag string, cs []string) {
		b.WriteString("<tr>")
		for k := range head {
			c := ""
			if k < len(cs) {
				c = cs[k]
			}
			b.WriteString("<" + tag + ">" + inline(c, true) + "</" + tag + ">")
		}
		b.WriteString("</tr>\n")
	}
	b.WriteString("<table>\n<thead>\n")
	row("th", head)
	b.WriteString("</thead>\n<tbody>\n")
	for i += 2; i < len(lines) && !blank(lines[i]) && strings.Contains(lines[i], "|"); i++ {
		row("td", cells(lines[i]))
	}
	b.WriteString("</tbody>\n</table>\n")
	return i
}

func isPunct(c byte) bool {
	return c < 0x80 && unicode.IsPunct(rune(c)) || strings.IndexByte("$+<=>^`|~", c) >= 0
}

func wordByte(c byte) bool { return c >= 0x80 || unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)) }

// inline renders a span of text: code, emphasis, links, escapes. Everything else is escaped text.
func inline(s string, links bool) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && isPunct(s[i+1]):
			b.WriteString(html.EscapeString(s[i+1 : i+2]))
			i += 2
			continue
		case c == '`':
			n := run(s, i, '`')
			if end := closingRun(s, i+n, '`', n); end >= 0 {
				code := s[i+n : end]
				if len(code) > 1 && code[0] == ' ' && code[len(code)-1] == ' ' && strings.Trim(code, " ") != "" {
					code = code[1 : len(code)-1]
				}
				b.WriteString("<code>" + html.EscapeString(code) + "</code>")
				i = end + n
			} else {
				b.WriteString(s[i : i+n])
				i += n
			}
			continue
		case c == '*' || c == '_':
			if out, next, ok := emphasis(s, i, links); ok {
				b.WriteString(out)
				i = next
				continue
			}
			n := run(s, i, c)
			b.WriteString(s[i : i+n])
			i += n
			continue
		case c == '!' && i+1 < len(s) && s[i+1] == '[':
			if text, _, next, ok := link(s, i+1); ok {
				b.WriteString(html.EscapeString(text))
				i = next
				continue
			}
		case c == '[' && links:
			if text, url, next, ok := link(s, i); ok {
				if safeURL(url) {
					b.WriteString(`<a href="` + html.EscapeString(url) + `" rel="noopener noreferrer">` + inline(text, false) + "</a>")
				} else {
					b.WriteString(inline(text, false))
				}
				i = next
				continue
			}
		}
		b.WriteString(html.EscapeString(s[i : i+1]))
		i++
	}
	return b.String()
}

// run is the length of the run of c at s[i].
func run(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}
	return n
}

// closingRun finds a run of exactly n c's at or after from.
func closingRun(s string, from int, c byte, n int) int {
	for j := from; j < len(s); {
		if s[j] != c {
			j++
			continue
		}
		k := run(s, j, c)
		if k == n {
			return j
		}
		j += k
	}
	return -1
}

// emphasis renders *em*, **strong** (or with _) starting at s[i], if it closes.
func emphasis(s string, i int, links bool) (string, int, bool) {
	c := s[i]
	d := min(run(s, i, c), 2)
	open := i + d
	if open >= len(s) || unicode.IsSpace(rune(s[open])) || (c == '_' && i > 0 && wordByte(s[i-1])) {
		return "", 0, false
	}
	for j := open + 1; j+d <= len(s); j++ {
		if s[j] != c {
			continue
		}
		k := run(s, j, c)
		if k < d || (d == 1 && k > 1) || unicode.IsSpace(rune(s[j-1])) ||
			(c == '_' && j+k < len(s) && wordByte(s[j+k])) {
			j += k - 1
			continue
		}
		tag := "em"
		if d == 2 {
			tag = "strong"
		}
		return "<" + tag + ">" + inline(s[open:j], links) + "</" + tag + ">", j + d, true
	}
	return "", 0, false
}

// link parses [text](url "title") at s[i] (the '['); the title is dropped.
func link(s string, i int) (text, url string, next int, ok bool) {
	depth := 0
	j := i
	for ; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '`':
			if end := closingRun(s, j+run(s, j, '`'), '`', run(s, j, '`')); end >= 0 {
				j = end + run(s, end, '`') - 1
			}
		case '[':
			depth++
		case ']':
			depth--
		}
		if depth == 0 {
			break
		}
	}
	if j >= len(s) || j+1 >= len(s) || s[j+1] != '(' {
		return "", "", 0, false
	}
	text = s[i+1 : j]
	k := j + 2
	for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
		k++
	}
	start, parens := k, 0
	for ; k < len(s); k++ {
		ch := s[k]
		if ch == ' ' || ch == '\t' || ch == '\n' || (ch == ')' && parens == 0) {
			break
		}
		if ch == '(' {
			parens++
		} else if ch == ')' {
			parens--
		}
	}
	url = s[start:k]
	for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n') {
		k++
	}
	if k < len(s) && (s[k] == '"' || s[k] == '\'') {
		end := strings.IndexByte(s[k+1:], s[k])
		if end < 0 {
			return "", "", 0, false
		}
		k += end + 2
		for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
			k++
		}
	}
	if k >= len(s) || s[k] != ')' {
		return "", "", 0, false
	}
	return text, url, k + 1, true
}

// safeURL allows http, https and relative addresses. Anything with a colon before its first
// slash must be http or https, so no scheme a browser would run (javascript:, data:, entity- or
// case-disguised ones) gets through; control characters are refused outright.
func safeURL(u string) bool {
	if u == "" {
		return false
	}
	for _, r := range u {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	before, _, _ := strings.Cut(u, "/")
	scheme, _, hasColon := strings.Cut(before, ":")
	if !hasColon {
		return true
	}
	scheme = strings.ToLower(scheme)
	return scheme == "http" || scheme == "https"
}
