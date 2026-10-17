package runner

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQwenProxy(t *testing.T) {
	type seen struct {
		path, id, key, auth, cookie, extra string
	}
	got := make(chan seen, 4)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.URL.Path, r.Header.Get("X-Jarvis-App-Id"), r.Header.Get("X-Jarvis-App-Key"), r.Header.Get("Authorization"),
			r.Header.Get("Cookie"), r.Header.Get("X-Jarvis-Extra")}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	q := &QwenConfig{BaseURL: up.URL + "/v1", CredentialsFile: credsFile(t, "PROD_APP_ID=loop-qwen\nPROD_APP_KEY=k3y\n", 0o600)}
	const tok = "run-token-0123456789abcdef"
	if _, err := startQwenProxy(q, "short"); err == nil {
		t.Fatal("a short run token was taken")
	}
	p, err := startQwenProxy(q, tok)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if !strings.HasPrefix(p.URL(), "http://127.0.0.1:") || !strings.HasSuffix(p.URL(), "/v1") {
		t.Fatalf("url %q", p.URL())
	}
	base := strings.TrimSuffix(p.URL(), "/v1")
	hc := &http.Client{Timeout: 10 * time.Second}
	do := func(method, path, auth string) int {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// Without the run's token, nothing passes.
	for _, auth := range []string{"", "Bearer wrong-token-0123456789", "Basic " + tok} {
		if code := do("GET", "/v1/models", auth); code != http.StatusUnauthorized {
			t.Errorf("auth %q: %d, want 401", auth, code)
		}
	}

	req, _ := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(`{"model":"background","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Cookie", "a=b")
	req.Header.Set("X-Jarvis-App-Id", "spoofed")
	req.Header.Set("X-Jarvis-Extra", "x")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// The first event arrives before the upstream finishes: the proxy streams.
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil || line != "data: one\n" {
		t.Fatalf("first line %q %v", line, err)
	}
	rest, _ := io.ReadAll(br)
	resp.Body.Close()
	if !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("rest %q", rest)
	}
	s := <-got
	if s.path != "/v1/chat/completions" || s.id != "loop-qwen" || s.key != "k3y" || s.auth != "" || s.cookie != "" || s.extra != "" {
		t.Fatalf("upstream saw %+v", s)
	}
	if p.Requests() != 1 {
		t.Fatalf("requests %d", p.Requests())
	}

	// Only the chat and model routes pass.
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/chat/completions"}, {"POST", "/v1/admin/apps"}, {"GET", "/internal/app-ping"},
		{"POST", "/v1/chat/completions/../../internal/x"}, {"POST", "/v2/chat/completions"},
	} {
		if code := do(c.method, c.path, "Bearer "+tok); code != http.StatusForbidden {
			t.Errorf("%s %s: %d, want 403", c.method, c.path, code)
		}
	}
	select {
	case s := <-got:
		t.Fatalf("a refused request reached the upstream: %+v", s)
	default:
	}
	if code := do("GET", "/v1/models", "Bearer "+tok); code != 200 {
		t.Fatalf("models: %d", code)
	}
	if s := <-got; s.path != "/v1/models" || s.key != "k3y" {
		t.Fatalf("models upstream %+v", s)
	}
}

// An upstream that answers before it has read the request body must not cut the stream: the
// proxy is still forwarding the body while the reply comes back. The client sends the body in
// parts over a raw connection, so the proxy has read only some of it when the reply's headers go
// out.
func TestQwenProxyRepliesWhileBodyIsSent(t *testing.T) {
	sent := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NewResponseController(w).EnableFullDuplex() // answers without waiting for the body
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-sent
		time.Sleep(100 * time.Millisecond)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	q := &QwenConfig{BaseURL: up.URL + "/v1", CredentialsFile: credsFile(t, "PROD_APP_ID=loop-qwen\nPROD_APP_KEY=k3y\n", 0o600)}
	const tok = "run-token-0123456789abcdef"
	p, err := startQwenProxy(q, tok)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(strings.TrimSuffix(p.URL(), "/v1"), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	parts := []string{`{"model":"background",`, `"stream":true,`, `"a":1,`, `"b":2,`, `"c":3}`}
	fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n",
		tok, len(strings.Join(parts, "")))
	go func() {
		for _, s := range parts {
			io.WriteString(conn, s)
			time.Sleep(100 * time.Millisecond)
		}
		close(sent)
	}()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(all), "data: one") || !strings.Contains(string(all), "[DONE]") {
		t.Fatalf("reply %q", all)
	}
}
