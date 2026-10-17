package slack

import "testing"

func TestIsTrustReply(t *testing.T) {
	for text, want := range map[string]bool{
		"trust": true, "Trust.": true, "<@U1> trust it": true, "trust this": true, "trust 0123abcd": true, "trust `0123abcd`": true,
		"": false, "trust me on this one": false, "I trust it": false, "don't trust": false, "trust nope": false,
		"retry": false, "trust?": false,
	} {
		if got := IsTrustReply(text); got != want {
			t.Errorf("IsTrustReply(%q) = %v, want %v", text, got, want)
		}
	}
}
