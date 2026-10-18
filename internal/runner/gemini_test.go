package runner

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func geminiCfg(block string) string {
	return strings.Replace(cfgBody, `"name"`, `"gemini":`+block+`,"name"`, 1)
}

func TestGeminiConfig(t *testing.T) {
	// No block: no second opinion.
	c, err := LoadConfig(writeCfg(t, cfgBody, 0o600))
	if err != nil || c.Gemini != nil {
		t.Fatalf("no block: %+v %v", c.Gemini, err)
	}
	c, err = LoadConfig(writeCfg(t, geminiCfg(`{}`), 0o600))
	if err != nil {
		t.Fatal(err)
	}
	g := c.Gemini
	if g.Agy != "agy" || g.Model != DefaultGeminiModel || g.Blocking || g.TimeoutDur() != DefaultGeminiTimeout || !*g.Fence {
		t.Fatalf("defaults %+v", g)
	}
	// agy's login is a secret to every agent and masked from fenced commands but agy's own.
	home := Expand("~/.gemini")
	if g.Home != home || !slices.Contains(c.Secrets(), home) || !slices.Contains(c.VerifyMask, home) || !slices.Contains(c.Protected(), home) {
		t.Fatalf("login not protected: home %q secrets %v mask %v", g.Home, c.Secrets(), c.VerifyMask)
	}
	c, err = LoadConfig(writeCfg(t, geminiCfg(`{"agy":"~/.local/bin/agy","model":"gemini-3.8-flash-low","blocking":true,"timeout":"3m","fence":false}`), 0o600))
	if err != nil {
		t.Fatal(err)
	}
	g = c.Gemini
	if g.Agy != Expand("~/.local/bin/agy") || g.Model != "gemini-3.8-flash-low" || !g.Blocking || g.TimeoutDur() != 3*time.Minute || *g.Fence {
		t.Fatalf("set: %+v", g)
	}
	for name, tc := range map[string]struct{ block, want string }{
		"bad timeout":  {`{"timeout":"soon"}`, "gemini.timeout"},
		"zero timeout": {`{"timeout":"0s"}`, "gemini.timeout"},
		"bad model":    {`{"model":"gemini --yolo"}`, "gemini.model"},
	} {
		if _, err := LoadConfig(writeCfg(t, geminiCfg(tc.block), 0o600)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", name, err, tc.want)
		}
	}
}

func TestAgyArgs(t *testing.T) {
	got := strings.Join(AgyArgs("find bugs", "gemini-3.8-flash-high", "/w/schema.json"), " ")
	want := "-p find bugs --model gemini-3.8-flash-high --mode plan --sandbox --output-format json --json-schema /w/schema.json"
	if got != want {
		t.Fatalf("args\n got %s\nwant %s", got, want)
	}
}

func TestParseAgy(t *testing.T) {
	ok := `{"response":"{\"findings\":[{\"severity\":\"high\",\"file\":\"a.go\",\"line\":12,\"claim\":\"nil map write\",\"evidence\":\"m is nil at a.go:10\"},{\"severity\":\"Low\",\"file\":\"b.go\",\"line\":\"3\",\"claim\":\"typo\",\"evidence\":\"teh\"}]}","status":"success","usage":{"input_tokens":10}}`
	res, why := ParseAgy([]byte(ok))
	if why != "" || len(res.Findings) != 2 || res.Usage != `{"input_tokens":10}` {
		t.Fatalf("ok: %+v %q", res, why)
	}
	if f := res.Findings[0]; f.ID != 1 || f.Severity != "high" || f.File != "a.go" || f.Line != 12 || f.Claim != "nil map write" {
		t.Fatalf("finding 1 %+v", f)
	}
	if f := res.Findings[1]; f.ID != 2 || f.Severity != "low" || f.Line != 3 {
		t.Fatalf("finding 2 %+v", f)
	}
	for name, tc := range map[string]struct {
		out      string
		why      string
		findings int
		dropped  int
	}{
		"an object response":   {`{"response":{"findings":[]},"status":"success"}`, "", 0, 0},
		"a fenced response":    {"{\"response\":\"```json\\n{\\\"findings\\\":[]}\\n```\",\"status\":\"success\"}", "", 0, 0},
		"bad severity dropped": {`{"response":{"findings":[{"severity":"critical","file":"a","line":1,"claim":"x","evidence":"y"},{"severity":"medium","file":"a","line":1,"claim":"x","evidence":"y"}]}}`, "", 1, 1},
		"not JSON":             {"Thinking…", AgyBadOutput, 0, 0},
		"no findings key":      {`{"response":"sure, looks fine","status":"success"}`, AgyBadOutput, 0, 0},
		"an error status":      {`{"response":null,"status":"error"}`, AgyError, 0, 0},
		"a quota status":       {`{"response":null,"status":"error","error":"RESOURCE_EXHAUSTED: quota"}`, AgyQuota, 0, 0},
	} {
		res, why := ParseAgy([]byte(tc.out))
		if why != tc.why || len(res.Findings) != tc.findings || res.Dropped != tc.dropped {
			t.Errorf("%s: %+v %q, want %q with %d findings, %d dropped", name, res, why, tc.why, tc.findings, tc.dropped)
		}
	}
	// At most MaxFindings are kept.
	var many []string
	for i := 0; i < MaxFindings+3; i++ {
		many = append(many, `{"severity":"low","file":"a","line":1,"claim":"x","evidence":"y"}`)
	}
	res, _ = ParseAgy([]byte(`{"response":{"findings":[` + strings.Join(many, ",") + `]}}`))
	if len(res.Findings) != MaxFindings || res.Dropped != 3 {
		t.Fatalf("cap: %d kept, %d dropped", len(res.Findings), res.Dropped)
	}
}

func TestAgyFailure(t *testing.T) {
	for out, want := range map[string]string{
		"Error: [429] RESOURCE_EXHAUSTED: You have exhausted your daily quota": AgyQuota,
		"rate limit exceeded, retry in 30s":                                    AgyQuota,
		"Too Many Requests":                                                    AgyQuota,
		"panic: runtime error":                                                 AgyError,
	} {
		if got := agyFailure(nil, []byte(out)); got != want {
			t.Errorf("%q: %q, want %q", out, got, want)
		}
	}
}
