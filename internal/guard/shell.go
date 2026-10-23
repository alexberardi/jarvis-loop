package guard

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// parse splits a shell script into simple commands (each a list of words, quotes removed) and
// returns the text of every command substitution ($(...), `...`, <(...), >(...)) so the caller
// checks those too. It is deliberately small: it knows quotes, escapes, the control operators,
// redirections, comments and here-documents, which is what the guard needs to find the program
// and its arguments. Anything it cannot read (an unterminated quote or substitution) is an error,
// and the guard refuses the command.
func parse(s string) (segs [][]string, subs []string, err error) {
	segs, subs, err = parseRaw(s)
	for _, words := range segs {
		for i, w := range words {
			words[i] = unmark(w)
		}
	}
	return segs, subs, err
}

var markRe = regexp.MustCompile("\x00[0-9]+\x00")

// unmark replaces parseRaw's substitution markers with "$SUB".
func unmark(w string) string { return markRe.ReplaceAllString(w, "$$SUB") }

// marked reports whether w is exactly one substitution marker, and which substitution it is.
func marked(w string) (int, bool) {
	if len(w) < 3 || w[0] != 0 || w[len(w)-1] != 0 || !markRe.MatchString(w) || strings.Count(w, "\x00") != 2 {
		return 0, false
	}
	n, err := strconv.Atoi(w[1 : len(w)-1])
	return n, err == nil
}

// parseRaw is parse, but each command substitution in a word is a marker "\x00<n>\x00" naming
// subs[n], so a caller can tell which word came from which substitution.
func parseRaw(s string) (segs [][]string, subs []string, err error) {
	sc, err := parseScript(s)
	if err != nil {
		return nil, nil, err
	}
	for _, sg := range sc.segs {
		if len(sg.words) > 0 {
			segs = append(segs, sg.words)
		}
	}
	return segs, sc.subs, nil
}

// script is a parsed shell script: its simple commands, its command substitutions, and whether
// any command's output goes into a pipe.
type script struct {
	segs  []segment
	subs  []string
	piped bool
}

// segment is one simple command: its words (with substitution markers), the text its
// redirections name (files, here-strings), the targets of the redirections that write (>, >>,
// >|, >&, <> — here-strings and < are reads) and the bodies of its here-documents.
type segment struct {
	words   []string
	targets []string
	writes  []string
	bodies  []heredocBody
}

type heredocBody struct {
	text   string
	quoted bool // the delimiter was quoted, so the shell expands nothing in the body
}

// parseScript is parseRaw with each command's redirections and here-documents kept.
func parseScript(s string) (script, error) {
	var sc script
	if strings.IndexByte(s, 0) >= 0 {
		return sc, errors.New("a NUL byte")
	}
	var (
		subs     []string
		words    []string
		targets  []string
		writes   []string
		cur      strings.Builder
		inWord   bool
		skipTgt  bool // the next word is a redirection target
		tgtWrite bool // ... of a redirection that writes
		hasDoc   bool // the command being read has a here-document (so it is kept even without words)
		// pending here-documents (the body starts after the newline): the delimiter, and the
		// command whose input it is (an index into sc.segs)
		heredoc []string
		hdStrip []bool
		hdQuote []bool
		hdSeg   []int
	)
	endWord := func() {
		if !inWord {
			return
		}
		if skipTgt {
			skipTgt = false
			targets = append(targets, cur.String())
			if tgtWrite {
				writes = append(writes, cur.String())
			}
		} else {
			words = append(words, cur.String())
		}
		cur.Reset()
		inWord = false
	}
	endSeg := func() {
		endWord()
		if len(words) > 0 || len(targets) > 0 || hasDoc {
			sc.segs = append(sc.segs, segment{words: words, targets: targets, writes: writes})
		}
		words, targets, writes, hasDoc = nil, nil, nil, false
	}
	// closing finds the end of a $( ... ) starting after the "(" at i, honouring nesting and quotes.
	closing := func(i int) (int, error) {
		depth := 1
		var q byte
		for ; i < len(s); i++ {
			ch := s[i]
			switch {
			case q != 0:
				if ch == '\\' && q == '"' {
					i++
				} else if ch == q {
					q = 0
				}
			case ch == '\\':
				i++
			case ch == '\'' || ch == '"':
				q = ch
			case ch == '(':
				depth++
			case ch == ')':
				depth--
				if depth == 0 {
					return i, nil
				}
			}
		}
		return 0, errors.New("unterminated $(")
	}
	backtick := func(i int) (int, error) {
		for ; i < len(s); i++ {
			if s[i] == '\\' {
				i++
				continue
			}
			if s[i] == '`' {
				return i, nil
			}
		}
		return 0, errors.New("unterminated backquote")
	}
	// substitution handles $( and ` at i (in or out of double quotes); it returns the index of the
	// last character consumed.
	substitution := func(i int) (int, bool, error) {
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '(' {
			end, err := closing(i + 2)
			if err != nil {
				return 0, false, err
			}
			subs = append(subs, s[i+2:end])
			cur.WriteString("\x00" + strconv.Itoa(len(subs)-1) + "\x00")
			inWord = true
			return end, true, nil
		}
		if s[i] == '`' {
			end, err := backtick(i + 1)
			if err != nil {
				return 0, false, err
			}
			subs = append(subs, s[i+1:end])
			cur.WriteString("\x00" + strconv.Itoa(len(subs)-1) + "\x00")
			inWord = true
			return end, true, nil
		}
		return i, false, nil
	}

	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return sc, errors.New("unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+end])
			inWord = true
			i += end + 1
		case ch == '"':
			inWord = true
			closed := false
			for i++; i < len(s); i++ {
				c := s[i]
				if c == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`\n", s[i+1]) >= 0 {
					i++
					cur.WriteByte(s[i])
					continue
				}
				if c == '"' {
					closed = true
					break
				}
				if c == '$' || c == '`' {
					end, ok, err := substitution(i)
					if err != nil {
						return sc, err
					}
					if ok {
						i = end
						continue
					}
				}
				cur.WriteByte(c)
			}
			if !closed {
				return sc, errors.New("unterminated double quote")
			}
		case ch == '\\':
			if i+1 < len(s) {
				i++
				if s[i] != '\n' {
					cur.WriteByte(s[i])
					inWord = true
				}
			}
		case ch == '$' || ch == '`':
			end, ok, err := substitution(i)
			if err != nil {
				return sc, err
			}
			if ok {
				i = end
			} else {
				cur.WriteByte(ch)
				inWord = true
			}
		case ch == '#' && !inWord:
			for i < len(s) && s[i] != '\n' {
				i++
			}
			i--
		case ch == '\n':
			endSeg()
			// Take the bodies of here-documents opened on the line just ended: they are not words.
			for k, delim := range heredoc {
				var text strings.Builder
				for {
					j := strings.IndexByte(s[i+1:], '\n')
					line := s[i+1:]
					if j >= 0 {
						line = s[i+1 : i+1+j]
					}
					if hdStrip[k] {
						line = strings.TrimLeft(line, "\t")
					}
					if j < 0 {
						i = len(s)
						break
					}
					i += j + 1
					if line == delim {
						break
					}
					text.WriteString(line + "\n")
				}
				sg := &sc.segs[hdSeg[k]]
				sg.bodies = append(sg.bodies, heredocBody{text: text.String(), quoted: hdQuote[k]})
			}
			heredoc, hdStrip, hdQuote, hdSeg = nil, nil, nil, nil
		case ch == '|' && i+1 < len(s) && s[i+1] == '|':
			endSeg()
			i++
		case ch == ';' || ch == '&' || ch == '|' || ch == '(' || ch == ')':
			if ch == '|' {
				sc.piped = true
			}
			endSeg()
		case ch == '<' || ch == '>':
			// Process substitution: check the inner command.
			if i+1 < len(s) && s[i+1] == '(' {
				end, err := closing(i + 2)
				if err != nil {
					return sc, err
				}
				subs = append(subs, s[i+2:end])
				i = end
				continue
			}
			// A digit (or &) glued before the operator is the fd, not a word.
			if inWord && strings.Trim(cur.String(), "0123456789&") == "" {
				cur.Reset()
				inWord = false
			}
			endWord()
			if ch == '<' && strings.HasPrefix(s[i:], "<<") && !strings.HasPrefix(s[i:], "<<<") {
				i += 2
				strip := false
				if i < len(s) && s[i] == '-' {
					strip = true
					i++
				}
				for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
					i++
				}
				j := i
				for j < len(s) && strings.IndexByte(" \t\n;&|<>()", s[j]) < 0 {
					j++
				}
				delim := strings.NewReplacer("'", "", "\"", "", "\\", "").Replace(s[i:j])
				if delim == "" {
					return sc, errors.New("here-document without a delimiter")
				}
				heredoc = append(heredoc, delim)
				hdStrip = append(hdStrip, strip)
				hdQuote = append(hdQuote, delim != s[i:j])
				hdSeg = append(hdSeg, len(sc.segs)) // the command being read is stored next
				hasDoc = true
				i = j - 1
				continue
			}
			op := i
			for i+1 < len(s) && strings.IndexByte("<>&|", s[i+1]) >= 0 {
				i++
			}
			skipTgt = true
			tgtWrite = strings.ContainsRune(s[op:i+1], '>')
		case ch == ' ' || ch == '\t' || ch == '\r':
			endWord()
		default:
			cur.WriteByte(ch)
			inWord = true
		}
	}
	if len(heredoc) > 0 {
		return sc, errors.New("unterminated here-document")
	}
	endSeg()
	sc.subs = subs
	return sc, nil
}
