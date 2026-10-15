package leader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// engineBeat is a heartbeat from a worker whose engine is in state and
// answers nothing: it reports nothing loaded, as a real worker does when
// nothing listens where its engine would.
func engineBeat(t *testing.T, srv *Server, id, state string) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.heartbeatNode(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/heartbeat",
		strings.NewReader(`{"id":"`+id+`","loaded_models":[],"engine":{"state":"`+state+`"}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
	}
}

// Both workers' engines only restarting (design-partner cell, PLAN T8.19):
// their placements go with the empty reports, and the answer read "no
// workers are awake for this model — waking (scale-up in progress or floor is
// 0)" while the floor was 2 and nothing scaled. It says the engines restart.
func TestBothEnginesRestartingSayRestartingNotWaking(t *testing.T) {
	srv := slottedLeader(t, 4, PolicyAdmission{})
	ctx := context.Background()
	if err := srv.store.Nodes().Upsert(ctx, store.Node{ID: "w2", Hostname: "w2", State: "ready", Address: "127.0.0.1:2", WorkerToken: "wtok2", LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	beat(t, srv, "w2", 4)
	engineBeat(t, srv, "w1", "starting")
	engineBeat(t, srv, "w2", "crash-looping")
	why := srv.unavailable(ctx, "m")
	if why == "" {
		t.Fatal("no engine serves: the model is unavailable")
	}
	if strings.Contains(why, "awake") || strings.Contains(why, "scale-up") || !strings.Contains(why, "2 worker(s) whose engine is restarting") {
		t.Fatalf("want the engines restarting, got %q", why)
	}
	// One engine reported stopped (also what a worker says during its
	// restart back-off) reads so, without claiming it will not come back.
	engineBeat(t, srv, "w2", "stopped")
	if why := srv.unavailable(ctx, "m"); !strings.Contains(why, "1 worker(s) whose engine is restarting") || !strings.Contains(why, "1 worker(s) whose engine has stopped") {
		t.Fatalf("want one restarting and one stopped, got %q", why)
	}
}

// A worker that unloaded the model with its engine serving no longer holds
// it: nothing restarts, and the answer is not about an engine.
func TestAnUnloadWithTheEngineServingIsNotARestart(t *testing.T) {
	srv := slottedLeader(t, 4, PolicyAdmission{})
	engineBeat(t, srv, "w1", "serving")
	if why := srv.unavailable(context.Background(), "m"); strings.Contains(why, "engine") {
		t.Fatalf("an unload is not an engine restart, got %q", why)
	}
}
