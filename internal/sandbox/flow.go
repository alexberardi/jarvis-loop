package sandbox

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// FlowMatch checks the flow panel (docs/decisions/2026-10-18-flow-panel.md) for Window (default
// 24h): JSON maps a dotted path into GET /flow's answer ("waiting.0.reason", "runners.0.slots")
// to a regexp on that value as JSON (a string with its quotes); Page is a regexp on /board/flow,
// read from the board listener. In both, {alias} is the item's id.
type FlowMatch struct {
	Window string            `json:"window,omitempty"`
	JSON   map[string]string `json:"json,omitempty"`
	Page   string            `json:"page,omitempty"`
}

func (p *player) flowProblems(m FlowMatch) []string {
	window := m.Window
	if window == "" {
		window = "24h"
	}
	var out []string
	var doc any
	if err := p.s.user.do("GET", "/flow?window="+window, nil, &doc); err != nil {
		return []string{"flow: " + err.Error()}
	}
	for _, path := range sortedKeys(m.JSON) {
		v, ok := jsonPath(doc, path)
		if !ok {
			out = append(out, fmt.Sprintf("flow: no %s in GET /flow", path))
			continue
		}
		b, _ := json.Marshal(v)
		if ok, err := matches(p.withIDs(m.JSON[path]), string(b)); err != nil {
			out = append(out, "flow: "+err.Error())
		} else if !ok {
			out = append(out, fmt.Sprintf("flow: %s is %s, want %q", path, b, m.JSON[path]))
		}
	}
	if m.Page != "" {
		_, c, err := p.s.boardSession()
		if err != nil {
			return append(out, "flow page: "+err.Error())
		}
		page, _, err := boardGet(c, p.s.BoardURL+"/board/flow?window="+window)
		if err != nil {
			return append(out, "flow page: "+err.Error())
		}
		if ok, err := matches(p.withIDs(m.Page), page); err != nil {
			out = append(out, "flow page: "+err.Error())
		} else if !ok {
			out = append(out, fmt.Sprintf("flow page does not match %q", m.Page))
		}
	}
	return out
}

// withIDs puts each item's id in for {alias}.
func (p *player) withIDs(pattern string) string {
	for alias, ref := range p.items {
		pattern = strings.ReplaceAll(pattern, "{"+alias+"}", strconv.FormatInt(ref.id, 10))
	}
	return pattern
}

// jsonPath follows a dotted path (object keys, array indexes) into a decoded JSON value.
func jsonPath(v any, path string) (any, bool) {
	for _, part := range strings.Split(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			nv, ok := x[part]
			if !ok {
				return nil, false
			}
			v = nv
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			v = x[i]
		default:
			return nil, false
		}
	}
	return v, true
}
