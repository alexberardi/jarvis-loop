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
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

const usage = `usage: loop <command> [args]

  add "<title>" [--ask TEXT] [--repo REPO] [--priority P1-P5] [--source SRC] [--after 12,13]
      [--feature NAME] [--json | -q]
                              REPO: owner/name, the bare name or an alias; loopd stores the slug
                              NAME: the feature it is part of (lowercase, words joined by -)
                              --json prints the item as JSON, -q (--quiet) only its number
  status [--stage STAGE] [--priority P]
  show <id>
  answer <id> "<text>"
  approve <id> --sha <head sha> [--words TEXT]
  trust <id> --sha <sha>      let verify run that one commit's change to trusted files
  hold <id> [--words TEXT]
  drop <id> [--words TEXT]
  qa <id>                     check it yourself once merged (it waits in qa)
  verified <id> [--words TEXT]
  reopen <id> "<what is wrong>"
  priority <id> <P1-P5>
  edit <id> [--title TEXT] [--ask TEXT]   before build starts (in triage or on its question)
  link <id> --after 12,13     make it wait for those items too
  unlink <id> <after id>      stop it waiting for that item
  resources <id> <macos,voice,...|none>   after triage; applies from the next claim
  feature <id> <name|none>    put it in that feature, or in none
  rebuild <id> "<notes>" | --notes TEXT   send a held or needs_you item back to build
  agents [<0-6>]
  stop | resume
  usage                       claude's 5h window and week, the pace, the days, the costliest items
  budget [claude] [--per-5h N] [--per-week N] [--pace-at PCT]
                              N: tokens (2M, 500k), a percent of what got us limited last time
                              (80%), or off; pacing starts at --pace-at percent (default 85)
  unfreeze <repo>             lift the freeze on a repo whose main went red (owner/name or bare name)

  for moving items by hand, as a runner:
  claim [--runner R] --agent A [--cap gpu,voice,...]
  heartbeat <lease> [--note TEXT]
  advance <id> <outcome> [--field k=v ...] [--evidence FILE ...]
          [--path "M internal/x.go" ...] [--paths-file FILE]
      pr_opened, merged and hold need the PR's changed paths: --path per file, or
      --paths-file with the output of git diff --name-status main...HEAD

flags go anywhere on the line, as --name value or --name=value; text that starts with - goes
after --.

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
		err = argCountError(args[0], cmd.minArgs, cmd.maxArgs, pos)
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

func argCountError(name string, min, max int, pos []string) error {
	want := fmt.Sprintf("%d to %d arguments", min, max)
	switch {
	case min == max && min == 1:
		want = "1 argument"
	case min == max:
		want = fmt.Sprintf("%d arguments", min)
	}
	msg := fmt.Sprintf("%s takes %s, got %d", name, want, len(pos))
	if len(pos) > max {
		quoted := make([]string, len(pos))
		for i, p := range pos {
			quoted[i] = strconv.Quote(p)
		}
		msg += " (" + strings.Join(quoted, " ") + "): quote text with spaces"
	}
	return usageError(msg)
}

// boolFlags take no value: given, they read "true".
var boolFlags = map[string]bool{"json": true, "quiet": true}

// shortFlags are the one-letter spellings of long flags.
var shortFlags = map[string]string{"q": "quiet"}

// parseArgs separates positionals from --flags (anywhere on the line, as --k v or --k=v).
// allowed lists the flags a command takes; all but boolFlags take a value and may repeat. A
// single-dash word is a short flag or refused, so a mistyped flag never becomes a positional.
func parseArgs(args []string, allowed []string) ([]string, map[string][]string, error) {
	var pos []string
	flags := map[string][]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		var name, val string
		var hasVal bool
		switch {
		case strings.HasPrefix(a, "--"):
			name, val, hasVal = strings.Cut(a[2:], "=")
		case len(a) > 1 && a[0] == '-' && (a[1] < '0' || a[1] > '9'):
			long, ok := shortFlags[a[1:]]
			if !ok || !slices.Contains(allowed, long) {
				return nil, nil, usageError("unknown flag " + a + " (flags are --long; put text that starts with - after --)")
			}
			name = long
		default:
			pos = append(pos, a)
			continue
		}
		if !slices.Contains(allowed, name) {
			return nil, nil, usageError("unknown flag --" + name)
		}
		if boolFlags[name] {
			if hasVal {
				return nil, nil, usageError("--" + name + " takes no value")
			}
			flags[name] = []string{"true"}
			continue
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
	"add":       {1, 1, []string{"ask", "repo", "priority", "source", "after", "feature", "json", "quiet"}, cmdAdd},
	"status":    {0, 0, []string{"stage", "priority"}, cmdStatus},
	"show":      {1, 1, nil, cmdShow},
	"answer":    {2, 2, nil, cmdAnswer},
	"approve":   {1, 1, []string{"sha", "words"}, cmdApprove},
	"trust":     {1, 1, []string{"sha"}, cmdTrust},
	"hold":      {1, 1, []string{"words"}, cmdHold},
	"drop":      {1, 1, []string{"words"}, cmdDrop},
	"qa":        {1, 1, nil, cmdQA},
	"verified":  {1, 1, []string{"words"}, cmdVerified},
	"reopen":    {2, 2, nil, cmdReopen},
	"priority":  {2, 2, nil, cmdPriority},
	"edit":      {1, 1, []string{"title", "ask"}, cmdEdit},
	"link":      {1, 1, []string{"after"}, cmdLink},
	"unlink":    {2, 2, nil, cmdUnlink},
	"resources": {2, 2, nil, cmdResources},
	"feature":   {2, 2, nil, cmdFeature},
	"rebuild":   {1, 2, []string{"notes"}, cmdRebuild},
	"agents":    {0, 1, nil, cmdAgents},
	"usage":     {0, 0, nil, cmdUsage},
	"budget":    {0, 1, []string{"per-5h", "per-week", "pace-at"}, cmdBudget},
	"unfreeze":  {1, 1, nil, cmdUnfreeze},
	"stop":      {0, 0, nil, cmdStop(true)},
	"resume":    {0, 0, nil, cmdStop(false)},
	"claim":     {0, 0, []string{"runner", "agent", "cap"}, cmdClaim},
	"heartbeat": {1, 1, []string{"note"}, cmdHeartbeat},
	"advance":   {2, 2, []string{"field", "evidence", "path", "paths-file"}, cmdAdvance},
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
	HoldReason  string    `json:"hold_reason"`
	Resources   []string  `json:"resources"`
	BounceCount int       `json:"bounce_count"`
	PR          int       `json:"pr"`
	Branch      string    `json:"branch"`
	HeadSHA     string    `json:"head_sha"`
	MergeSHA    string    `json:"merge_sha"`
	NeedsQA     bool      `json:"needs_qa"`
	ReopenOf    int64     `json:"reopen_of"`
	DependsOn   []int64   `json:"depends_on"`
	Feature     string    `json:"feature"`
	CreatedAt   time.Time `json:"created_at"`
	Approval    *struct {
		By      string    `json:"by"`
		Words   string    `json:"words"`
		HeadSHA string    `json:"head_sha"`
		At      time.Time `json:"at"`
	} `json:"approval"`
	Trust *struct {
		HeadSHA  string   `json:"head_sha"`
		Paths    []string `json:"paths"`
		Diffstat string   `json:"diffstat"`
		By       string   `json:"by"`
		Used     bool     `json:"used"`
		// CarriedFrom: the OK was for this head and carried to HeadSHA (the same trusted diff).
		CarriedFrom string `json:"carried_from"`
	} `json:"trust"`
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

// afterIDs reads --after: comma lists of item ids, the flag repeatable.
func afterIDs(f map[string][]string) ([]int64, error) {
	var after []int64
	for _, a := range f["after"] {
		for _, s := range strings.Split(a, ",") {
			id, err := itemID(strings.TrimSpace(s))
			if err != nil {
				return nil, err
			}
			n, _ := strconv.ParseInt(id, 10, 64)
			after = append(after, n)
		}
	}
	return after, nil
}

func cmdAdd(c *client, pos []string, f map[string][]string) error {
	asJSON, quiet := last(f, "json") != "", last(f, "quiet") != ""
	if asJSON && quiet {
		return usageError("add prints --json or --quiet, not both")
	}
	after, err := afterIDs(f)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	_, err = c.json("POST", "/items", map[string]any{
		"title": pos[0], "ask": last(f, "ask"), "repo": last(f, "repo"), "priority": last(f, "priority"), "source": last(f, "source"),
		"depends_on": after, "feature": last(f, "feature"),
	}, &raw)
	if err != nil {
		return err
	}
	var it item
	if err := json.Unmarshal(raw, &it); err != nil {
		return fmt.Errorf("decode answer: %w", err)
	}
	switch {
	case asJSON:
		fmt.Fprintf(c.out, "%s\n", raw)
		return nil
	case quiet:
		fmt.Fprintln(c.out, it.ID)
		return nil
	}
	line := fmt.Sprintf("added #%d %s (%s)", it.ID, it.Title, it.Priority)
	if len(it.DependsOn) > 0 {
		line += ", after " + ids(it.DependsOn)
	}
	if it.Feature != "" {
		line += ", feature " + it.Feature
	}
	fmt.Fprintln(c.out, line)
	return nil
}

// ids lists item ids as "#1, #2".
func ids(ns []int64) string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(out, ", ")
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
	var fz []freeze
	if _, err := c.json("GET", "/freezes", nil, &fz); err != nil {
		return err
	}
	for _, f := range fz {
		fmt.Fprintf(c.out, "FROZEN %s: main is red (%s); only #%d moves, %d result(s) waiting; lift with: loop unfreeze %s\n",
			f.Repo, strings.Join(f.Jobs, ", "), f.ItemID, f.Queued, f.Repo)
	}
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
		Item      item    `json:"item"`
		Display   string  `json:"display"`
		WaitingOn []int64 `json:"waiting_on"`
		Lease     *struct {
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
	if len(d.WaitingOn) > 0 {
		fmt.Fprintf(w, "blocked:  waiting on %s\n", ids(d.WaitingOn))
	}
	repo := it.Repo
	switch {
	case repo != "":
	case it.Stage == "triage":
		repo = "not set, triage will pick"
	default:
		repo = "not set"
	}
	fmt.Fprintf(w, "priority: %s   source: %s   repo: %s\n", it.Priority, it.Source, repo)
	if it.Feature != "" {
		fmt.Fprintf(w, "feature:  %s\n", it.Feature)
	}
	if it.Ask != "" {
		fmt.Fprintf(w, "ask:      %s\n", it.Ask)
	}
	if it.Area != "" {
		fmt.Fprintf(w, "triage:   area %s, size %s, resources [%s]\n", it.Area, it.Size, strings.Join(it.Resources, ","))
	}
	if it.Risk == "hold" {
		fmt.Fprintf(w, "risk:     hold (%s)\n", it.HoldReason)
	} else {
		fmt.Fprintf(w, "risk:     %s\n", it.Risk)
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
	if t := it.Trust; t != nil {
		switch {
		case t.By == "":
			fmt.Fprintf(w, "trust:    verify refused %s: it changes trusted files (%s). Look at them, then: loop trust %d --sha %s\n",
				t.HeadSHA, strings.Join(t.Paths, ", "), it.ID, t.HeadSHA)
		case t.Used:
			fmt.Fprintf(w, "trust:    %s OK'd %s; a verify ran it (another commit needs another OK)\n", t.By, t.HeadSHA)
		default:
			fmt.Fprintf(w, "trust:    %s OK'd %s; the next verify of it runs the change\n", t.By, t.HeadSHA)
		}
		if t.CarriedFrom != "" {
			fmt.Fprintf(w, "          (carried from %s: the trusted files' diff is unchanged)\n", t.CarriedFrom)
		}
		for _, l := range strings.Split(t.Diffstat, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				fmt.Fprintf(w, "          %s\n", l)
			}
		}
	}
	if it.MergeSHA != "" {
		fmt.Fprintf(w, "merged:   %s\n", it.MergeSHA)
	}
	if it.NeedsQA {
		fmt.Fprintln(w, "qa:       waits for your check once merged")
	}
	if it.ReopenOf != 0 {
		fmt.Fprintf(w, "reopens:  #%d\n", it.ReopenOf)
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

func cmdTrust(c *client, pos []string, f map[string][]string) error {
	id, err := itemID(pos[0])
	if err != nil {
		return err
	}
	sha := last(f, "sha")
	if sha == "" {
		return usageError("trust needs --sha, the commit verify refused (loop show shows it)")
	}
	var it item
	if _, err := c.json("POST", "/items/"+id+"/trust", map[string]string{"head_sha": sha}, &it); err != nil {
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

func cmdQA(c *client, pos []string, _ map[string][]string) error {
	id, err := itemID(pos[0])
	if err != nil {
		return err
	}
	var it item
	if _, err := c.json("POST", "/items/"+id+"/qa", map[string]string{}, &it); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "#%d waits in qa for your check once merged\n", it.ID)
	return nil
}

func cmdVerified(c *client, pos []string, f map[string][]string) error {
	id, err := itemID(pos[0])
	if err != nil {
		return err
	}
	var it item
	if _, err := c.json("POST", "/items/"+id+"/verified", map[string]string{"words": last(f, "words")}, &it); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "#%d verified (done)\n", it.ID)
	return nil
}

func cmdReopen(c *client, pos []string, _ map[string][]string) error {
	id, err := itemID(pos[0])
	if err != nil {
		return err
	}
	var out struct {
		Item  item `json:"item"`
		Filed item `json:"filed"`
	}
	if _, err := c.json("POST", "/items/"+id+"/reopen", map[string]string{"words": pos[1]}, &out); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "#%d done; filed #%d (%s): %s\n", out.Item.ID, out.Filed.ID, out.Filed.Priority, out.Filed.Title)
	return nil
}

func cmdDrop(c *client, pos []string, f map[string][]string) error {
	id, err := itemID(pos[0])
	if err != nil {
		return err
	}
	var it item
	if _, err := c.json("POST", "/items/"+id+"/drop", map[string]string{"words": last(f, "words")}, &it); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "#%d dropped (closed)\n", it.ID)
	return nil
}

type freeze struct {
	Repo   string   `json:"repo"`
	ItemID int64    `json:"item_id"`
	Jobs   []string `json:"jobs"`
	Queued int      `json:"queued"`
}

func cmdUnfreeze(c *client, pos []string, _ map[string][]string) error {
	var f freeze
	if _, err := c.json("POST", "/unfreeze", map[string]string{"repo": pos[0]}, &f); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "%s unfrozen: the results that waited are applied\n", f.Repo)
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

func cmdEdit(c *client, pos []string, f map[string][]string) error {
	id, err := itemID(pos[0])
	if err != nil {
		return err
	}
	body := map[string]string{}
	for _, k := range []string{"title", "ask"} {
		if _, ok := f[k]; ok {
			body[k] = last(f, k)
		}
	}
	if len(body) == 0 {
		return usageError("edit needs --title or --ask")
	}
	var it item
	if _, err := c.json("POST", "/items/"+id+"/edit", body, &it); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "#%d edited: %s\n", it.ID, it.Title)
	return nil
}

func (c *client) waits(it item) {
	if len(it.DependsOn) == 0 {
		fmt.Fprintf(c.out, "#%d waits on nothing\n", it.ID)
		return
	}
	fmt.Fprintf(c.out, "#%d waits on %s\n", it.ID, ids(it.DependsOn))
}

func cmdLink(c *client, pos []string, f map[string][]string) error {
	id, err := itemID(pos[0])
	if err != nil {
		return err
	}
	after, err := afterIDs(f)
	if err != nil {
		return err
	}
	if len(after) == 0 {
		return usageError("link needs --after, the items to wait for")
	}
	var it item
	if _, err := c.json("POST", "/items/"+id+"/link", map[string]any{"after": after}, &it); err != nil {
		return err
	}
	c.waits(it)
	return nil
}

func cmdUnlink(c *client, pos []string, _ map[string][]string) error {
	id, err := itemID(pos[0])
	if err != nil {
		return err
	}
	dep, err := itemID(pos[1])
	if err != nil {
		return err
	}
	n, _ := strconv.ParseInt(dep, 10, 64)
	var it item
	if _, err := c.json("POST", "/items/"+id+"/unlink", map[string]any{"after": n}, &it); err != nil {
		return err
	}
	c.waits(it)
	return nil
}

func cmdResources(c *client, pos []string, _ map[string][]string) error {
	id, err := itemID(pos[0])
	if err != nil {
		return err
	}
	var it item
	if _, err := c.json("POST", "/items/"+id+"/resources", map[string]any{"resources": strings.Split(pos[1], ",")}, &it); err != nil {
		return err
	}
	if len(it.Resources) == 0 {
		fmt.Fprintf(c.out, "#%d needs no resources\n", it.ID)
	} else {
		fmt.Fprintf(c.out, "#%d needs %s\n", it.ID, strings.Join(it.Resources, ", "))
	}
	return nil
}

func cmdFeature(c *client, pos []string, _ map[string][]string) error {
	id, err := itemID(pos[0])
	if err != nil {
		return err
	}
	var it item
	if _, err := c.json("POST", "/items/"+id+"/feature", map[string]any{"feature": pos[1]}, &it); err != nil {
		return err
	}
	if it.Feature == "" {
		fmt.Fprintf(c.out, "#%d is in no feature\n", it.ID)
	} else {
		fmt.Fprintf(c.out, "#%d is in feature %s\n", it.ID, it.Feature)
	}
	return nil
}

func cmdRebuild(c *client, pos []string, f map[string][]string) error {
	id, err := itemID(pos[0])
	if err != nil {
		return err
	}
	notes := last(f, "notes")
	if len(pos) == 2 {
		if notes != "" {
			return usageError("rebuild takes the notes once: as an argument or --notes")
		}
		notes = pos[1]
	}
	if strings.TrimSpace(notes) == "" {
		return usageError("rebuild needs notes: what the builder should do")
	}
	var it item
	if _, err := c.json("POST", "/items/"+id+"/rebuild", map[string]string{"notes": notes}, &it); err != nil {
		return err
	}
	c.moved(it)
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

// cmdBudget sets (or, with no flags, shows) a provider's usage budget.
func cmdBudget(c *client, pos []string, f map[string][]string) error {
	provider := pipeline.ProviderClaude
	if len(pos) > 0 {
		provider = pos[0]
	}
	var ctl struct {
		Budgets map[string]pipeline.Budget `json:"budgets"`
	}
	if _, err := c.json("GET", "/control", nil, &ctl); err != nil {
		return err
	}
	b := ctl.Budgets[provider]
	window := func(flag string, tokens *int64, pct *int) error {
		v := last(f, flag)
		if v == "" {
			return nil
		}
		*tokens, *pct = 0, 0
		switch {
		case v == "off" || v == "0":
		case strings.HasSuffix(v, "%"):
			n, err := strconv.Atoi(strings.TrimSuffix(v, "%"))
			if err != nil {
				return usageError("--" + flag + " " + v + ": a percent is 1% to 100%")
			}
			*pct = n
		default:
			n, err := pipeline.ParseTokens(v)
			if err != nil {
				return usageError("--" + flag + " " + v + ": tokens (2M, 500k), a percent (80%) or off")
			}
			*tokens = n
		}
		return nil
	}
	if err := window("per-5h", &b.Per5h, &b.Per5hPct); err != nil {
		return err
	}
	if err := window("per-week", &b.PerWeek, &b.PerWeekPct); err != nil {
		return err
	}
	if v := last(f, "pace-at"); v != "" {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "%"))
		if err != nil {
			return usageError("--pace-at takes a percent (85)")
		}
		b.PaceAt = n
	}
	if len(f) > 0 {
		if _, err := c.json("PUT", "/control", map[string]any{"budgets": map[string]pipeline.Budget{provider: b}}, &ctl); err != nil {
			return err
		}
		b = ctl.Budgets[provider]
	}
	fmt.Fprintf(c.out, "%s budget: %s\n", provider, pipeline.DescribeBudget(b))
	return nil
}

// usageReport is the part of GET /usage the CLI prints.
type usageReport struct {
	Claude   pipeline.UsageState `json:"claude"`
	Pace     pipeline.Pace       `json:"pace"`
	Budget   pipeline.Budget     `json:"budget"`
	Forecast *time.Time          `json:"forecast"`
	Days     []struct {
		Key      string  `json:"key"`
		Provider string  `json:"provider"`
		Burn     int64   `json:"burn"`
		Tokens   int64   `json:"tokens"`
		CostUSD  float64 `json:"cost_usd"`
		Runs     int     `json:"runs"`
	} `json:"days"`
	Items []struct {
		ItemID  int64   `json:"item_id"`
		Title   string  `json:"title"`
		Burn    int64   `json:"burn"`
		CostUSD float64 `json:"cost_usd"`
		Runs    int     `json:"runs"`
	} `json:"items"`
	Hits []pipeline.UsageHit `json:"hits"`
}

func cmdUsage(c *client, _ []string, _ map[string][]string) error {
	var u usageReport
	if _, err := c.json("GET", "/usage", nil, &u); err != nil {
		return err
	}
	for _, l := range usageLines(u, time.Local) {
		fmt.Fprintln(c.out, l)
	}
	return nil
}

// usageLines are `loop usage`'s lines. Burn is claude's weighted tokens (cache reads count a
// tenth, output five times: docs/decisions/2026-10-18-usage-awareness.md).
func usageLines(u usageReport, loc *time.Location) []string {
	st := u.Claude
	of := func(burn, limit int64) string {
		if limit <= 0 {
			return pipeline.FormatTokens(burn) + " (no limit known yet)"
		}
		return fmt.Sprintf("%s of %s (%d%%)", pipeline.FormatTokens(burn), pipeline.FormatTokens(limit), burn*100/limit)
	}
	hm := func(t time.Time) string { return t.In(loc).Format("Mon 15:04") }
	var out []string
	if st.WindowStart.IsZero() {
		out = append(out, "claude 5h window: none open (the next run opens one)")
	} else {
		l := fmt.Sprintf("claude 5h window: %s, %s–%s", of(st.Burn5h, st.Limit5h), hm(st.WindowStart), st.WindowEnd.In(loc).Format("15:04"))
		if u.Forecast != nil {
			l += " · at this rate it runs out at " + u.Forecast.In(loc).Format("15:04")
		}
		out = append(out, l)
	}
	week := "rolling 7 days"
	if !st.WeekEnd.IsZero() {
		week = "resets " + hm(st.WeekEnd)
	}
	out = append(out, fmt.Sprintf("claude week: %s, %s", of(st.BurnWeek, st.LimitWeek), week))
	out = append(out, "pace: "+u.Pace.Describe(loc), "budget: "+pipeline.DescribeBudget(u.Budget))
	if len(u.Days) > 0 {
		out = append(out, "days:")
		for _, d := range u.Days {
			l := fmt.Sprintf("  %s  %-6s %7s tokens", d.Key, d.Provider, pipeline.FormatTokens(d.Tokens))
			if d.Provider == pipeline.ProviderClaude {
				l += fmt.Sprintf("  burn %s  $%.2f", pipeline.FormatTokens(d.Burn), d.CostUSD)
			}
			out = append(out, l+fmt.Sprintf("  %d runs", d.Runs))
		}
	}
	if len(u.Items) > 0 {
		out = append(out, "costliest items (7 days):")
		for _, it := range u.Items {
			out = append(out, fmt.Sprintf("  #%d %s  burn %s  $%.2f  %d runs", it.ItemID, it.Title, pipeline.FormatTokens(it.Burn), it.CostUSD, it.Runs))
		}
	}
	for i, h := range u.Hits {
		if i == 0 {
			out = append(out, "limits hit:")
		}
		l := fmt.Sprintf("  %s %s limit at burn %s", hm(h.At), h.Window, pipeline.FormatTokens(h.Burn))
		if !h.ResetAt.IsZero() {
			l += ", reset " + hm(h.ResetAt)
		}
		out = append(out, l)
	}
	return out
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
	paths := f["path"]
	for _, file := range f["paths-file"] {
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(line) != "" {
				paths = append(paths, line)
			}
		}
	}
	// Refuse locally before uploading evidence the server would then attach to a refused advance.
	if len(paths) == 0 && (pos[1] == "pr_opened" || pos[1] == "merged" || pos[1] == "hold") {
		return usageError(pos[1] + " needs the PR's changed paths: --path \"M file\" or --paths-file FILE")
	}
	for _, path := range f["evidence"] {
		if err := c.upload(id, path); err != nil {
			return fmt.Errorf("evidence %s: %w", path, err)
		}
	}
	var it item
	if _, err := c.json("POST", "/items/"+id+"/advance", map[string]any{"outcome": pos[1], "fields": fields, "changed_paths": paths}, &it); err != nil {
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
