// Command loop is the CLI over loopd's API.
//
// It reads LOOP_URL (default http://127.0.0.1:7780) and LOOP_TOKEN from the environment; the
// token is never taken as an argument, so it never shows in a process list or shell history.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

const usage = `usage: loop <command> [args]

  add "<title>" [--ask TEXT] [--repo REPO] [--priority P1-P5] [--source SRC]
  status [--stage STAGE] [--priority P]
  show <id>
  answer <id> "<text>"
  approve <id> --sha <head sha> [--words TEXT]
  hold <id> [--words TEXT]
  priority <id> <P1-P5>
  agents [<0-6>]
  stop | resume

  for moving items by hand, as a runner:
  claim [--runner R] --agent A [--cap gpu,voice,...]
  heartbeat <lease> [--note TEXT]
  advance <id> <outcome> [--field k=v ...] [--evidence FILE ...]

environment: LOOP_URL (default http://127.0.0.1:7780), LOOP_TOKEN (required)
`

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

type usageError string

func (e usageError) Error() string { return string(e) }

type apiError struct {
	Status int
	Code   string `json:"code"`
	Msg    string `json:"error"`
}

func (e *apiError) Error() string { return fmt.Sprintf("%s: %s (HTTP %d)", e.Code, e.Msg, e.Status) }

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stderr, usage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	base := getenv("LOOP_URL")
	if base == "" {
		base = "http://127.0.0.1:7780"
	}
	tok := getenv("LOOP_TOKEN")
	if tok == "" {
		fmt.Fprintln(stderr, "loop: set LOOP_TOKEN to your loopd token")
		return 2
	}
	c := &client{base: strings.TrimRight(base, "/"), token: tok, http: &http.Client{Timeout: 60 * time.Second}, out: stdout}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "loop: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	pos, flags, err := parseArgs(args[1:], cmd.flags)
	if err == nil && (len(pos) < cmd.minArgs || len(pos) > cmd.maxArgs) {
		err = usageError(fmt.Sprintf("%s takes %d to %d arguments", args[0], cmd.minArgs, cmd.maxArgs))
	}
	if err == nil {
		err = cmd.run(c, pos, flags)
	}
	if err != nil {
		if _, ok := err.(usageError); ok {
			fmt.Fprintf(stderr, "loop: %v\n\n%s", err, usage)
			return 2
		}
		fmt.Fprintf(stderr, "loop: %v\n", err)
		return 1
	}
	return 0
}

// parseArgs separates positionals from --flags (anywhere on the line, as --k v or --k=v).
// allowed lists the flags a command takes; all take a value and may repeat.
func parseArgs(args []string, allowed []string) ([]string, map[string][]string, error) {
	var pos []string
	flags := map[string][]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "--") {
			pos = append(pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(a[2:], "=")
		known := false
		for _, f := range allowed {
			known = known || f == name
		}
		if !known {
			return nil, nil, usageError("unknown flag --" + name)
		}
		if !hasVal {
			if i+1 >= len(args) {
				return nil, nil, usageError("--" + name + " needs a value")
			}
			i++
			val = args[i]
		}
		flags[name] = append(flags[name], val)
	}
	return pos, flags, nil
}

func last(flags map[string][]string, k string) string {
	v := flags[k]
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}

type command struct {
	minArgs, maxArgs int
	flags            []string
	run              func(c *client, pos []string, flags map[string][]string) error
}

var commands = map[string]command{
	"add":       {1, 1, []string{"ask", "repo", "priority", "source"}, cmdAdd},
	"status":    {0, 0, []string{"stage", "priority"}, cmdStatus},
	"show":      {1, 1, nil, cmdShow},
	"answer":    {2, 2, nil, cmdAnswer},
	"approve":   {1, 1, []string{"sha", "words"}, cmdApprove},
	"hold":      {1, 1, []string{"words"}, cmdHold},
	"priority":  {2, 2, nil, cmdPriority},
	"agents":    {0, 1, nil, cmdAgents},
	"stop":      {0, 0, nil, cmdStop(true)},
	"resume":    {0, 0, nil, cmdStop(false)},
	"claim":     {0, 0, []string{"runner", "agent", "cap"}, cmdClaim},
	"heartbeat": {1, 1, []string{"note"}, cmdHeartbeat},
	"advance":   {2, 2, []string{"field", "evidence"}, cmdAdvance},
}

// ---- HTTP ----

type client struct {
	base  string
	token string
	http  *http.Client
	out   io.Writer
}

func (c *client) send(method, path, contentType string, body io.Reader, out any) (int, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		ae := &apiError{Status: resp.StatusCode}
		if json.Unmarshal(raw, ae) != nil || ae.Code == "" {
			ae.Code, ae.Msg = "http_error", strings.TrimSpace(string(raw))
		}
		return resp.StatusCode, ae
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode answer: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func (c *client) json(method, path string, in, out any) (int, error) {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	return c.send(method, path, ct, body, out)
}

// ---- shapes ----

type item struct {
	ID          int64     `json:"id"`
	Title       string    `json:"title"`
	Ask         string    `json:"ask"`
	Repo        string    `json:"repo"`
	Source      string    `json:"source"`
	Priority    string    `json:"priority"`
	Stage       string    `json:"stage"`
	ReturnStage string    `json:"return_stage"`
	Question    string    `json:"question"`
	Area        string    `json:"area"`
	Size        string    `json:"size"`
	Risk        string    `json:"risk"`
	Resources   []string  `json:"resources"`
	BounceCount int       `json:"bounce_count"`
	PR          int       `json:"pr"`
	Branch      string    `json:"branch"`
	HeadSHA     string    `json:"head_sha"`
	MergeSHA    string    `json:"merge_sha"`
	CreatedAt   time.Time `json:"created_at"`
	Approval    *struct {
		By      string    `json:"by"`
		Words   string    `json:"words"`
		HeadSHA string    `json:"head_sha"`
		At      time.Time `json:"at"`
	} `json:"approval"`
}

type control struct {
	ActiveAgents  int            `json:"active_agents"`
	Stopped       bool           `json:"stopped"`
	MaxRunMinutes int            `json:"max_run_minutes"`
	Limits        map[string]int `json:"limits"`
}

func itemID(s string) (string, error) {
	n, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil || n <= 0 {
		return "", usageError(fmt.Sprintf("%q is not an item id", s))
	}
	return strconv.FormatInt(n, 10), nil
}

func (c *client) moved(it item) {
	line := fmt.Sprintf("#%d → %s", it.ID, it.Stage)
	if it.Question != "" {
		line += fmt.Sprintf(" (question: %s)", it.Question)
	}
	fmt.Fprintln(c.out, line)
}

// ---- commands ----

func cmdAdd(c *client, pos []string, f map[string][]string) error {
	var it item
	_, err := c.json("POST", "/items", map[string]string{
		"title": pos[0], "ask": last(f, "ask"), "repo": last(f, "repo"), "priority": last(f, "priority"), "source": last(f, "source"),
	}, &it)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "added #%d %s (%s)\n", it.ID, it.Title, it.Priority)
	return nil
}

func cmdStatus(c *client, _ []string, f map[string][]string) error {
	q := url.Values{}
	if s := last(f, "stage"); s != "" {
		q.Set("stage", s)
	}
	if p := last(f, "priority"); p != "" {
		q.Set("priority", p)
	}
	path := "/items"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var board struct {
		Items []struct {
			ID         int64  `json:"id"`
			Title      string `json:"title"`
			Priority   string `json:"priority"`
			Display    string `json:"display"`
			ClaimedBy  string `json:"claimed_by"`
			AgeSeconds int64  `json:"age_seconds"`
		} `json:"items"`
	}
	if _, err := c.json("GET", path, nil, &board); err != nil {
		return err
	}
	var ctl control
	if _, err := c.json("GET", "/control", nil, &ctl); err != nil {
		return err
	}
	fmt.Fprintln(c.out, controlLine(ctl))
	if len(board.Items) == 0 {
		fmt.Fprintln(c.out, "no items")
		return nil
	}
	tw := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPRI\tSTAGE\tAGE\tTITLE\tCLAIMED BY")
	for _, r := range board.Items {
		fmt.Fprintf(tw, "#%d\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Priority, r.Display, age(r.AgeSeconds), r.Title, r.ClaimedBy)
	}
	return tw.Flush()
}

func age(secs int64) string {
	d := time.Duration(secs) * time.Second
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func controlLine(ctl control) string {
	state := "running"
	if ctl.Stopped {
		state = "STOPPED"
	}
	return fmt.Sprintf("active agents: %d of 6, %s, max run %dm", ctl.ActiveAgents, state, ctl.MaxRunMinutes)
}

func cmdShow(c *client, pos []string, _ map[string][]string) error {
	id, err := itemID(pos[0])
	if err != nil {
		return err
	}
	var d struct {
		Item    item   `json:"item"`
		Display string `json:"display"`
		Lease   *struct {
			ID        string    `json:"id"`
			Runner    string    `json:"runner"`
			Agent     string    `json:"agent"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"lease"`
		Events []struct {
			At      time.Time      `json:"at"`
			Actor   string         `json:"actor"`
			Kind    string         `json:"kind"`
			From    string         `json:"from"`
			To      string         `json:"to"`
			Outcome string         `json:"outcome"`
			Data    map[string]any `json:"data"`
		} `json:"events"`
		Evidence []struct {
			N           int    `json:"n"`
			Label       string `json:"label"`
			Name        string `json:"name"`
			Size        int64  `json:"size"`
			ContentType string `json:"content_type"`
			SHA256      string `json:"sha256"`
		} `json:"evidence"`
	}
	if _, err := c.json("GET", "/items/"+id, nil, &d); err != nil {
		return err
	}
	it := d.Item
	w := c.out
	fmt.Fprintf(w, "#%d %s\n", it.ID, it.Title)
	fmt.Fprintf(w, "stage:    %s (%s)\n", it.Stage, d.Display)
	fmt.Fprintf(w, "priority: %s   source: %s   repo: %s\n", it.Priority, it.Source, it.Repo)
	if it.Ask != "" {
		fmt.Fprintf(w, "ask:      %s\n", it.Ask)
	}
	if it.Area != "" {
		fmt.Fprintf(w, "triage:   area %s, size %s, risk %s, resources [%s]\n", it.Area, it.Size, it.Risk, strings.Join(it.Resources, ","))
	}
	if it.PR != 0 {
		fmt.Fprintf(w, "pr:       #%d %s @ %s (bounces %d)\n", it.PR, it.Branch, it.HeadSHA, it.BounceCount)
	}
	if it.Question != "" {
		fmt.Fprintf(w, "question: %s (returns to %s)\n", it.Question, it.ReturnStage)
	}
	if a := it.Approval; a != nil {
		fmt.Fprintf(w, "approval: %s for %s: %q\n", a.By, a.HeadSHA, a.Words)
	}
	if it.MergeSHA != "" {
		fmt.Fprintf(w, "merged:   %s\n", it.MergeSHA)
	}
	if l := d.Lease; l != nil {
		fmt.Fprintf(w, "lease:    %s by %s/%s, expires %s\n", l.ID, l.Runner, l.Agent, l.ExpiresAt.Format(time.RFC3339))
	}
	fmt.Fprintln(w, "\nevents:")
	for _, e := range d.Events {
		move := ""
		if e.From != "" || e.To != "" {
			move = fmt.Sprintf(" %s→%s", e.From, e.To)
		}
		fmt.Fprintf(w, "  %s  %-14s %-12s%s %s %s\n", e.At.Format("2006-01-02 15:04:05"), e.Kind, e.Outcome, move, e.Actor, compact(e.Data))
	}
	if len(d.Evidence) > 0 {
		fmt.Fprintln(w, "\nevidence:")
		for _, ev := range d.Evidence {
			fmt.Fprintf(w, "  %d  %s  %d bytes  %s  %s  %s\n", ev.N, ev.Name, ev.Size, ev.ContentType, ev.SHA256[:12], ev.Label)
		}
	}
	return nil
}

func compact(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		v, _ := json.Marshal(m[k])
		s := string(v)
		if len(s) > 80 {
			s = s[:77] + "..."
		}
		parts = append(parts, k+"="+s)
	}
	return strings.Join(parts, " ")
}

func cmdAnswer(c *client, pos []string, _ map[string][]string) error {
	id, err := itemID(pos[0])
	if err != nil {
		return err
	}
	var it item
	if _, err := c.json("POST", "/items/"+id+"/answer", map[string]string{"text": pos[1]}, &it); err != nil {
		return err
	}
	c.moved(it)
	return nil
}

func cmdApprove(c *client, pos []string, f map[string][]string) error {
	id, err := itemID(pos[0])
	if err != nil {
		return err
	}
	sha := last(f, "sha")
	if sha == "" {
		return usageError("approve needs --sha, the PR head you are approving")
	}
	var it item
	if _, err := c.json("POST", "/items/"+id+"/approve", map[string]string{"head_sha": sha, "words": last(f, "words")}, &it); err != nil {
		return err
	}
	c.moved(it)
	return nil
}

func cmdHold(c *client, pos []string, f map[string][]string) error {
	id, err := itemID(pos[0])
	if err != nil {
		return err
	}
	var it item
	if _, err := c.json("POST", "/items/"+id+"/hold", map[string]string{"words": last(f, "words")}, &it); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "#%d still held\n", it.ID)
	return nil
}

func cmdPriority(c *client, pos []string, _ map[string][]string) error {
	id, err := itemID(pos[0])
	if err != nil {
		return err
	}
	var it item
	if _, err := c.json("PATCH", "/items/"+id, map[string]string{"priority": strings.ToUpper(pos[1])}, &it); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "#%d is %s\n", it.ID, it.Priority)
	return nil
}

func (c *client) setControl(patch map[string]any) error {
	var ctl control
	method := "PUT"
	if patch == nil {
		method = "GET"
	}
	if _, err := c.json(method, "/control", patch, &ctl); err != nil {
		return err
	}
	fmt.Fprintln(c.out, controlLine(ctl))
	return nil
}

func cmdAgents(c *client, pos []string, _ map[string][]string) error {
	if len(pos) == 0 {
		return c.setControl(nil)
	}
	n, err := strconv.Atoi(pos[0])
	if err != nil {
		return usageError("agents takes a number from 0 to 6")
	}
	return c.setControl(map[string]any{"active_agents": n})
}

func cmdStop(stop bool) func(*client, []string, map[string][]string) error {
	return func(c *client, _ []string, _ map[string][]string) error {
		return c.setControl(map[string]any{"stopped": stop})
	}
}

func cmdClaim(c *client, _ []string, f map[string][]string) error {
	var caps []string
	for _, v := range f["cap"] {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				caps = append(caps, s)
			}
		}
	}
	var out struct {
		Item  item   `json:"item"`
		Stage string `json:"stage"`
		Lease struct {
			ID        string    `json:"id"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"lease"`
	}
	status, err := c.json("POST", "/claim", map[string]any{"runner": last(f, "runner"), "agent": last(f, "agent"), "capabilities": caps}, &out)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent {
		fmt.Fprintln(c.out, "nothing to do")
		return nil
	}
	fmt.Fprintf(c.out, "claimed #%d at %s, lease %s (expires %s): %s\n", out.Item.ID, out.Stage, out.Lease.ID,
		out.Lease.ExpiresAt.Format(time.RFC3339), out.Item.Title)
	return nil
}

func cmdHeartbeat(c *client, pos []string, f map[string][]string) error {
	var out struct {
		Stop bool `json:"stop"`
	}
	if _, err := c.json("POST", "/leases/"+url.PathEscape(pos[0])+"/heartbeat", map[string]string{"note": last(f, "note")}, &out); err != nil {
		return err
	}
	if out.Stop {
		fmt.Fprintln(c.out, "stop")
	} else {
		fmt.Fprintln(c.out, "keep going")
	}
	return nil
}

func cmdAdvance(c *client, pos []string, f map[string][]string) error {
	id, err := itemID(pos[0])
	if err != nil {
		return err
	}
	fields := map[string]string{}
	for _, kv := range f["field"] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return usageError("--field takes key=value, got " + strconv.Quote(kv))
		}
		fields[k] = v
	}
	for _, path := range f["evidence"] {
		if err := c.upload(id, path); err != nil {
			return fmt.Errorf("evidence %s: %w", path, err)
		}
	}
	var it item
	if _, err := c.json("POST", "/items/"+id+"/advance", map[string]any{"outcome": pos[1], "fields": fields}, &it); err != nil {
		return err
	}
	c.moved(it)
	return nil
}

// upload streams one file as multipart (label first, then file) without holding it in memory.
func (c *client) upload(id, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := mw.WriteField("label", filepath.Base(path))
		if err == nil {
			var fw io.Writer
			if fw, err = mw.CreateFormFile("file", filepath.Base(path)); err == nil {
				if _, err = io.Copy(fw, f); err == nil {
					err = mw.Close()
				}
			}
		}
		pw.CloseWithError(err)
	}()
	var ev struct {
		N    int    `json:"n"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if _, err := c.send("POST", "/items/"+id+"/evidence", mw.FormDataContentType(), pr, &ev); err != nil {
		pr.CloseWithError(err)
		return err
	}
	fmt.Fprintf(c.out, "evidence %d: %s (%d bytes)\n", ev.N, ev.Name, ev.Size)
	return nil
}
