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
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "leader down", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
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
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
