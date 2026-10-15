package runner

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

func TestUsageLimitMatcher(t *testing.T) {
	f, err := os.Open("testdata/limit_fixtures.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		var fx struct {
			Name  string          `json:"name"`
			Want  bool            `json:"want"`
			Reset string          `json:"reset"`
			Event json.RawMessage `json:"event"`
		}
		if err := json.Unmarshal(sc.Bytes(), &fx); err != nil {
			t.Fatal(err)
		}
		n++
		hit, ok := MatchUsageLimit(fx.Event)
		if ok != fx.Want {
			t.Errorf("%s: matched %v, want %v", fx.Name, ok, fx.Want)
			continue
		}
		if ok && (hit.Reset != fx.Reset || hit.Error == "") {
			t.Errorf("%s: %+v, want reset %q", fx.Name, hit, fx.Reset)
		}
	}
	if n < 15 {
		t.Fatalf("only %d fixtures", n)
	}
}
