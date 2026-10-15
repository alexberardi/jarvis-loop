package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

func TestSubscribersSeeCommittedEventsInOrder(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	var mu sync.Mutex
	var got []Event
	s.Subscribe(func(e Event) {
		// After commit: the item is already readable from the reader pool.
		if e.ItemID != 0 {
			if _, err := s.GetItem(ctx, e.ItemID); err != nil {
				t.Errorf("event %s for item %d not readable yet: %v", e.Kind, e.ItemID, err)
			}
		}
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
	})
	it := add(t, s, "one", "P2")
	clk.Advance(time.Second)
	// A refused action rolls back its transaction: only the refused event is emitted.
	if _, err := s.Answer(ctx, user, it.ID, "not asked"); code(err) != pipeline.CodeWrongStage {
		t.Fatalf("answer: %v", err)
	}
	if _, err := s.SetControl(ctx, user, ControlPatch{ActiveAgents: ptr(2)}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	kinds := []string{}
	for _, e := range got {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"created", "refused", "control"}
	if len(kinds) != len(want) {
		t.Fatalf("events %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("events %v, want %v", kinds, want)
		}
	}
	if got[0].ID == 0 || got[0].ItemID != it.ID || !got[0].At.Equal(t0) || got[0].Data["title"] != "one" {
		t.Fatalf("created event %+v", got[0])
	}
	if got[1].ID <= got[0].ID || got[2].ItemID != 0 {
		t.Fatalf("ids/items: %+v", got)
	}
}

func ptr[T any](v T) *T { return &v }

func TestSlackThreadAndState(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	it := add(t, s, "threaded", "P3")
	if ch, ts, err := s.SlackThread(ctx, it.ID); err != nil || ch != "" || ts != "" {
		t.Fatalf("fresh thread %q %q %v", ch, ts, err)
	}
	if err := s.SetSlackThread(ctx, it.ID, "C1", "111.222"); err != nil {
		t.Fatal(err)
	}
	if ch, ts, _ := s.SlackThread(ctx, it.ID); ch != "C1" || ts != "111.222" {
		t.Fatalf("thread %q %q", ch, ts)
	}
	id, err := s.ItemByThread(ctx, "C1", "111.222")
	if err != nil || id != it.ID {
		t.Fatalf("by thread %d %v", id, err)
	}
	if id, err := s.ItemByThread(ctx, "C1", "999.000"); err != nil || id != 0 {
		t.Fatalf("unknown thread %d %v", id, err)
	}
	// Setting the thread is not a pipeline change: it writes no event, and saving the item
	// through a transition keeps it.
	if _, err := s.PatchItem(ctx, user, it.ID, ptr("P1"), nil); err != nil {
		t.Fatal(err)
	}
	if ch, ts, _ := s.SlackThread(ctx, it.ID); ch != "C1" || ts != "111.222" {
		t.Fatalf("thread lost on save: %q %q", ch, ts)
	}

	st, err := s.SlackState(ctx)
	if err != nil || st.ControlTS != "" || !st.LastDigest.IsZero() {
		t.Fatalf("fresh state %+v %v", st, err)
	}
	if err := s.SetSlackControlTS(ctx, "333.444"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLastDigest(ctx, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A control change keeps the Slack columns.
	if _, err := s.SetControl(ctx, user, ControlPatch{ActiveAgents: ptr(3)}); err != nil {
		t.Fatal(err)
	}
	st, _ = s.SlackState(ctx)
	if st.ControlTS != "333.444" || !st.LastDigest.Equal(t0.Add(time.Hour)) {
		t.Fatalf("state %+v", st)
	}
	_ = clk
}

func TestRecordAndEventQueries(t *testing.T) {
	s, clk := open(t)
	ctx := context.Background()
	it := add(t, s, "recorded", "P3")
	clk.Advance(time.Minute)
	if err := s.Record(ctx, Event{ItemID: it.ID, Actor: "slack:U1", Kind: "refused", Data: map[string]any{"action": "answer"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, Event{ItemID: 999, Actor: "slack:U1", Kind: "refused"}); code(err) != CodeNotFound {
		t.Fatalf("record on a missing item: %v", err)
	}
	if err := s.Record(ctx, Event{Actor: "slack:U1"}); err == nil {
		t.Fatal("record without a kind should fail")
	}
	clk.Advance(time.Minute)
	if err := s.Record(ctx, Event{Actor: "slack:U1", Kind: "usage", Data: map[string]any{"percent": 42}}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	if err := s.Record(ctx, Event{Actor: "slack:U1", Kind: "usage", Data: map[string]any{"percent": 50}}); err != nil {
		t.Fatal(err)
	}

	evs, err := s.EventsBetween(ctx, t0, t0.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// (t0, t0+2m]: the refused event at +1m and the first usage at +2m; created at t0 is excluded.
	if len(evs) != 2 || evs[0].Kind != "refused" || evs[1].Kind != "usage" {
		t.Fatalf("between: %+v", evs)
	}
	last, ok, err := s.LastEvent(ctx, "usage")
	if err != nil || !ok || last.Data["percent"] != float64(50) {
		t.Fatalf("last usage %+v %v %v", last, ok, err)
	}
	if _, ok, err := s.LastEvent(ctx, "nothing"); err != nil || ok {
		t.Fatalf("last nothing %v %v", ok, err)
	}
}
