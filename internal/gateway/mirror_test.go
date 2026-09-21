package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// A gateway must keep serving when the leader is unreachable — that is the
// point of splitting the front from the brain — but it must not ADOPT a worker
// it learned from a list it can no longer refresh. That worker may have been
// drained, moved or destroyed since, and a door that adds one sends requests
// somewhere the brain has stopped accounting for.
func TestAStaleRegistryKeepsWorkersAndAddsNone(t *testing.T) {
	ctx := context.Background()
	var body atomic.Value
	var fail atomic.Bool
	body.Store(`[{"ID":"n_a","Hostname":"a","Address":"10.0.0.1:8081","state":"ready","heartbeat_age_seconds":1}]`)
	var shards atomic.Value
	shards.Store(`[]`)
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "leader down", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Path-aware, because the mirror reads two surfaces: the registry and
		// the gangs. A handler that answered the registry to every path let
		// the node list decode as a shard list — every field zero but the id —
		// and the door invented gang parts nobody had.
		if r.URL.Path == "/admin/v1/shards" {
			fmt.Fprint(w, shards.Load().(string))
			return
		}
		fmt.Fprint(w, body.Load().(string))
	}))
	defer leader.Close()

	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := NewMirror(leader.URL, "tok", st)
	clock := time.Now()
	m.now = func() time.Time { return clock }

	// First sync: a door with no list must be allowed to populate. "Stale" is
	// about a list that went cold, not about not having one yet.
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	nodes, _ := st.Nodes().List(ctx)
	if len(nodes) != 1 || nodes[0].ID != "n_a" {
		t.Fatalf("the first sync populates: %+v", nodes)
	}
	if fresh, _, _ := m.Fresh(); !fresh {
		t.Error("a list just fetched is fresh")
	}

	// The leader now reports a second worker, and the door is still fresh:
	// adopt it.
	body.Store(`[{"ID":"n_a","Hostname":"a","Address":"10.0.0.1:8081","state":"ready","heartbeat_age_seconds":1},
	             {"ID":"n_b","Hostname":"b","Address":"10.0.0.2:8081","state":"ready","heartbeat_age_seconds":1}]`)
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if nodes, _ = st.Nodes().List(ctx); len(nodes) != 2 {
		t.Fatalf("a fresh door adopts a new worker: %+v", nodes)
	}

	// The leader goes away. The list goes stale.
	fail.Store(true)
	clock = clock.Add(RegistryTTL + time.Second)
	if err := m.Sync(ctx); err == nil {
		t.Error("a failed sync must report the failure")
	}
	if fresh, age, lastErr := m.Fresh(); fresh || age < RegistryTTL || lastErr == "" {
		t.Errorf("the door must know its list is stale and why: fresh=%v age=%v err=%q", fresh, age, lastErr)
	}
	// …and it still has both workers: serving beats refusing.
	if nodes, _ = st.Nodes().List(ctx); len(nodes) != 2 {
		t.Errorf("a stale door keeps the workers it knows: %+v", nodes)
	}

	// The leader answers again but the door is stale at the moment of the read
	// (its last success is older than the TTL), and the answer names a THIRD
	// worker. It must not be adopted on this pass.
	fail.Store(false)
	body.Store(`[{"ID":"n_a","Hostname":"a","Address":"10.0.0.1:8081","state":"ready","heartbeat_age_seconds":1},
	             {"ID":"n_b","Hostname":"b","Address":"10.0.0.2:8081","state":"ready","heartbeat_age_seconds":1},
	             {"ID":"n_c","Hostname":"c","Address":"10.0.0.3:8081","state":"ready","heartbeat_age_seconds":1}]`)
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	nodes, _ = st.Nodes().List(ctx)
	for _, n := range nodes {
		if n.ID == "n_c" {
			t.Error("a worker learned from a stale list must not be adopted on the pass that was stale")
		}
	}
	// The pass succeeded, so the door is fresh again and the NEXT pass adopts it.
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if nodes, _ = st.Nodes().List(ctx); len(nodes) != 3 {
		t.Errorf("once fresh again, the third worker is adopted: %+v", nodes)
	}
}

// The leader's DERIVED state is what a door must obey: a node the brain calls
// lost or engine-silent is out of rotation, whatever its stored row says. Both
// keys are on the wire and only one can be decoded, so this pins which.
func TestTheMirrorTakesTheLeadersDerivedState(t *testing.T) {
	ctx := context.Background()
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/v1/shards" {
			fmt.Fprint(w, `[]`)
			return
		}
		// Stored "ready", derived "engine-silent": the brain has taken it out.
		fmt.Fprint(w, `[{"ID":"n_a","Hostname":"a","Address":"10.0.0.1:8081","State":"ready","state":"engine-silent","heartbeat_age_seconds":2}]`)
	}))
	defer leader.Close()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := NewMirror(leader.URL, "tok", st)
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	nodes, _ := st.Nodes().List(ctx)
	if len(nodes) != 1 || nodes[0].State != "engine-silent" {
		t.Fatalf("the derived state is what routing obeys: %+v", nodes)
	}
}

// A door routes BY placement and, for a sharded endpoint, by shard row: the
// machines alone are a list it cannot send one request to. Both follow the
// leader exactly — what it stops listing leaves the door's view, because a
// door still holding a placement the worker dropped fails every request it
// sends there, and a coordinator address that moved is worse than none.
func TestTheMirrorCarriesPlacementsAndGangs(t *testing.T) {
	ctx := context.Background()
	var nodes, shards atomic.Value
	nodes.Store(`[{"ID":"n_a","Hostname":"a","Address":"10.0.0.1:8081","state":"ready","heartbeat_age_seconds":1,
	               "WorkerToken":"sk-orc-worker-a",
	               "placements":[{"NodeID":"n_a","ModelID":"qwen","Status":"ready"},{"NodeID":"n_a","ModelID":"llama","Status":"ready"}]}]`)
	shards.Store(`[{"ID":"sh_1","ModelID":"big","GangID":"g1","Role":"coordinator","NodeID":"n_a","Address":"10.0.0.1:9000","Status":"ready"},
	               {"ID":"sh_2","ModelID":"big","GangID":"g1","Role":"rpc","NodeID":"n_a","Address":"10.0.0.1:9001","Status":"ready"}]`)
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/v1/shards" {
			fmt.Fprint(w, shards.Load().(string))
			return
		}
		fmt.Fprint(w, nodes.Load().(string))
	}))
	defer leader.Close()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := NewMirror(leader.URL, "tok", st)
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	ps, _ := st.Placements().GetByNode(ctx, "n_a")
	if len(ps) != 2 {
		t.Fatalf("both resident models mirrored: %+v", ps)
	}
	// The credential the worker minted at join: the leader keeps it on the row
	// so it can call that worker back, and a door dispatches to the same
	// workers. Without it the worker answered 401 and the door turned every
	// dispatched request into a 502 — the first request a door ever dispatched,
	// on the cell.
	if n, _ := st.Nodes().Get(ctx, "n_a"); n == nil || n.WorkerToken != "sk-orc-worker-a" {
		t.Fatalf("the worker's own token is mirrored, or a door cannot dial it: %+v", n)
	}
	if got, _ := st.Placements().GetByModel(ctx, "qwen"); len(got) != 1 {
		t.Fatalf("a door must be able to find a holder by model: %+v", got)
	}
	shs, _ := st.Shards().List(ctx)
	if len(shs) != 2 {
		t.Fatalf("a gang's parts mirrored: %+v", shs)
	}
	coord, err := st.Shards().Get(ctx, "sh_1")
	if err != nil || coord == nil || coord.Address != "10.0.0.1:9000" || coord.Gang() != "g1" {
		t.Fatalf("the coordinator's address and gang are what the door streams to: %+v (%v)", coord, err)
	}

	// The worker unloads one model and the gang loses a part. Both must leave.
	nodes.Store(`[{"ID":"n_a","Hostname":"a","Address":"10.0.0.1:8081","state":"ready","heartbeat_age_seconds":1,
	               "placements":[{"NodeID":"n_a","ModelID":"qwen","Status":"ready"}]}]`)
	shards.Store(`[{"ID":"sh_1","ModelID":"big","GangID":"g1","Role":"coordinator","NodeID":"n_a","Address":"10.0.0.1:9100","Status":"ready"}]`)
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Placements().GetByModel(ctx, "llama"); len(got) != 0 {
		t.Errorf("a model the leader no longer lists on the worker must leave the door's view: %+v", got)
	}
	if shs, _ = st.Shards().List(ctx); len(shs) != 1 {
		t.Errorf("a part the leader dropped must leave: %+v", shs)
	}
	coord, _ = st.Shards().Get(ctx, "sh_1")
	if coord == nil || coord.Address != "10.0.0.1:9100" {
		t.Errorf("a coordinator that moved is followed, not kept: %+v", coord)
	}
}
