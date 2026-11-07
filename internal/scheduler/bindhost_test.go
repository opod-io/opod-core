package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/opod-io/opod-core/internal/agent"
	"github.com/opod-io/opod-core/internal/store"
)

// PLAN T10.22: behind NAT the advertised address is the leader's to dial and
// not one the worker owns, so a gang process bound to it fails. A worker that
// registered a BindHost (OPOD_BIND_ADDR) gets its processes bound there; every
// other worker binds the advertised host, as before.
func TestBindHostOf(t *testing.T) {
	for _, tc := range []struct {
		name, hw, want string
	}{
		{"no hardware doc", "", "203.0.113.7"},
		{"no field", `{"Hostname":"node-a"}`, "203.0.113.7"},
		{"stated", `{"BindHost":"10.0.0.5"}`, "10.0.0.5"},
		{"a doc that does not parse", `{`, "203.0.113.7"},
	} {
		if got := bindHostOf(store.Node{HardwareJSON: tc.hw}, "203.0.113.7"); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The coordinator on a worker that stated a BindHost is started on that host,
// and probed there — while the leader keeps the advertised address.
func TestAGangProcessBindsTheWorkersBindHost(t *testing.T) {
	var mu sync.Mutex
	var specs []agent.ProcessSpec
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/v1/process/file"):
			rw.Header().Set("X-File-Path", "/models/m.gguf")
			rw.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/v1/process/start"):
			var spec agent.ProcessSpec
			_ = json.NewDecoder(r.Body).Decode(&spec)
			mu.Lock()
			specs = append(specs, spec)
			mu.Unlock()
			_ = json.NewEncoder(rw).Encode(map[string]any{"id": spec.ID, "status": "starting"})
		case strings.HasSuffix(r.URL.Path, "/v1/process/get"):
			_ = json.NewEncoder(rw).Encode(map[string]any{"id": r.URL.Query().Get("id"), "status": "running"})
		default:
			_, _ = io.WriteString(rw, `[]`)
		}
	}))
	t.Cleanup(srv.Close)
	o, entry := partLostFixture(t, &scriptedWorker{srv: srv})
	n, _ := o.Store.Nodes().Get(context.Background(), "n1")
	n.HardwareJSON = `{"BindHost":"127.0.0.2"}`
	if err := o.Store.Nodes().Upsert(context.Background(), *n); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, createIn(o, entry)); err != nil {
		t.Fatalf("create: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(specs) == 0 {
		t.Fatal("no process was started on the worker")
	}
	for _, s := range specs {
		i := slices.Index(s.Args, "--host")
		if i < 0 {
			i = slices.Index(s.Args, "-H")
		}
		if i < 0 || i+1 >= len(s.Args) || s.Args[i+1] != "127.0.0.2" {
			t.Errorf("%s: started with %v, want it bound to the BindHost 127.0.0.2", s.Command, s.Args)
		}
		if s.HealthHost != "127.0.0.2" {
			t.Errorf("%s: probed at %q, want the host it binds", s.Command, s.HealthHost)
		}
	}
	shards, _ := o.Store.Shards().GetByGang(context.Background(), entry.ID, "g1")
	advertised := strings.TrimPrefix(srv.URL, "http://")
	host := advertised[:strings.LastIndex(advertised, ":")]
	for _, sh := range shards {
		if !strings.HasPrefix(sh.Address, host+":") {
			t.Errorf("shard %s is dialled at %q: the leader must keep the advertised host %s", sh.ID, sh.Address, host)
		}
	}
}
