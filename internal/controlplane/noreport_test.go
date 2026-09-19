package controlplane

// A heartbeat whose loaded_models is null carries no engine report: the
// worker is alive, and it says nothing about what it serves. These tests hold
// what that must and must not do.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// wireHeartbeat posts a heartbeat body exactly as given — the difference
// between null and [] only exists on the wire.
func wireHeartbeat(t *testing.T, ts *httptest.Server, body string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/v1/nodes/heartbeat", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+drainAdminKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat %s: %d", body, resp.StatusCode)
	}
}

func rowsOf(t *testing.T, srv *Server, node string) map[string]store.Placement {
	t.Helper()
	rows, err := srv.store.Placements().GetByNode(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.Placement{}
	for _, r := range rows {
		out[r.ModelID] = r
	}
	return out
}

// Report-less heartbeats leave the node's rows exactly as they are — the
// leader's draining mark included, so the router keeps skipping the placement
// — while the node's liveness still advances. An empty list still means
// "nothing is loaded" and clears the rows.
func TestNullHeartbeatKeepsRowsAndMarks(t *testing.T) {
	srv, ts, workers := drainFixture(t)
	ctx := context.Background()
	wireHeartbeat(t, ts, `{"id":"w1","loaded_models":["m","other"]}`)
	if err := srv.store.Placements().SetStatus(ctx, "w1", "m", store.PlacementDraining); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.Nodes().Heartbeat(ctx, "w1", time.Now().Add(-40*time.Second), ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		wireHeartbeat(t, ts, `{"id":"w1","loaded_models":null,"boot_id":""}`)
	}
	rows := rowsOf(t, srv, "w1")
	if rows["m"].Status != store.PlacementDraining || rows["other"].Status != "ready" || len(rows) != 2 {
		t.Fatalf("after three report-less heartbeats: %+v, want m draining and other ready", rows)
	}
	if n, _ := srv.store.Nodes().Get(ctx, "w1"); time.Since(n.LastHeartbeat) > 10*time.Second {
		t.Errorf("a report-less heartbeat is still a heartbeat: last_heartbeat is %s old", time.Since(n.LastHeartbeat))
	}
	// A real report after them does not resurrect the placement either.
	wireHeartbeat(t, ts, `{"id":"w1","loaded_models":["m","other"]}`)
	before1, before2 := workers["w1"].chats.Load(), workers["w2"].chats.Load()
	for i := 0; i < 10; i++ {
		if resp, out := chat(t, ts, drainChatBody, ""); resp.StatusCode != http.StatusOK {
			t.Fatalf("chat %d: %d %s", i, resp.StatusCode, out)
		}
	}
	if workers["w1"].chats.Load() != before1 || workers["w2"].chats.Load() != before2+10 {
		t.Errorf("the draining placement got requests: w1 +%d, w2 +%d; want 0 and 10",
			workers["w1"].chats.Load()-before1, workers["w2"].chats.Load()-before2)
	}
	// A key that is absent is the same "no report".
	wireHeartbeat(t, ts, `{"id":"w1"}`)
	if rows := rowsOf(t, srv, "w1"); len(rows) != 2 {
		t.Fatalf("a heartbeat without the key must change no row: %+v", rows)
	}
	// [] is a report: nothing is loaded.
	wireHeartbeat(t, ts, `{"id":"w1","loaded_models":[]}`)
	if rows := rowsOf(t, srv, "w1"); len(rows) != 0 {
		t.Fatalf("an empty list clears the rows, as before: %+v", rows)
	}
}

// The same for a released, cold row: neither the mark nor the cold flag is
// touched by a heartbeat that reports nothing.
func TestNullHeartbeatKeepsAReleasedColdRow(t *testing.T) {
	srv, ts, _ := drainFixture(t)
	ctx := context.Background()
	wireHeartbeat(t, ts, `{"id":"w1","loaded_models":["m"],"resident_models":[]}`)
	if err := srv.store.Placements().SetStatus(ctx, "w1", "m", store.PlacementReleased); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		wireHeartbeat(t, ts, `{"id":"w1","loaded_models":null,"resident_models":null}`)
	}
	if row := rowsOf(t, srv, "w1")["m"]; row.Status != store.PlacementReleased || !row.Cold {
		t.Fatalf("after three report-less heartbeats: %+v, want released and cold", row)
	}
	wireHeartbeat(t, ts, `{"id":"w1","loaded_models":["m"],"resident_models":[]}`)
	if row := rowsOf(t, srv, "w1")["m"]; row.Status != store.PlacementReleased || !row.Cold {
		t.Fatalf("the next real report: %+v, want released and cold", row)
	}
}

// A worker whose engine never answers again does not keep routable rows for
// ever: past the bound the one live rule takes the NODE out of rotation — its
// rows and their marks stay as they are — the 503 says why, `node ls` shows
// engine-silent, and the first real report brings it back.
func TestSilentEngineTakesTheWorkerOutOfRotationUntilItReports(t *testing.T) {
	srv, ts, workers := drainFixture(t)
	ctx := context.Background()
	maxAge := srv.heartbeatMaxAge()

	wireHeartbeat(t, ts, `{"id":"w1","loaded_models":null}`)
	n, _ := srv.store.Nodes().Get(ctx, "w1")
	if n.EngineSilentSince.IsZero() || !n.TakesNewWork(maxAge, time.Now()) {
		t.Fatalf("one slow tick is recorded and changes nothing: since %v, takes new work %v", n.EngineSilentSince, n.TakesNewWork(maxAge, time.Now()))
	}
	first := n.EngineSilentSince
	wireHeartbeat(t, ts, `{"id":"w1","loaded_models":null}`)
	if n, _ = srv.store.Nodes().Get(ctx, "w1"); !n.EngineSilentSince.Equal(first) {
		t.Fatalf("the stamp is the FIRST report-less heartbeat, not the last: %v then %v", first, n.EngineSilentSince)
	}

	// The engine has now been silent past the bound (the row says since when).
	n.EngineSilentSince = time.Now().Add(-maxAge - 10*time.Second)
	if err := srv.store.Nodes().Upsert(ctx, *n); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		wireHeartbeat(t, ts, `{"id":"w1","loaded_models":null}`) // still heartbeating, still silent
	}
	n, _ = srv.store.Nodes().Get(ctx, "w1")
	if n.TakesNewWork(maxAge, time.Now()) || n.LiveState(maxAge, time.Now()) != store.NodeStateEngineSilent ||
		!strings.Contains(n.WhyNoNewWork(maxAge, time.Now()), "engine has not answered") {
		t.Fatalf("past the bound: takes new work %v, state %q, why %q", n.TakesNewWork(maxAge, time.Now()),
			n.LiveState(maxAge, time.Now()), n.WhyNoNewWork(maxAge, time.Now()))
	}
	if row := rowsOf(t, srv, "w1")["m"]; row.Status != "ready" {
		t.Fatalf("the rows are not rewritten — the node is what is out of rotation: %+v", row)
	}
	before1, before2 := workers["w1"].chats.Load(), workers["w2"].chats.Load()
	for i := 0; i < 10; i++ {
		if resp, out := chat(t, ts, drainChatBody, ""); resp.StatusCode != http.StatusOK {
			t.Fatalf("chat %d: %d %s", i, resp.StatusCode, out)
		}
	}
	if workers["w1"].chats.Load() != before1 || workers["w2"].chats.Load() != before2+10 {
		t.Errorf("a silent-engine worker got requests: w1 +%d, w2 +%d; want 0 and 10",
			workers["w1"].chats.Load()-before1, workers["w2"].chats.Load()-before2)
	}

	// With the other worker drained, the 503 names the cause.
	if code, out := adminPost(t, ts, "/admin/v1/nodes/w2/drain"); code != http.StatusOK {
		t.Fatalf("drain: %d %s", code, out)
	}
	resp, out := chat(t, ts, drainChatBody, "")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" || !strings.Contains(out, "engine stopped answering") {
		t.Fatalf("nobody can serve: %d %s", resp.StatusCode, out)
	}

	// The engine answers again: the first real report puts the node back.
	wireHeartbeat(t, ts, `{"id":"w1","loaded_models":["m"]}`)
	if n, _ = srv.store.Nodes().Get(ctx, "w1"); !n.EngineSilentSince.IsZero() || !n.TakesNewWork(maxAge, time.Now()) {
		t.Fatalf("after a real report: since %v, takes new work %v", n.EngineSilentSince, n.TakesNewWork(maxAge, time.Now()))
	}
	if resp, out := chat(t, ts, drainChatBody, ""); resp.StatusCode != http.StatusOK || workers["w1"].chats.Load() != before1+1 {
		t.Fatalf("the worker serves again: %d %s (w1 +%d)", resp.StatusCode, out, workers["w1"].chats.Load()-before1)
	}
	journal := map[string]int{}
	evs, _ := srv.store.EventLog().After(ctx, 0, 1000)
	for _, e := range evs {
		if e.Subject == "w1" {
			journal[e.Type]++
		}
	}
	if journal["node.engine_silent"] != 1 || journal["node.engine_reporting"] != 1 {
		t.Errorf("the silence and the return are each journalled once, not per tick: %v", journal)
	}
}
