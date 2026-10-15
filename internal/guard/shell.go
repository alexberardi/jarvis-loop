package guard

import (
	"errors"
	"strings"
)

// parse splits a shell script into simple commands (each a list of words, quotes removed) and
// returns the text of every command substitution ($(...), `...`, <(...), >(...)) so the caller
// checks those too. It is deliberately small: it knows quotes, escapes, the control operators,
// redirections, comments and here-documents, which is what the guard needs to find the program
// and its arguments. Anything it cannot read (an unterminated quote or substitution) is an error,
// and the guard refuses the command.
func parse(s string) (segs [][]string, subs []string, err error) {
	var (
		words   []string
		cur     strings.Builder
		inWord  bool
		skipTgt bool     // the next word is a redirection target
		heredoc []string // pending here-document delimiters (body starts after the newline)
		hdStrip []bool
	)
	endWord := func() {
		if !inWord {
			return
		}
		if skipTgt {
			skipTgt = false
		} else {
			words = append(words, cur.String())
		}
		cur.Reset()
		inWord = false
	}
	endSeg := func() {
		endWord()
		if len(words) > 0 {
			segs = append(segs, words)
		}
		words = nil
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
			cur.WriteString("$SUB")
			inWord = true
			return end, true, nil
		}
		if s[i] == '`' {
			end, err := backtick(i + 1)
			if err != nil {
				return 0, false, err
			}
			subs = append(subs, s[i+1:end])
			cur.WriteString("$SUB")
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
				return nil, nil, errors.New("unterminated single quote")
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
						return nil, nil, err
					}
					if ok {
						i = end
						continue
					}
				}
				cur.WriteByte(c)
			}
			if !closed {
				return nil, nil, errors.New("unterminated double quote")
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
				return nil, nil, err
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
			// Skip the bodies of here-documents opened on the line just ended.
			for k, delim := range heredoc {
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
				}
			}
			heredoc, hdStrip = nil, nil
		case ch == ';' || ch == '&' || ch == '|' || ch == '(' || ch == ')':
			endSeg()
		case ch == '<' || ch == '>':
			// Process substitution: check the inner command.
			if i+1 < len(s) && s[i+1] == '(' {
				end, err := closing(i + 2)
				if err != nil {
					return nil, nil, err
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
					return nil, nil, errors.New("here-document without a delimiter")
				}
				heredoc = append(heredoc, delim)
				hdStrip = append(hdStrip, strip)
				i = j - 1
				continue
			}
			for i+1 < len(s) && strings.IndexByte("<>&|", s[i+1]) >= 0 {
				i++
			}
			skipTgt = true
		case ch == ' ' || ch == '\t' || ch == '\r':
			endWord()
		default:
			cur.WriteByte(ch)
			inWord = true
		}
	}
	if len(heredoc) > 0 {
		return nil, nil, errors.New("unterminated here-document")
	}
	endSeg()
	return segs, subs, nil
}
