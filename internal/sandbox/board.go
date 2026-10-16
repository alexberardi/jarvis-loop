package sandbox

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	neturl "net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// boardSession signs in to the board listener as a phone would (the link with ?t= once, then
// the cookie) and returns the board's HTML and the signed-in client.
func (s *Stack) boardSession() (string, *http.Client, error) {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	page, final, err := boardGet(c, s.BoardURL+"/board?t="+s.boardTok)
	if err != nil {
		return "", nil, err
	}
	if final.RawQuery != "" {
		return "", nil, fmt.Errorf("the board did not drop the token from its address: ended at %s", final.Path)
	}
	return page, c, nil
}

func boardGet(c *http.Client, url string) (string, *neturl.URL, error) {
	resp, err := c.Get(url)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return "", nil, fmt.Errorf("GET %s: HTTP %d: %s", resp.Request.URL.Path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return string(b), resp.Request.URL, nil
}

var (
	laneName = regexp.MustCompile(`<h2>(.*?) · \d+</h2>`)
	cardID   = regexp.MustCompile(`^\s*<a class="title" href="/board/(items|plans)/(\d+)"`)
)

// boardCards maps each card's item (or plan) id to its column and HTML.
func boardCards(page string) (items, plans map[int64][2]string) {
	items, plans = map[int64][2]string{}, map[int64][2]string{}
	for _, lane := range strings.Split(page, `<section class="lane">`)[1:] {
		m := laneName.FindStringSubmatch(lane)
		if m == nil {
			continue
		}
		for _, card := range strings.Split(lane, `<article class="card">`)[1:] {
			if id := cardID.FindStringSubmatch(card); id != nil {
				n, _ := strconv.ParseInt(id[2], 10, 64)
				if id[1] == "plans" {
					plans[n] = [2]string{m[1], card}
				} else {
					items[n] = [2]string{m[1], card}
				}
			}
		}
	}
	return items, plans
}

func (p *player) boardProblems(want map[string]BoardMatch) []string {
	page, client, err := p.s.boardSession()
	if err != nil {
		return []string{"board: " + err.Error()}
	}
	items, plans := boardCards(page)
	var out []string
	for _, alias := range sortedKeys(want) {
		m := want[alias]
		// An alias is an item's or a plan's (a plan's card links to its page, P<id>).
		var id int64
		var cards map[int64][2]string
		var name, path string
		if ref, ok := p.items[alias]; ok {
			id, cards, name, path = ref.id, items, fmt.Sprintf("#%d", ref.id), fmt.Sprintf("/board/items/%d", ref.id)
		} else if pid, ok := p.plans[alias]; ok {
			id, cards, name, path = pid, plans, fmt.Sprintf("P%d", pid), fmt.Sprintf("/board/plans/%d", pid)
		} else {
			out = append(out, fmt.Sprintf("board: no item or plan %q", alias))
			continue
		}
		c, ok := cards[id]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("board: %s (%s) has no card, want one in %s", alias, name, m.Column))
		case c[0] != m.Column:
			out = append(out, fmt.Sprintf("board: %s (%s) is in %s, want %s", alias, name, c[0], m.Column))
		case m.Card != "":
			ok, err := matches(m.Card, c[1])
			if err != nil {
				out = append(out, "board: "+err.Error())
			} else if !ok {
				out = append(out, fmt.Sprintf("board: %s (%s)'s card does not match %q:\n%s", alias, name, m.Card, c[1]))
			}
		}
		if m.Page == "" {
			continue
		}
		body, _, err := boardGet(client, p.s.BoardURL+path)
		if err != nil {
			out = append(out, "board: "+err.Error())
			continue
		}
		if ok, err := matches(m.Page, body); err != nil {
			out = append(out, "board: "+err.Error())
		} else if !ok {
			out = append(out, fmt.Sprintf("board: %s's page %s does not match %q", alias, path, m.Page))
		}
	}
	return out
}
