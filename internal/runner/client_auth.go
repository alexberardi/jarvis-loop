package runner

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// send makes a request to loopd with the runner's token: the one place it leaves the runner. On
// jarvis-loop's hold list (docs/decisions/2026-10-18-narrow-loop-holds.md).
func (c *Client) send(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Base, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	return hc.Do(req)
}
