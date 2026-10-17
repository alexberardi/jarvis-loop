package pipeline

import (
	"encoding/json"
	"fmt"
	"slices"
	"unicode/utf8"
)

// Activity is what a running agent is doing right now, sent with every heartbeat and shown on the
// board's "Working now" cards. The runner builds it from the agent's stream: never a command's
// text or a file's contents, only tool names, Bash's own description and worktree-relative paths.
type Activity struct {
	// Phase is where the run is: "<stage> agent" (triage, build, review), "plan agent",
	// "setup", "verify", "land".
	Phase string `json:"phase"`
	// Persona and PlanID are a plan turn's.
	Persona string `json:"persona,omitempty"`
	PlanID  int64  `json:"plan_id,omitempty"`
	// ElapsedS is the seconds since the run started.
	ElapsedS int64 `json:"elapsed_s"`
	// Step and StepElapsedS are a verify step's name and seconds (or land's wait).
	Step         string `json:"step,omitempty"`
	StepElapsedS int64  `json:"step_elapsed_s,omitempty"`
	ToolCalls    int    `json:"tool_calls"`
	// Todos is the agent's to-do list from its latest TodoWrite.
	Todos []Todo `json:"todos,omitempty"`
	// Recent is the last few tool calls, oldest first, as "<tool>: <what>".
	Recent []string `json:"recent,omitempty"`
	// Message is the agent's latest text, cut to MaxActivityMessage.
	Message string `json:"message,omitempty"`
}

// Todo is one entry of the agent's to-do list.
type Todo struct {
	Text   string `json:"text"`
	Status string `json:"status"`
}

// The to-do statuses (TodoWrite's).
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoCompleted  = "completed"
)

// The activity's limits: loopd refuses an activity past them, the runner cuts to them.
const (
	MaxActivityTodos   = 20
	MaxActivityTodo    = 200 // runes per to-do
	MaxActivityRecent  = 5
	MaxActivityCall    = 200 // runes per recent call
	MaxActivityMessage = 300 // runes
	MaxActivityName    = 64  // runes: phase, persona, step
	MaxActivityBytes   = 8 << 10
)

// Validate checks an activity against the limits.
func (a Activity) Validate() error {
	for name, s := range map[string]string{"phase": a.Phase, "persona": a.Persona, "step": a.Step} {
		if err := checkText(name, s, MaxActivityName); err != nil {
			return err
		}
	}
	if a.ElapsedS < 0 || a.StepElapsedS < 0 || a.ToolCalls < 0 || a.PlanID < 0 {
		return fmt.Errorf("activity: negative numbers")
	}
	if len(a.Todos) > MaxActivityTodos {
		return fmt.Errorf("activity: more than %d to-dos", MaxActivityTodos)
	}
	for _, t := range a.Todos {
		if err := checkText("to-do", t.Text, MaxActivityTodo); err != nil {
			return err
		}
		if !slices.Contains([]string{TodoPending, TodoInProgress, TodoCompleted}, t.Status) {
			return fmt.Errorf("activity: to-do status %q is not pending, in_progress or completed", clipRunes(t.Status, 20))
		}
	}
	if len(a.Recent) > MaxActivityRecent {
		return fmt.Errorf("activity: more than %d recent calls", MaxActivityRecent)
	}
	for _, c := range a.Recent {
		if err := checkText("recent call", c, MaxActivityCall); err != nil {
			return err
		}
	}
	if err := checkText("message", a.Message, MaxActivityMessage); err != nil {
		return err
	}
	if b, _ := json.Marshal(a); len(b) > MaxActivityBytes {
		return fmt.Errorf("activity: %d bytes encoded, more than %d", len(b), MaxActivityBytes)
	}
	return nil
}

func checkText(name, s string, max int) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("activity: %s is not UTF-8", name)
	}
	if n := utf8.RuneCountInString(s); n > max {
		return fmt.Errorf("activity: %s has %d characters, more than %d", name, n, max)
	}
	return nil
}

// ClipRunes cuts s to at most n runes, the last one "…" when it was cut.
func ClipRunes(s string, n int) string { return clipRunes(s, n) }

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
