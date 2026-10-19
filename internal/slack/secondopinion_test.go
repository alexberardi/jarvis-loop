package slack

import "testing"

// A second_opinion event is one line in the item's thread: counts, never the findings' text.
func TestSecondOpinionLine(t *testing.T) {
	for want, d := range map[string]map[string]any{
		"Second opinion (Gemini) unavailable (quota); the review stands.": {"status": "unavailable", "reason": "quota"},
		"Second opinion (Gemini): no findings.":                           {"status": "ok", "findings": 0.0},
		"Second opinion (Gemini): 3 findings (1 high), Claude agreed with 2 (1 high); advisory, recorded only.": {
			"status": "ok", "findings": 3.0, "high": 1.0, "agreed": 2.0, "agreed_high": 1.0, "blocking": false},
		"Second opinion (Gemini): 3 findings (1 high), Claude agreed with 2 (1 high); sent back to build.": {
			"status": "ok", "findings": 3.0, "high": 1.0, "agreed": 2.0, "agreed_high": 1.0, "blocking": true, "bounced": true},
		"Second opinion (Gemini): 3 findings (1 high), Claude agreed with 2 (1 high); this head was sent back once already, so it stands.": {
			"status": "ok", "findings": 3.0, "high": 1.0, "agreed": 2.0, "agreed_high": 1.0, "blocking": true, "repeat": true},
		"Second opinion (Gemini): 3 findings (0 high), Claude agreed with 0 (0 high); the review stands.": {
			"status": "ok", "findings": 3.0, "blocking": true},
		"Second opinion (Gemini): 2 findings (1 high); not judged (Claude's usage limit); the review stands.": {
			"status": "unjudged", "findings": 2.0, "high": 1.0, "reason": "Claude's usage limit"},
	} {
		if got := secondOpinionLine(d); got != want {
			t.Errorf("got  %s\nwant %s", got, want)
		}
	}
}
