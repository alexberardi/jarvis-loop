package runner

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// liveActivity is what the run's stream says the agent is doing (its to-do list, its last tool
// calls), sent with every heartbeat as part of a pipeline.Activity. It keeps only tool names,
// Bash's own description and worktree-relative paths: never a command's text or a file's
// contents.
type liveActivity struct {
	todos  []pipeline.Todo
	recent []string
}

// toolUse is an assistant message's tool_use block.
type toolUse struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// observe takes one tool_use block; wt is the agent's working directory.
func (a *liveActivity) observe(wt string, tu toolUse) {
	var in struct {
		Description  string `json:"description"`
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Todos        []struct {
			Content    string `json:"content"`
			ActiveForm string `json:"activeForm"`
			Status     string `json:"status"`
		} `json:"todos"`
	}
	_ = json.Unmarshal(tu.Input, &in)
	name := oneLineText(tu.Name)
	if name == "" {
		return
	}
	call := name
	switch name {
	case "TodoWrite":
		a.todos = a.todos[:0]
		for _, t := range in.Todos {
			if len(a.todos) == pipeline.MaxActivityTodos {
				break
			}
			text := oneLineText(t.Content)
			if text == "" {
				text = oneLineText(t.ActiveForm)
			}
			st := t.Status
			if st != pipeline.TodoInProgress && st != pipeline.TodoCompleted {
				st = pipeline.TodoPending
			}
			a.todos = append(a.todos, pipeline.Todo{Text: pipeline.ClipRunes(text, pipeline.MaxActivityTodo), Status: st})
		}
		return // the list is shown as itself
	case "Bash", "Agent", "Task":
		// Bash's description is the agent's own summary of the command; the command is never kept.
		if d := oneLineText(in.Description); d != "" {
			call += ": " + d
		}
	case "Read", "Edit", "Write", "MultiEdit", "NotebookEdit":
		p := in.FilePath
		if p == "" {
			p = in.NotebookPath
		}
		if p != "" {
			call += ": " + relPath(wt, p)
		}
	}
	a.recent = append(a.recent, pipeline.ClipRunes(call, pipeline.MaxActivityCall))
	if n := len(a.recent) - pipeline.MaxActivityRecent; n > 0 {
		a.recent = append(a.recent[:0], a.recent[n:]...)
	}
}

// relPath is p relative to the worktree; a path outside it is only its base name.
func relPath(wt, p string) string {
	p = oneLineText(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(wt, p)
	}
	rel, err := filepath.Rel(wt, filepath.Clean(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return filepath.Base(p)
	}
	return rel
}

// oneLineText is s on one line, spaces collapsed.
func oneLineText(s string) string { return strings.Join(strings.Fields(s), " ") }

// agentActivity is the activity of a run whose agent is running.
func (r *Runner) agentActivity(ru *run, st *streamStats) *pipeline.Activity {
	a := &pipeline.Activity{Phase: ru.stage + " agent", ElapsedS: r.since(ru.started), ToolCalls: st.toolResults,
		Message: pipeline.ClipRunes(oneLineText(st.lastText), pipeline.MaxActivityMessage)}
	if ru.plan != nil {
		a.Phase, a.Persona, a.PlanID = "plan agent", ru.stage, ru.plan.ID
	}
	a.Todos = append([]pipeline.Todo(nil), st.act.todos...)
	a.Recent = append([]string(nil), st.act.recent...)
	return a
}

// stepActivity is the activity of a run outside its agent (setup, verify, land): the phase and
// the step with its time so far.
func (r *Runner) stepActivity(phase, step string, started, stepStarted time.Time) *pipeline.Activity {
	return &pipeline.Activity{Phase: phase, Step: pipeline.ClipRunes(oneLineText(step), pipeline.MaxActivityName),
		ElapsedS: r.since(started), StepElapsedS: r.since(stepStarted)}
}

func (r *Runner) since(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	if d := int64(r.Clock.Now().Sub(t) / time.Second); d > 0 {
		return d
	}
	return 0
}
