package runner

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/fakeclaude"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// #96: opencode dying before its first request says why: its stderr is in the failure.
func TestQwenFailureCarriesStderr(t *testing.T) {
	g, up := qwenRig(t, fakeclaude.Good)
	bin := filepath.Join(g.scripts, "opencode-broken")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'Error: Cannot find module @ai-sdk/openai-compatible' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	g.r.Cfg.Qwen.OpenCode = bin
	id := g.toBuild("Add a feature")
	g.setBuilder(id, pipeline.BuilderQwen)
	g.runOnce()
	d := g.item(id)
	if d.Item.Stage != pipeline.NeedsYou || up.ok+up.wrong != 0 {
		t.Fatalf("stage %s, %d requests", d.Item.Stage, up.ok+up.wrong)
	}
	if !strings.Contains(d.Item.Question, "stderr: Error: Cannot find module @ai-sdk/openai-compatible") {
		t.Fatalf("the failure does not carry opencode's stderr: %q", d.Item.Question)
	}
}

// A qwen item builds with claude this run when the runner's qwen config cannot work: the opencode
// binary is missing or a mise shim, or jarvisd is not reachable.
func TestQwenFallsBackWhenQwenCannotRun(t *testing.T) {
	closed := func(t *testing.T) string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		l.Close()
		return "http://" + addr + "/v1"
	}
	for _, tc := range []struct {
		name, reason string
		set          func(t *testing.T, q *QwenConfig)
	}{
		{"missing opencode", "opencode not found", func(t *testing.T, q *QwenConfig) {
			q.OpenCode = filepath.Join(t.TempDir(), "opencode")
		}},
		{"mise shim", "mise which opencode", func(t *testing.T, q *QwenConfig) {
			dir := t.TempDir()
			mise := filepath.Join(dir, "mise")
			if err := os.WriteFile(mise, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			q.OpenCode = filepath.Join(dir, "opencode")
			if err := os.Symlink(mise, q.OpenCode); err != nil {
				t.Fatal(err)
			}
		}},
		{"endpoint unreachable", "qwen endpoint unreachable", func(t *testing.T, q *QwenConfig) {
			q.BaseURL = closed(t)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, up := qwenRig(t, fakeclaude.Good)
			tc.set(t, g.r.Cfg.Qwen)
			id := g.toBuild("Add a feature")
			g.setBuilder(id, pipeline.BuilderQwen)
			g.runOnce()
			d := g.item(id)
			if d.Item.Stage != pipeline.Review || d.Item.Builder != pipeline.BuilderQwen || up.ok+up.wrong != 0 {
				t.Fatalf("item %+v; log:\n%s", d.Item, g.logs.String())
			}
			if ev := g.advanced(id); ev.Data["builder"] != "claude" {
				t.Fatalf("data %v", ev.Data)
			}
			fb := g.events(id, "builder_fallback")
			if len(fb) != 1 || fb[0].Data["this_run_only"] != true || !strings.Contains(fmt.Sprint(fb[0].Data["reason"]), tc.reason) {
				t.Fatalf("builder_fallback %+v", fb)
			}
		})
	}
}
