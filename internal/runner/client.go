package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client talks to loopd with the runner's token. The agent never sees it.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// APIError is a loopd error answer.
type APIError struct {
	Status int
	Code   string `json:"code"`
	Msg    string `json:"error"`
}

func (e *APIError) Error() string { return fmt.Sprintf("loopd %d %s: %s", e.Status, e.Code, e.Msg) }

// Claim is loopd's answer to a claim.
type Claim struct {
	Item  Item   `json:"item"`
	Stage string `json:"stage"`
	Lease struct {
		ID        string    `json:"id"`
		ExpiresAt time.Time `json:"expires_at"`
	} `json:"lease"`
}

// Control is the part of loopd's control the runner reads.
type Control struct {
	ActiveAgents  int  `json:"active_agents"`
	Stopped       bool `json:"stopped"`
	MaxRunMinutes int  `json:"max_run_minutes"`
}

func (c *Client) do(ctx context.Context, method, path, contentType string, body io.Reader, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Base, "/")+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 300 {
		ae := &APIError{Status: resp.StatusCode}
		if json.Unmarshal(raw, ae) != nil || ae.Code == "" {
			ae.Code, ae.Msg = "http", strings.TrimSpace(string(raw))
		}
		return resp.StatusCode, ae
	}
	if out != nil && resp.StatusCode != http.StatusNoContent && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("loopd %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

func (c *Client) json(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	return c.do(ctx, method, path, ct, body, out)
}

// Claim asks for work in the given stages; nil when there is none.
func (c *Client) Claim(ctx context.Context, agent string, caps, stages []string) (*Claim, error) {
	var cl Claim
	st, err := c.json(ctx, "POST", "/claim", map[string]any{"agent": agent, "capabilities": caps, "stages": stages}, &cl)
	if err != nil {
		return nil, err
	}
	if st == http.StatusNoContent {
		return nil, nil
	}
	return &cl, nil
}

// Heartbeat keeps a lease alive; true means stop.
func (c *Client) Heartbeat(ctx context.Context, lease, note string) (bool, error) {
	var out struct {
		Stop bool `json:"stop"`
	}
	if _, err := c.json(ctx, "POST", "/leases/"+lease+"/heartbeat", map[string]any{"note": note}, &out); err != nil {
		return false, err
	}
	return out.Stop, nil
}

// Advance finishes the stage with an outcome.
func (c *Client) Advance(ctx context.Context, id int64, outcome string, fields map[string]string, paths []string) error {
	_, err := c.json(ctx, "POST", fmt.Sprintf("/items/%d/advance", id),
		map[string]any{"outcome": outcome, "fields": fields, "changed_paths": paths}, nil)
	return err
}

// Event records an event from inside the run (a guard refusal).
func (c *Client) Event(ctx context.Context, id int64, kind string, data map[string]any) error {
	_, err := c.json(ctx, "POST", fmt.Sprintf("/items/%d/events", id), map[string]any{"kind": kind, "data": data}, nil)
	return err
}

// Control reads loopd's control.
func (c *Client) Control(ctx context.Context) (Control, error) {
	var ctl Control
	_, err := c.json(ctx, "GET", "/control", nil, &ctl)
	return ctl, err
}

// Upload streams one evidence file (label first, then the file, as loopd wants it).
func (c *Client) Upload(ctx context.Context, id int64, path, name, label string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			if label != "" {
				if err := mw.WriteField("label", label); err != nil {
					return err
				}
			}
			fw, err := mw.CreateFormFile("file", name)
			if err != nil {
				return err
			}
			if _, err := io.Copy(fw, f); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	_, err = c.do(ctx, "POST", fmt.Sprintf("/items/%d/evidence", id), mw.FormDataContentType(), pr, nil)
	return err
}

// EvidenceInfo is one evidence file on an item.
type EvidenceInfo struct {
	N     int    `json:"n"`
	Name  string `json:"name"`
	Label string `json:"label"`
	Size  int64  `json:"size"`
}

// Evidence lists an item's evidence files, oldest first.
func (c *Client) Evidence(ctx context.Context, id int64) ([]EvidenceInfo, error) {
	var d struct {
		Evidence []EvidenceInfo `json:"evidence"`
	}
	_, err := c.json(ctx, "GET", fmt.Sprintf("/items/%d", id), nil, &d)
	return d.Evidence, err
}

// DownloadEvidence saves evidence file n of an item to dst (at most MaxEvidenceBytes).
func (c *Client) DownloadEvidence(ctx context.Context, id int64, n int, dst string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.Base, "/")+fmt.Sprintf("/items/%d/evidence/%d", id, n), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Status: resp.StatusCode, Code: "http", Msg: strings.TrimSpace(string(b))}
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	n64, err := io.Copy(f, io.LimitReader(resp.Body, MaxEvidenceBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n64 > MaxEvidenceBytes {
		err = fmt.Errorf("larger than 20 MB")
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}
