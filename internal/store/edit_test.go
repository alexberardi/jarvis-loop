package store

import (
	"context"
	"slices"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

func TestEdit(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	built := toDone(t, s, runner)
	it := add(t, s, "typo tittle", "")
	title, ask := "typo title", "the fixed ask"
	got, err := s.Edit(ctx, user, it.ID, &title, &ask)
	if err != nil || got.Title != title || got.Ask != ask {
		t.Fatalf("%+v %v", got, err)
	}
	e := newest(t, s, it.ID)
	if e.Kind != pipeline.KindEdited || e.Actor != user || e.Data["title"].(map[string]any)["from"] != "typo tittle" {
		t.Fatalf("event %+v", e)
	}

	if _, err := s.Edit(ctx, user, built.ID, &title, nil); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("edit after build: %v", err)
	}
	if e := newest(t, s, built.ID); e.Kind != "refused" || e.Data["action"] != "edit" {
		t.Fatalf("the refusal is not recorded: %+v", e)
	}
}

func TestLinkUnlink(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	a, b, c := add(t, s, "a", ""), add(t, s, "b", ""), add(t, s, "c", "")
	got, err := s.Link(ctx, user, c.ID, []int64{a.ID, b.ID})
	if err != nil || !slices.Equal(got.DependsOn, []int64{a.ID, b.ID}) {
		t.Fatalf("%+v %v", got, err)
	}
	if e := newest(t, s, c.ID); e.Kind != KindLinked || e.Actor != user || len(e.Data["after"].([]any)) != 2 {
		t.Fatalf("event %+v", e)
	}
	// All or none: a cycle in the second refuses the first too.
	d := add(t, s, "d", "")
	if _, err := s.Link(ctx, user, a.ID, []int64{d.ID, c.ID}); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("cycle: %v", err)
	}
	if g, _ := s.GetItem(ctx, a.ID); len(g.Item.DependsOn) != 0 {
		t.Fatalf("a refused link was kept: %v", g.Item.DependsOn)
	}
	if _, err := s.Link(ctx, user, c.ID, []int64{a.ID}); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("already linked: %v", err)
	}
	if _, err := s.Link(ctx, user, c.ID, nil); code(err) != pipeline.CodeMissingField {
		t.Fatalf("nothing to link: %v", err)
	}

	got, err = s.Unlink(ctx, user, c.ID, a.ID)
	if err != nil || !slices.Equal(got.DependsOn, []int64{b.ID}) {
		t.Fatalf("%+v %v", got, err)
	}
	if e := newest(t, s, c.ID); e.Kind != pipeline.KindUnlinked || e.Data["after"] != float64(a.ID) {
		t.Fatalf("event %+v", e)
	}
	if _, err := s.Unlink(ctx, user, c.ID, a.ID); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("unlink twice: %v", err)
	}
	if bi := boardItem(t, s, c.ID); len(bi.Waits) != 1 {
		t.Fatalf("waits %v, want b only", bi.Waits)
	}
}

func TestSetResourcesClearsUnservable(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	it := add(t, s, "mac app", "P4")
	if _, err := s.SetResources(ctx, user, it.ID, "voice"); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("resources in triage: %v", err)
	}
	dev := []string{"emulator", "gpu", "voice"}
	triagedWith(t, s, it, "macos", dev...)
	if b := boardItem(t, s, it.ID); !slices.Equal(b.Unservable, []string{"macos"}) {
		t.Fatalf("board unservable %v", b.Unservable)
	}
	if _, err := s.SetResources(ctx, user, it.ID, "macos,pi"); code(err) != pipeline.CodeInvalidField {
		t.Fatalf("unknown resource: %v", err)
	}
	// Still none that a runner offers: flagged anew, for what is missing now.
	if _, err := s.SetResources(ctx, user, it.ID, "macos,voice"); err != nil {
		t.Fatal(err)
	}
	if b := boardItem(t, s, it.ID); !slices.Equal(b.Unservable, []string{"macos"}) {
		t.Fatalf("board unservable %v", b.Unservable)
	}
	got, err := s.SetResources(ctx, user, it.ID, "voice")
	if err != nil || !slices.Equal(got.Resources, []string{"voice"}) {
		t.Fatalf("%+v %v", got, err)
	}
	// The resources event says why the flag went; no servable (that says a runner came).
	if k := kindsOf(t, s, it.ID, pipeline.KindResources, KindUnservable, KindServable); !slices.Equal(k, []string{KindUnservable, pipeline.KindResources, KindUnservable, pipeline.KindResources}) {
		t.Fatalf("events %v", k)
	}
	if b := boardItem(t, s, it.ID); b.Unservable != nil {
		t.Fatalf("still flagged: %v", b.Unservable)
	}
	if c := claim(t, s, "agent-1", dev...); c == nil || c.Item.ID != it.ID {
		t.Fatalf("devbox cannot claim it now: %+v", c)
	}
}
