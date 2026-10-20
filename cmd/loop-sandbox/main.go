// Command loop-sandbox is the loop's own harness (docs/phases/04b-loop-harness.md). Use it
// through scripts/loop-sandbox, which builds it with loop-guard, the fake claude and the loop CLI
// into .sandbox/<name>/bin first:
//
//	loop-sandbox run --dir D [scenario.json | dir]...   play scenarios, each on a fresh stack
//	loop-sandbox transcript --dir D [--run R] [--scenario S] [--item N] [--live]
//	loop-sandbox serve --dir D                          the long-lived sandbox (`up`)
//	loop-sandbox inject --dir D reply|press|command ...  act in the live sandbox's fake Slack
//
// Nothing here reads real tokens or talks to real Slack or GitHub.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/sandbox"
	"github.com/alexberardi/jarvis-loop/internal/slack"
	"github.com/alexberardi/jarvis-loop/internal/slackfake"
)

const usage = `usage: loop-sandbox <command> [flags]

  run --dir D [--out DIR] [scenario.json | dir ...]   (default: scenarios/)
  transcript --dir D [--run DIR] [--scenario S] [--item N] [--live]
  serve --dir D
  inject --dir D reply --item N [--as other] TEXT
  inject --dir D press --item N --button ACTION_ID [--as other]
  inject --dir D command [--as other] TEXT
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "run":
		var ok bool
		ok, err = runCmd(args[1:], stdout)
		if err == nil && !ok {
			return 1
		}
	case "transcript":
		err = transcriptCmd(args[1:], stdout)
	case "serve":
		err = serveCmd(args[1:], stderr)
	case "inject":
		err = injectCmd(args[1:], stdout)
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "loop-sandbox:", err)
		return 1
	}
	return 0
}

// binaries are the programs next to this one in the sandbox's bin dir.
func binaries() (sandbox.Binaries, error) {
	self, err := os.Executable()
	if err != nil {
		return sandbox.Binaries{}, err
	}
	dir := filepath.Dir(self)
	b := sandbox.Binaries{Claude: filepath.Join(dir, "fakeclaude"), Guard: filepath.Join(dir, "loop-guard")}
	for _, p := range []string{b.Claude, b.Guard} {
		if _, err := os.Stat(p); err != nil {
			return b, fmt.Errorf("%s is missing; build with scripts/loop-sandbox", p)
		}
	}
	return b, nil
}

func runCmd(args []string, stdout io.Writer) (bool, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	dir := fs.String("dir", "", "the sandbox dir (.sandbox/<name>)")
	out := fs.String("out", "", "where to write the run (default <dir>/runs/<time>)")
	if err := fs.Parse(args); err != nil {
		return false, err
	}
	if *dir == "" {
		return false, errors.New("--dir is required")
	}
	paths := fs.Args()
	if len(paths) == 0 {
		paths = []string{"scenarios"}
	}
	scs, err := sandbox.LoadScenarios(paths)
	if err != nil {
		return false, err
	}
	if len(scs) == 0 {
		return false, errors.New("no scenarios")
	}
	bin, err := binaries()
	if err != nil {
		return false, err
	}
	runs := filepath.Join(*dir, "runs")
	if *out == "" {
		*out = filepath.Join(runs, time.Now().UTC().Format("20060102T150405Z"))
	}
	start := time.Now()
	results, err := sandbox.RunAll(scs, sandbox.RunOptions{Out: *out, Bin: bin}, stdout)
	if err != nil {
		return false, err
	}
	latest := filepath.Join(runs, "latest")
	os.MkdirAll(runs, 0o700)
	os.Remove(latest)
	if abs, err := filepath.Abs(*out); err == nil {
		os.Symlink(abs, latest)
	}
	pass := 0
	for _, r := range results {
		if r.Pass {
			pass++
		}
	}
	fmt.Fprintf(stdout, "\n%d of %d scenarios pass in %.1fs. Results: %s/results.md; transcripts: %s/<scenario>/transcript.md\n",
		pass, len(results), time.Since(start).Seconds(), *out, *out)
	return pass == len(results), nil
}

func transcriptCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("transcript", flag.ContinueOnError)
	dir := fs.String("dir", "", "the sandbox dir")
	runDir := fs.String("run", "", "a run dir (default: the latest run)")
	scenario := fs.String("scenario", "", "only this scenario")
	item := fs.Int64("item", 0, "only this item's thread")
	live := fs.Bool("live", false, "the live sandbox (up) instead of a run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("--dir is required")
	}
	if *live {
		var msgs []slackfake.Message
		if err := control(*dir, "GET", "/messages", nil, &msgs); err != nil {
			return err
		}
		return printTranscript(stdout, "live sandbox", msgs, *item)
	}
	if *runDir == "" {
		*runDir = filepath.Join(*dir, "runs", "latest")
	}
	files, _ := filepath.Glob(filepath.Join(*runDir, "*", "slack.json"))
	if len(files) == 0 {
		return fmt.Errorf("no recorded run in %s; run scenarios first", *runDir)
	}
	shown := 0
	for _, f := range files {
		name := filepath.Base(filepath.Dir(f))
		if *scenario != "" && name != *scenario {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var msgs []slackfake.Message
		if err := json.Unmarshal(b, &msgs); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		if err := printTranscript(stdout, name, msgs, *item); err != nil {
			return err
		}
		shown++
	}
	if shown == 0 {
		return fmt.Errorf("no scenario %q in %s", *scenario, *runDir)
	}
	return nil
}

// printTranscript renders one recording, or one item's thread (found by its parent, "#N …").
func printTranscript(w io.Writer, title string, msgs []slackfake.Message, item int64) error {
	thread := ""
	if item != 0 {
		re := regexp.MustCompile(`^#` + strconv.FormatInt(item, 10) + ` `)
		for _, m := range msgs {
			if m.Channel == slack.ChannelLoop && m.ThreadTS == "" && m.User == slackfake.BotUser && re.MatchString(m.Text) {
				thread = m.TS
			}
		}
		if thread == "" {
			fmt.Fprintf(w, "== %s: no thread for item #%d\n\n", title, item)
			return nil
		}
	}
	fmt.Fprintf(w, "== %s\n%s\n", title, sandbox.Render(msgs, thread))
	return nil
}

// ---- the live sandbox ----

type liveInfo struct {
	Control string `json:"control"` // the control API's address
	Token   string `json:"token"`
}

func liveDir(dir string) string { return filepath.Join(dir, "live") }

func serveCmd(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dir := fs.String("dir", "", "the sandbox dir")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("--dir is required")
	}
	bin, err := binaries()
	if err != nil {
		return err
	}
	ld := liveDir(*dir)
	os.RemoveAll(ld)
	logf, err := os.OpenFile(filepath.Join(*dir, "live.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	s, err := sandbox.StartStack(sandbox.Options{Dir: ld, Bin: bin, RealClock: true, Log: logf, Qwen: true})
	if err != nil {
		return err
	}
	defer s.Close()

	// The `loop` CLI's environment for this sandbox (0600: it holds the sandbox user's token).
	env := fmt.Sprintf("export LOOP_URL=%s\nexport LOOP_TOKEN=%s\n", s.LoopdURL, s.Secrets[0])
	if err := os.WriteFile(filepath.Join(ld, "loop.env"), []byte(env), 0o600); err != nil {
		return err
	}
	// As a hand runner, for the stages no runner works yet (`loop claim`, `loop advance`).
	hand := fmt.Sprintf("export LOOP_URL=%s\nexport LOOP_TOKEN=%s\n", s.LoopdURL, s.Secrets[2])
	if err := os.WriteFile(filepath.Join(ld, "hand.env"), []byte(hand), 0o600); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	tok := make([]byte, 16)
	rand.Read(tok)
	info := liveInfo{Control: ln.Addr().String(), Token: hex.EncodeToString(tok)}
	b, _ := json.Marshal(info)
	if err := os.WriteFile(filepath.Join(ld, "control.json"), b, 0o600); err != nil {
		return err
	}
	srv := &http.Server{Handler: controlHandler(s, info.Token), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()
	if err := os.WriteFile(filepath.Join(ld, "ready"), []byte(s.LoopdURL+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "loop-sandbox: live sandbox up: loopd %s (loopback), fake Slack %s, remote %s\n", s.LoopdURL, s.Slack.URL(), s.Remote)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	return nil
}

type injectReq struct {
	Kind   string `json:"kind"`
	As     string `json:"as"`
	Item   int64  `json:"item"`
	Text   string `json:"text"`
	Button string `json:"button"`
}

func controlHandler(s *sandbox.Stack, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /messages", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(s.Slack.Messages())
	})
	mux.HandleFunc("POST /inject", func(w http.ResponseWriter, r *http.Request) {
		var in injectReq
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out, err := inject(s, in)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"answer": out})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func inject(s *sandbox.Stack, in injectReq) (string, error) {
	user := slack.Approver
	switch in.As {
	case "", "approver":
	case "other":
		user = sandbox.Other
	default:
		return "", fmt.Errorf("--as %q: approver or other", in.As)
	}
	switch in.Kind {
	case "command":
		return s.Slack.Command(user, slack.ChannelLoop, "/loop", in.Text)
	case "reply", "press":
		ts := s.Thread(in.Item)
		if ts == "" {
			return "", fmt.Errorf("item #%d has no thread", in.Item)
		}
		if in.Kind == "reply" {
			_, err := s.Slack.Reply(user, slack.ChannelLoop, ts, in.Text)
			return "", err
		}
		msgs := s.Slack.Messages()
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			if (m.TS == ts || m.ThreadTS == ts) && m.User == slackfake.BotUser {
				if err := s.Slack.Press(user, m.Channel, m.TS, in.Button); err == nil {
					return "", nil
				} else if !errors.Is(err, slackfake.ErrNoElement) {
					return "", err
				}
			}
		}
		return "", fmt.Errorf("no message in #%d's thread has %s", in.Item, in.Button)
	}
	return "", fmt.Errorf("unknown inject %q", in.Kind)
}

func control(dir, method, path string, in, out any) error {
	b, err := os.ReadFile(filepath.Join(liveDir(dir), "control.json"))
	if err != nil {
		return errors.New("no live sandbox: run scripts/loop-sandbox up first")
	}
	var info liveInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		raw, _ := json.Marshal(in)
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, "http://"+info.Control+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+info.Token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("the live sandbox does not answer (%v); is it up?", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func injectCmd(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("inject", flag.ContinueOnError)
	dir := fs.String("dir", "", "the sandbox dir")
	// The subcommand may come before or after --dir.
	var kind string
	rest := args
	if !strings.HasPrefix(args[0], "-") {
		kind, rest = args[0], args[1:]
	}
	as := fs.String("as", "approver", "approver or other")
	item := fs.Int64("item", 0, "the item")
	button := fs.String("button", "", "the button's action_id (approve, keep_holding, cancel_approval, …)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	pos := fs.Args()
	if kind == "" && len(pos) > 0 {
		kind, pos = pos[0], pos[1:]
	}
	if *dir == "" {
		return errors.New("--dir is required")
	}
	req := injectReq{Kind: kind, As: *as, Item: *item, Button: *button, Text: strings.Join(pos, " ")}
	var out map[string]string
	if err := control(*dir, "POST", "/inject", req, &out); err != nil {
		return err
	}
	if out["answer"] != "" {
		fmt.Fprintln(stdout, out["answer"])
	}
	return nil
}
