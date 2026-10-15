package runner

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// LimitHit is a usage or rate limit that ends a run.
type LimitHit struct {
	Error string
	Reset string
}

var (
	// "You've hit your session limit · resets 3:45pm", "… weekly limit …", "… Opus limit …",
	// "You've hit your monthly spend limit · …" (docs: errors#usage-limits).
	hitLimitRe = regexp.MustCompile(`(?i)you(?:'|’)ve hit your [^·\n]*?(?:limit|budget)(?:\s*·\s*resets\s+([^\n]+))?`)
	// The older "Claude AI usage limit reached|<unix seconds>".
	legacyLimitRe = regexp.MustCompile(`(?i)usage limit reached\|(\d{9,11})`)
)

// notAPlanLimit is the server-side throttle the docs say is unrelated to the plan quota.
const notAPlanLimit = "not your usage limit"

// MatchUsageLimit reports whether one stream-json event says the run hit a usage or rate limit.
// Shapes (docs/decisions/2026-10-15-claude-headless-facts.md): a rate_limit_event with status
// "rejected"; an assistant message with error "rate_limit" or a limit text; a result whose text
// is a limit text or whose api_error_status is 429. An api_retry is a retry, not an end, and the
// "temporarily limiting requests (not your usage limit)" throttle and 529 overloads are not plan
// limits.
func MatchUsageLimit(raw []byte) (LimitHit, bool) {
	var ev struct {
		Type          string `json:"type"`
		Subtype       string `json:"subtype"`
		Error         string `json:"error"`
		IsError       bool   `json:"is_error"`
		Result        string `json:"result"`
		Errors        []string
		APIErrorState *int `json:"api_error_status"`
		RateLimit     *struct {
			Status    string  `json:"status"`
			ResetsAt  float64 `json:"resetsAt"`
			ErrorCode string  `json:"errorCode"`
		} `json:"rate_limit_info"`
		Message *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return LimitHit{}, false
	}
	switch ev.Type {
	case "rate_limit_event":
		if ev.RateLimit == nil || ev.RateLimit.Status != "rejected" {
			return LimitHit{}, false
		}
		h := LimitHit{Error: "rate limit: rejected"}
		if ev.RateLimit.ErrorCode != "" {
			h.Error += " (" + ev.RateLimit.ErrorCode + ")"
		}
		if ev.RateLimit.ResetsAt > 0 {
			h.Reset = unixReset(int64(ev.RateLimit.ResetsAt))
		}
		return h, true
	case "assistant":
		var text string
		if ev.Message != nil {
			for _, c := range ev.Message.Content {
				if c.Type == "text" {
					text += c.Text
				}
			}
		}
		if strings.Contains(text, notAPlanLimit) {
			return LimitHit{}, false
		}
		if h, ok := matchText(text); ok {
			return h, true
		}
		if ev.Error == "rate_limit" {
			return LimitHit{Error: firstNonEmpty(clip(text, 300), "rate_limit")}, true
		}
	case "result":
		text := ev.Result + " " + strings.Join(ev.Errors, " ")
		if strings.Contains(text, notAPlanLimit) {
			return LimitHit{}, false
		}
		if h, ok := matchText(text); ok {
			return h, true
		}
		if ev.IsError && ev.APIErrorState != nil && *ev.APIErrorState == 429 {
			return LimitHit{Error: firstNonEmpty(clip(strings.TrimSpace(text), 300), "HTTP 429")}, true
		}
	}
	return LimitHit{}, false
}

func matchText(text string) (LimitHit, bool) {
	if m := legacyLimitRe.FindStringSubmatch(text); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		return LimitHit{Error: "Claude AI usage limit reached", Reset: unixReset(n)}, true
	}
	if m := hitLimitRe.FindStringSubmatch(text); m != nil {
		h := LimitHit{Error: clip(strings.TrimSpace(m[0]), 300)}
		h.Reset = strings.TrimSpace(m[1])
		return h, true
	}
	return LimitHit{}, false
}

func unixReset(n int64) string { return time.Unix(n, 0).UTC().Format(time.RFC3339) }

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// clip cuts s to n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
