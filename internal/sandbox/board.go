package sandbox

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// boardPage signs in to the board listener as a phone would (the link with ?t= once, then the
// cookie) and returns the board's HTML.
func (s *Stack) boardPage() (string, error) {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	resp, err := c.Get(s.BoardURL + "/board?t=" + s.boardTok)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GET /board: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if resp.Request.URL.RawQuery != "" {
		return "", fmt.Errorf("the board did not drop the token from its address: ended at %s", resp.Request.URL.Path)
	}
	return string(b), nil
}

var (
	laneName = regexp.MustCompile(`<h2>(.*?) · \d+</h2>`)
	cardID   = regexp.MustCompile(`href="/board/items/(\d+)"`)
)

// boardCards maps each card's item id to its column and HTML.
func boardCards(page string) map[int64][2]string {
	out := map[int64][2]string{}
	for _, lane := range strings.Split(page, `<section class="lane">`)[1:] {
		m := laneName.FindStringSubmatch(lane)
		if m == nil {
			continue
		}
		for _, card := range strings.Split(lane, `<article class="card">`)[1:] {
			if id := cardID.FindStringSubmatch(card); id != nil {
				n, _ := strconv.ParseInt(id[1], 10, 64)
				out[n] = [2]string{m[1], card}
			}
		}
	}
	return out
}

func (p *player) boardProblems(want map[string]BoardMatch) []string {
	page, err := p.s.boardPage()
	if err != nil {
		return []string{"board: " + err.Error()}
	}
	cards := boardCards(page)
	var out []string
	for _, alias := range sortedKeys(want) {
		m := want[alias]
		ref, ok := p.items[alias]
		if !ok {
			out = append(out, fmt.Sprintf("board: no item %q", alias))
			continue
		}
		c, ok := cards[ref.id]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("board: %s (#%d) has no card, want one in %s", alias, ref.id, m.Column))
		case c[0] != m.Column:
			out = append(out, fmt.Sprintf("board: %s (#%d) is in %s, want %s", alias, ref.id, c[0], m.Column))
		case m.Card != "":
			ok, err := matches(m.Card, c[1])
			if err != nil {
				out = append(out, "board: "+err.Error())
			} else if !ok {
				out = append(out, fmt.Sprintf("board: %s (#%d)'s card does not match %q:\n%s", alias, ref.id, m.Card, c[1]))
			}
		}
	}
	return out
}
