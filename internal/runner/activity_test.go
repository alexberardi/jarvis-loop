package runner

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// TestActivityFromStream plays a recorded stream: the to-do list, the tool calls as tool +
// description or worktree-relative path, the latest text; never a command, a file's contents or
// a path outside the worktree.
func TestActivityFromStream(t *testing.T) {
	clk := NewFakeClock(t0)
	r := &Runner{Clock: clk}
	ru := &run{stage: StageBuild, wt: "/work/68-build", dir: t.TempDir(), started: t0}
	if err := os.WriteFile(filepath.Join(ru.dir, "armed"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open("testdata/activity_stream.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var st streamStats
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if why, detail := r.handle(ru, &st, sc.Bytes()); why != "" {
			t.Fatalf("handle: %s %s", why, detail)
		}
	}
	clk.Advance(95 * time.Second)
	a := r.agentActivity(ru, &st)
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	if a.Phase != "build agent" || a.ElapsedS != 95 || a.ToolCalls != 7 {
		t.Fatalf("phase %q elapsed %d calls %d", a.Phase, a.ElapsedS, a.ToolCalls)
	}
	wantTodos := []pipeline.Todo{
		{Text: "Write the failing test", Status: pipeline.TodoCompleted},
		{Text: "Fix the <b>parser</b>", Status: pipeline.TodoInProgress},
		{Text: "Running the race suite", Status: pipeline.TodoPending}, // no content: its activeForm
	}
	if !equalJSON(a.Todos, wantTodos) {
		t.Fatalf("todos %+v", a.Todos)
	}
	wantRecent := []string{"Bash: Run the tests", "Bash", "Edit: internal/x/parse.go", "Read: hosts", "Grep"}
	if strings.Join(a.Recent, "|") != strings.Join(wantRecent, "|") {
		t.Fatalf("recent %q", a.Recent)
	}
	if !strings.HasPrefix(a.Message, "Done reading. Long explanation.") || utf8.RuneCountInString(a.Message) != pipeline.MaxActivityMessage ||
		!strings.HasSuffix(a.Message, "…") {
		t.Fatalf("message (%d) %q", utf8.RuneCountInString(a.Message), a.Message)
	}
	b, _ := json.Marshal(a)
	for _, marker := range []string{"SECRET_CMD_MARKER", "FILE_CONTENTS_MARKER", "OLD_TEXT_MARKER", "NEW_TEXT_MARKER", "OUTSIDE_DIR_MARKER", "PATTERN_MARKER", "/work/68-build"} {
		if strings.Contains(string(b), marker) {
			t.Errorf("the activity holds %s: %s", marker, b)
		}
	}

	// A plan turn: its persona and plan.
	ru.plan, ru.stage = &pipeline.Plan{ID: 7}, pipeline.PersonaEngineering
	if a := r.agentActivity(ru, &st); a.Phase != "plan agent" || a.Persona != pipeline.PersonaEngineering || a.PlanID != 7 {
		t.Fatalf("plan activity %+v", a)
	}
	// Verify: the step and its time.
	if a := r.stepActivity("verify", "loop-sandbox", t0, t0.Add(80*time.Second)); a.Phase != "verify" || a.Step != "loop-sandbox" ||
		a.StepElapsedS != 15 || a.ElapsedS != 95 {
		t.Fatalf("verify activity %+v", a)
	}
}

func TestActivityKeepsTheLastCalls(t *testing.T) {
	var a liveActivity
	for i := range 8 {
		a.observe("/wt", toolUse{Name: "Bash", Input: json.RawMessage(`{"description":"step ` + string(rune('a'+i)) + `"}`)})
	}
	if strings.Join(a.recent, "|") != "Bash: step d|Bash: step e|Bash: step f|Bash: step g|Bash: step h" {
		t.Fatalf("recent %q", a.recent)
	}
	a.observe("/wt", toolUse{Name: "Write", Input: json.RawMessage(`{"file_path":"/wt/../wt2/x.go","content":"x"}`)})
	a.observe("/wt", toolUse{Name: "Write", Input: json.RawMessage(`{"file_path":"rel/y.go"}`)})
	if got := a.recent[3:]; got[0] != "Write: x.go" || got[1] != "Write: rel/y.go" {
		t.Fatalf("paths %q", got)
	}
}

func TestActivityFitsTheByteCap(t *testing.T) {
	var a liveActivity
	long := strings.Repeat("<é>", pipeline.MaxActivityTodo)
	var todos []any
	for range pipeline.MaxActivityTodos {
		todos = append(todos, map[string]string{"content": long, "status": "pending"})
	}
	in, _ := json.Marshal(map[string]any{"todos": todos})
	a.observe("/wt", toolUse{Name: "TodoWrite", Input: in})
	a.observe("/wt", toolUse{Name: "Bash", Input: json.RawMessage(`{"description":"` + long + `"}`)})
	r := &Runner{Clock: NewFakeClock(t0)}
	act := r.agentActivity(&run{stage: StageBuild, started: t0}, &streamStats{act: a})
	if err := act.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(act.Todos) == 0 || len(act.Todos) == pipeline.MaxActivityTodos {
		t.Fatalf("%d to-dos kept", len(act.Todos))
	}
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
