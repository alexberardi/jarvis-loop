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

func TestTrustReplySHA(t *testing.T) {
	const head = "0123abcd4567ef890123abcd4567ef890123abcd"
	for _, c := range []struct {
		text, want string
		ok         bool
	}{
		{"trust", head, true},
		{"<@U1> trust it", head, true},
		{"trust this", head, true},
		{"trust 0123abc", head, true},
		{"trust `0123ABCD`", head, true},
		{"trust " + head, head, true},
		{"trust 0000000", "0000000", false},
		{"trust 0123abce", "0123abce", false},
		{"trust " + head + "0", head + "0", false},
	} {
		got, ok := trustReplySHA(c.text, head)
		if got != c.want || ok != c.ok {
			t.Errorf("trustReplySHA(%q) = %q, %v; want %q, %v", c.text, got, ok, c.want, c.ok)
		}
	}
}
