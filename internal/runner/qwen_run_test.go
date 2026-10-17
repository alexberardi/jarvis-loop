package runner

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

const qwenAppKey = "prod-app-key-must-stay-with-the-runner"

// fakeJarvisd is jarvisd's OpenAI route: it answers a streamed completion to requests with the
// app's credentials, 401 otherwise, and counts both.
type fakeJarvisd struct {
	mu        sync.Mutex
	ok, wrong int
	auth      []string // the Authorization headers that reached it
}

func (f *fakeJarvisd) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	if r.URL.Path != "/v1/chat/completions" || r.Header.Get("X-Jarvis-App-Id") != "loop-qwen" || r.Header.Get("X-Jarvis-App-Key") != qwenAppKey {
		f.wrong++
		http.Error(w, `{"detail":"Missing app credentials"}`, http.StatusUnauthorized)
		return
	}
	f.ok++
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
}

// qwenRig is a rig whose runner has a qwen config pointing at a fake jarvisd, with the test
// binary as opencode.
func qwenRig(t *testing.T, scenario string) (*rig, *fakeJarvisd) {
	g := newRig(t, scenario)
	up := &fakeJarvisd{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	self, _ := os.Executable()
	q := &QwenConfig{BaseURL: srv.URL + "/v1", CredentialsFile: credsFile(t, "PROD_APP_ID=loop-qwen\nPROD_APP_KEY="+qwenAppKey+"\n", 0o600), OpenCode: self}
	if err := q.normalize(); err != nil {
		t.Fatal(err)
	}
	g.r.Cfg.Qwen = q
	// The fake keeps its sessions in the scripts dir, which may sit under the fence's private /tmp.
	g.r.Cfg.VerifyWritable = append(g.r.Cfg.VerifyWritable, g.scripts)
	g.r.Cfg.VerifyMask = append(g.r.Cfg.VerifyMask, q.CredentialsFile)
	if err := g.r.ProbeFence(); err != nil {
		t.Skipf("no fence here: %v", err)
	}
	return g, up
}

// setBuilder marks an item as a plan piece's with that builder (as the hand-off files it).
func (g *rig) setBuilder(id int64, builder string) {
	g.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(filepath.Dir(g.remote), "..", "loopd", "loop.db")+"?_pragma=busy_timeout(10000)")
	if err != nil {
		g.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE items SET builder = ? WHERE id = ?`, builder, id); err != nil {
		g.t.Fatal(err)
	}
}

// bounce is a review agent bouncing the item's PR.
func (g *rig) bounce(id int64, notes string) {
	g.t.Helper()
	c, err := g.st.Claim(g.ctx, "devbox", pipeline.ClaimRequest{Runner: "devbox", Agent: "reviewer", Stages: []pipeline.Stage{pipeline.Review}})
	if err != nil || c == nil || c.Item.ID != id {
		g.t.Fatalf("review claim %v %v", c, err)
	}
	if _, err := g.st.Advance(g.ctx, "devbox", id, pipeline.Bounce, map[string]string{"notes": notes}, nil); err != nil {
		g.t.Fatal(err)
	}
}

func runArgv(t *testing.T, g *rig, id int64, n int) []string {
	t.Helper()
	runs, _ := filepath.Glob(filepath.Join(g.r.Cfg.WorkRoot, "runs", fmt.Sprintf("%d-build-*", id)))
	if len(runs) < n {
		t.Fatalf("%d build runs, want %d", len(runs), n)
	}
	var argv []string
	b, err := os.ReadFile(filepath.Join(runs[n-1], "argv.json"))
	if err != nil || json.Unmarshal(b, &argv) != nil {
		t.Fatalf("argv of run %d: %v", n, err)
	}
	return argv
}

// A qwen item builds with OpenCode in the fence, through the runner's proxy (which alone adds the
// app credentials), to the same result contract; a bounce resumes the same session.
func TestQwenBuildRunsOpenCode(t *testing.T) {
	g, up := qwenRig(t, fakeclaude.Good)
	id := g.toBuild("Add a feature")
	g.setBuilder(id, pipeline.BuilderQwen)
	if !g.runOnce() {
		t.Fatal("nothing claimed")
	}
	d := g.item(id)
	if d.Item.Stage != pipeline.Review {
		t.Fatalf("stage %s (%s); log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	sess := fmt.Sprintf("ses_fake_%d_good", id)
	if d.Item.BuildSession != sess || d.Item.Builder != pipeline.BuilderQwen {
		t.Fatalf("session %q builder %q", d.Item.BuildSession, d.Item.Builder)
	}
	ev := g.advanced(id)
	for k, want := range map[string]string{"builder": "qwen", "session_id": sess, "qwen_requests": "1", "qwen_tokens_input": "12000",
		"qwen_tokens_output": "800"} {
		if ev.Data[k] != want {
			t.Errorf("%s = %v, want %s", k, ev.Data[k], want)
		}
	}
	for _, k := range []string{"usage_input", "total_cost_usd", "permission_mode"} {
		if _, ok := ev.Data[k]; ok {
			t.Errorf("a qwen run reports claude's %s", k)
		}
	}
	if up.ok != 1 || up.wrong != 0 {
		t.Fatalf("jarvisd saw %d good and %d bad requests", up.ok, up.wrong)
	}
	if up.auth[0] != "" {
		t.Fatalf("the run token reached jarvisd: %q", up.auth[0])
	}
	if got := runArgv(t, g, id, 1); strings.Join(got, " ") != "run --pure --format json -m qwen/background" {
		t.Fatalf("argv %v", got)
	}
	spec, _ := g.prs.byNumber(7)
	if spec.Title != "Add a feature" || !strings.Contains(g.prs.bodies[7], "### Verification") {
		t.Fatalf("PR %+v", spec)
	}
	// The app key is in no file the agent's side has: the run dir, the worktree's .loop, the
	// item's OpenCode home.
	for _, root := range []string{filepath.Join(g.r.Cfg.WorkRoot, "runs"), filepath.Join(g.r.Cfg.WorkRoot, "opencode")} {
		filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() {
				if b, _ := os.ReadFile(p); strings.Contains(string(b), qwenAppKey) {
					t.Errorf("%s holds the app key", p)
				}
			}
			return nil
		})
	}

	// A bounce: the next build resumes the session, still with OpenCode.
	g.bounce(id, "feature.txt:1: add a test")
	if !g.runOnce() {
		t.Fatal("nothing claimed after the bounce")
	}
	d = g.item(id)
	if d.Item.Stage != pipeline.Review || d.Item.Builder != pipeline.BuilderQwen {
		t.Fatalf("after the resumed run: %s %s; log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	if got := runArgv(t, g, id, 2); strings.Join(got, " ") != "run --pure --format json -m qwen/background -s "+sess {
		t.Fatalf("resumed argv %v", got)
	}
	if ev := g.advanced(id); ev.Data["session_id"] != sess || ev.Data["builder"] != "qwen" {
		t.Fatalf("resumed run data %v", ev.Data)
	}
	g.userUntouched()

	// The second strike: the builder is claude for the rest.
	g.bounce(id, "still no test")
	d = g.item(id)
	if d.Item.Builder != pipeline.BuilderClaude || d.Item.BuildSession != "" || len(g.events(id, "builder_fallback")) != 1 {
		t.Fatalf("after the second strike: %+v", d.Item)
	}
}

// Jarvisd refusing the credentials: the qwen run fails with OpenCode's error, to the user.
func TestQwenBuildUpstreamRefuses(t *testing.T) {
	g, up := qwenRig(t, fakeclaude.Good)
	os.WriteFile(g.r.Cfg.Qwen.CredentialsFile, []byte("PROD_APP_ID=loop-qwen\nPROD_APP_KEY=stale\n"), 0o600)
	id := g.toBuild("Add a feature")
	g.setBuilder(id, pipeline.BuilderQwen)
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.NeedsYou || !strings.Contains(d.Item.Question, "OpenCode error") || !strings.Contains(d.Item.Question, "401") {
		t.Fatalf("stage %s, question %q", d.Item.Stage, d.Item.Question)
	}
	if strings.Contains(d.Item.Question, "stale") || up.wrong != 1 {
		t.Fatalf("question %q, refused %d", d.Item.Question, up.wrong)
	}
}

// A qwen item on a runner with no qwen config builds with claude, and says so.
func TestQwenItemWithoutQwenConfig(t *testing.T) {
	g := newRig(t, fakeclaude.Good)
	id := g.toBuild("Add a feature")
	g.setBuilder(id, pipeline.BuilderQwen)
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Review || d.Item.Builder != pipeline.BuilderQwen || d.Item.BuildSession != "" {
		t.Fatalf("item %+v; log:\n%s", d.Item, g.logs.String())
	}
	ev := g.advanced(id)
	if ev.Data["builder"] != "claude" || ev.Data["session_id"] != "fake-session-good" {
		t.Fatalf("data %v", ev.Data)
	}
	fb := g.events(id, "builder_fallback")
	if len(fb) != 1 || fb[0].Data["this_run_only"] != true || !strings.Contains(fmt.Sprint(fb[0].Data["reason"]), "no qwen config") {
		t.Fatalf("builder_fallback %+v", fb)
	}
}

// No guard runs in OpenCode, so the fence keeps the worktree's git plumbing read-only: the agent
// cannot repoint .git, commondir or config.worktree (or rename the admin dir away) at a repository
// with a config of its own, and the runner's own git still commits and pushes the real work.
func TestQwenBuildCannotRepointGit(t *testing.T) {
	g, _ := qwenRig(t, fakeclaude.GitTamper)
	id := g.toBuild("Add a feature")
	g.setBuilder(id, pipeline.BuilderQwen)
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Review {
		t.Fatalf("stage %s (%s); log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	runs, _ := filepath.Glob(filepath.Join(g.r.Cfg.WorkRoot, "runs", fmt.Sprintf("%d-build-*", id)))
	stream := g.read(filepath.Join(runs[0], "stream.jsonl"))
	for _, p := range []string{`"file_path":"[^"]*/\.git"`, `"file_path":"[^"]*/commondir"`, `"file_path":"[^"]*/config\.worktree"`} {
		if !regexp.MustCompile(p).MatchString(stream) {
			t.Errorf("the agent never tried %s", p)
		}
	}
	if n := strings.Count(stream, "read-only file system"); n < 3 {
		t.Errorf("%d writes refused, want the 3 writes and the renames:\n%s", n, stream)
	}
	if h := g.remoteHead(d.Item.Branch); h == "" || g.prs.bodies[7] == "" {
		t.Fatalf("the runner pushed %q and opened %v; log:\n%s", h, g.prs.bodies, g.logs.String())
	}
	if len(g.events(id, "builder_fallback")) != 0 || strings.Contains(g.logs.String(), endGitTampered) {
		t.Fatalf("log:\n%s", g.logs.String())
	}
}

// The mirror is shared with the runner's other lanes, so the fence keeps it read-only but for the
// objects, refs and logs a commit writes: the agent cannot give it a commondir, rewrite another
// worktree's plumbing or add an admin dir, and the build still commits, pushes and opens its PR.
func TestQwenBuildCannotPlantInMirror(t *testing.T) {
	g, _ := qwenRig(t, fakeclaude.GitTamperShared)
	mirror := g.r.mirrorPath("demo")
	if err := g.r.Git.EnsureMirror(g.ctx, mirror, g.r.Cfg.Repos["demo"]); err != nil {
		t.Fatal(err)
	}
	// Another lane's worktree of the same mirror (a land-conflict merge, a plan, a second build).
	gitT(t, mirror, "worktree", "add", "-q", "--detach", filepath.Join(t.TempDir(), "other"), "origin/main")
	otherCommondir := filepath.Join(mirror, "worktrees", "other", "commondir")
	was := g.read(otherCommondir)
	id := g.toBuild("Add a feature")
	g.setBuilder(id, pipeline.BuilderQwen)
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.Review {
		t.Fatalf("stage %s (%s); log:\n%s", d.Item.Stage, d.Item.Question, g.logs.String())
	}
	runs, _ := filepath.Glob(filepath.Join(g.r.Cfg.WorkRoot, "runs", fmt.Sprintf("%d-build-*", id)))
	stream := g.read(filepath.Join(runs[0], "stream.jsonl"))
	for _, p := range []string{mirror + "/commondir", otherCommondir, mirror + "/worktrees/planted/gitdir"} {
		if !strings.Contains(stream, `"file_path":"`+p+`"`) {
			t.Errorf("the agent never tried %s", p)
		}
	}
	if n := strings.Count(stream, "read-only file system"); n != 3 {
		t.Errorf("%d writes refused, want 3:\n%s", n, stream)
	}
	if _, err := os.Stat(filepath.Join(mirror, "commondir")); !os.IsNotExist(err) {
		t.Errorf("mirror commondir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mirror, "worktrees", "planted")); !os.IsNotExist(err) {
		t.Errorf("planted admin dir: %v", err)
	}
	if got := g.read(otherCommondir); got != was {
		t.Errorf("the other worktree's commondir is %q, was %q", got, was)
	}
	if h := g.remoteHead(d.Item.Branch); h == "" || g.prs.bodies[7] == "" {
		t.Fatalf("the runner pushed %q and opened %v; log:\n%s", h, g.prs.bodies, g.logs.String())
	}
	if strings.Contains(g.logs.String(), endGitTampered) {
		t.Fatalf("log:\n%s", g.logs.String())
	}
}

// The second layer: should the mirror be writable after all (here a verify_writable that covers
// it), a run that gives it a commondir fails as a boundary check, and the runner removes it
// before its own git reads the mirror.
func TestQwenBuildMirrorCommondir(t *testing.T) {
	g, _ := qwenRig(t, fakeclaude.GitTamperMirror)
	g.r.Cfg.VerifyWritable = append(g.r.Cfg.VerifyWritable, g.r.mirrorPath("demo"))
	id := g.toBuild("Add a feature")
	g.setBuilder(id, pipeline.BuilderQwen)
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.NeedsYou || !strings.Contains(d.Item.Question, endGitTampered) || !strings.Contains(d.Item.Question, "commondir") {
		t.Fatalf("stage %s, question %q", d.Item.Stage, d.Item.Question)
	}
	mirrors, _ := filepath.Glob(filepath.Join(g.r.Cfg.WorkRoot, "mirrors", "*.git"))
	for _, m := range mirrors {
		if _, err := os.Stat(filepath.Join(m, "commondir")); !os.IsNotExist(err) {
			t.Fatalf("%s/commondir left behind: %v", m, err)
		}
	}
	if h := g.remoteHead(d.Item.Branch); h != "" {
		t.Fatalf("the runner pushed %s after the tamper", h)
	}
}
