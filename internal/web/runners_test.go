package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/store"
)

// A draining runner (loop-runner drain) is on the board until it claims again, after its restart.
func TestBoardShowsADrainingRunner(t *testing.T) {
	srv, st := boardSetup(t)
	ctx := context.Background()
	cookie := &http.Cookie{Name: "loop_board", Value: boardTok}
	if _, page := get(t, srv.URL+"/board", cookie); strings.Contains(page, `class="runners"`) {
		t.Fatalf("a runner notice with no runner draining")
	}
	if err := st.SetRunnerState(ctx, "devbox", store.RunnerDraining); err != nil {
		t.Fatal(err)
	}
	_, page := get(t, srv.URL+"/board", cookie)
	if !strings.Contains(page, `<p class="runners">Runner <strong>devbox</strong> is draining since`) {
		t.Fatalf("no draining notice:\n%s", page)
	}
	if err := st.SetRunnerState(ctx, "devbox", ""); err != nil {
		t.Fatal(err)
	}
	if _, page := get(t, srv.URL+"/board", cookie); strings.Contains(page, `class="runners"`) {
		t.Fatalf("the notice stayed after the runner said it works again")
	}
}
