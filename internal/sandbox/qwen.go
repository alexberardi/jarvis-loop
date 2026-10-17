package sandbox

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/runner"
)

// The sandbox's qwen builder (docs/decisions/2026-10-16-qwen-builder.md): a fake jarvisd
// background slot on loopback, and a credentials file only the runner reads. The fake opencode
// (the fake claude called as `run`) makes one chat request through the runner's proxy; the fake
// slot answers it only with the app headers the proxy adds and none of the agent's own auth.
const (
	qwenAppID  = "sandbox-app"
	qwenAppKey = "sandbox-key-not-a-secret"
)

type fakeQwen struct {
	srv      *http.Server
	url      string
	Requests atomic.Int64
	Refused  atomic.Int64
}

func startFakeQwen() (*fakeQwen, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	q := &fakeQwen{url: "http://" + ln.Addr().String() + "/v1"}
	q.srv = &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Jarvis-App-Id") != qwenAppID || r.Header.Get("X-Jarvis-App-Key") != qwenAppKey || r.Header.Get("Authorization") != "" {
			q.Refused.Add(1)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		q.Requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	})}
	go q.srv.Serve(ln)
	return q, nil
}

func (q *fakeQwen) Close() {
	if q != nil {
		q.srv.Close()
	}
}

// qwenConfig is the runner's qwen block against q, with its credentials file under dir; the
// fake claude plays opencode.
func (q *fakeQwen) config(dir, fake string) (*runner.QwenConfig, error) {
	creds := filepath.Join(dir, "qwen.env")
	if err := os.WriteFile(creds, []byte("PROD_APP_ID="+qwenAppID+"\nPROD_APP_KEY="+qwenAppKey+"\n"), 0o600); err != nil {
		return nil, err
	}
	return &runner.QwenConfig{BaseURL: q.url, CredentialsFile: creds, Model: "background", MaxMinutes: 30, OpenCode: fake}, nil
}
