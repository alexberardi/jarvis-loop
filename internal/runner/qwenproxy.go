package runner

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// qwenRoutes are the only requests the proxy forwards: OpenCode's chat completions and model list,
// under the local /v1.
var qwenRoutes = map[string]string{"/v1/chat/completions": http.MethodPost, "/v1/models": http.MethodGet}

// qwenProxy is the runner's proxy for one qwen run: it listens on a loopback port, takes only
// requests with the run's own token, forwards the OpenAI routes to qwen.base_url and adds the app
// credentials, which only the runner reads. Whatever auth the agent's side sends is dropped.
type qwenProxy struct {
	srv      *http.Server
	ln       net.Listener
	token    string
	requests atomic.Int64
}

// startQwenProxy starts the proxy for one run; token is the bearer token OpenCode must send (the
// run's own, so no other process on the box can use the app credentials through it).
func startQwenProxy(q *QwenConfig, token string) (*qwenProxy, error) {
	id, key, err := q.Credentials()
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(q.BaseURL)
	if err != nil {
		return nil, err
	}
	if len(token) < 16 {
		return nil, errors.New("qwen proxy: the run token is too short")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &qwenProxy{ln: ln, token: token}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			out := r.Out
			out.URL.Scheme, out.URL.Host = base.Scheme, base.Host
			out.URL.Path = strings.TrimRight(base.Path, "/") + strings.TrimPrefix(r.In.URL.Path, "/v1")
			out.URL.RawPath, out.URL.RawQuery = "", ""
			out.Host = base.Host
			for k := range out.Header {
				lk := strings.ToLower(k)
				if lk == "authorization" || lk == "cookie" || lk == "x-api-key" || strings.HasPrefix(lk, "proxy-") || strings.HasPrefix(lk, "x-jarvis-") {
					out.Header.Del(k)
				}
			}
			out.Header.Set("X-Jarvis-App-Id", id)
			out.Header.Set("X-Jarvis-App-Key", key)
		},
		FlushInterval: -1, // server-sent events go through as they come
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "qwen proxy: the upstream did not answer", http.StatusBadGateway)
		},
	}
	p.srv = &http.Server{ReadHeaderTimeout: 30 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+p.token)) != 1 {
			http.Error(w, "qwen proxy: not this run's token", http.StatusUnauthorized)
			return
		}
		if m, ok := qwenRoutes[r.URL.Path]; !ok || m != r.Method || r.URL.RawPath != "" {
			http.Error(w, "qwen proxy: only POST /v1/chat/completions and GET /v1/models", http.StatusForbidden)
			return
		}
		p.requests.Add(1)
		rp.ServeHTTP(w, r)
	})}
	go p.srv.Serve(ln)
	return p, nil
}

// Requests is how many requests the proxy forwarded.
func (p *qwenProxy) Requests() int64 { return p.requests.Load() }

// URL is the base URL OpenCode is given ("http://127.0.0.1:<port>/v1").
func (p *qwenProxy) URL() string { return "http://" + p.ln.Addr().String() + "/v1" }

func (p *qwenProxy) Close() error { return p.srv.Close() }
