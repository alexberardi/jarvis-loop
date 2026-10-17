package pipeline

import (
	"testing"
	"time"
)

func TestCheckOrchestratorAnswer(t *testing.T) {
	h1, h2 := "aaaa", "bbbb"
	cases := []struct {
		name, kind, text, head string
		prior                  []OrchAnswer
		want                   string
	}{
		{"first", AnswerHarness, "list it under not verified", h1, nil, ""},
		{"unknown kind", "product", "do it", h1, nil, CodeInvalidField},
		{"retry is not an answer", "retry", "again", h1, nil, CodeInvalidField},
		{"empty text", AnswerLoop, "  ", h1, nil, CodeMissingField},
		{"second on head", AnswerLoop, "b", h1, []OrchAnswer{{AnswerLoop, "a", h1}}, ""},
		{"third on head", AnswerLoop, "c", h1, []OrchAnswer{{AnswerLoop, "a", h1}, {AnswerLoop, "b", h1}}, CodeLimit},
		{"new head resets", AnswerLoop, "c", h2, []OrchAnswer{{AnswerLoop, "a", h1}, {AnswerLoop, "b", h1}}, ""},
		{"same as previous", AnswerLoop, " Use  the Harness ", h2, []OrchAnswer{{AnswerLoop, "use the harness", h1}}, CodeLimit},
		{"same as older is fine", AnswerLoop, "a", h2, []OrchAnswer{{AnswerLoop, "a", h1}, {AnswerLoop, "b", h1}}, ""},
		{"bounce once per item", AnswerBounce, "go on", h2, []OrchAnswer{{AnswerBounce, "go on with the notes", h1}}, CodeLimit},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := code(CheckOrchestratorAnswer(c.kind, c.text, c.head, c.prior)); got != c.want {
				t.Fatalf("code %q, want %q", got, c.want)
			}
		})
	}
}

func TestRetryWait(t *testing.T) {
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		failures int
		wait     time.Duration
		ask      bool
	}{{1, 2 * time.Minute, false}, {2, 10 * time.Minute, false}, {3, 0, true}, {4, 0, true}}
	for _, c := range cases {
		nb, ask := RetryWait(c.failures, at)
		if ask != c.ask || (!ask && nb.Sub(at) != c.wait) {
			t.Fatalf("failure %d: %v %v, want wait %v ask %v", c.failures, nb.Sub(at), ask, c.wait, c.ask)
		}
	}
}

func TestCheckAgentsDown(t *testing.T) {
	for _, c := range []struct {
		cur, want int
		code      string
	}{{4, 2, ""}, {4, 0, ""}, {4, 4, CodeLimit}, {2, 5, CodeLimit}, {4, -1, CodeInvalidField}} {
		if got := code(CheckAgentsDown(c.cur, c.want)); got != c.code {
			t.Fatalf("%d -> %d: %q, want %q", c.cur, c.want, got, c.code)
		}
	}
}

func TestNormaliseSignature(t *testing.T) {
	a := NormaliseSignature("fatal: Unable to create '/home/alex/.cache/w/71-build/.git/index.lock': File exists (run 3, sha 1f27f8a)")
	b := NormaliseSignature("fatal: unable to create '/home/alex/.cache/w/68-build/.git/index.lock': file exists (run 12, sha 8beed37)")
	if a != b || a == "" {
		t.Fatalf("%q != %q", a, b)
	}
	if c := NormaliseSignature("claude: model overloaded"); c == a {
		t.Fatalf("different errors share a signature %q", c)
	}
}

func TestNeedReason(t *testing.T) {
	if _, err := NeedReason(" "); code(err) != CodeMissingField {
		t.Fatalf("empty reason: %v", err)
	}
	if r, err := NeedReason(" flaky git lock "); err != nil || r != "flaky git lock" {
		t.Fatalf("%q %v", r, err)
	}
}
