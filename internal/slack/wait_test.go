package slack

import (
	"slices"
	"testing"
)

func TestParseWaitWords(t *testing.T) {
	on, prs, reason, err := ParseWaitWords([]string{"on", "#472,", "and", "483", "pr", "o/n#444", "#9-merge", "because", "main's", "test", "fix"})
	if err != nil || !slices.Equal(on, []int64{472, 483}) || !slices.Equal(prs, []string{"o/n#444", "#9-merge"}) || reason != "main's test fix" {
		t.Fatalf("%v %v %q %v", on, prs, reason, err)
	}
	if _, _, _, err := ParseWaitWords([]string{"soon"}); err == nil {
		t.Fatal("soon parsed")
	}
	if _, _, _, err := ParseWaitWords([]string{"pr"}); err == nil {
		t.Fatal("pr without a PR parsed")
	}
	for text, want := range map[string]bool{"wait on #472": true, "wait #444-merge": true, "wait, let me look": false, "wait": false, "rebuild after #1": false} {
		if _, _, _, ok := WaitReply(text); ok != want {
			t.Errorf("%q: %v, want %v", text, ok, want)
		}
	}
}
