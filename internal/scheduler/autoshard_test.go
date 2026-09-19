package scheduler

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/lifecycle"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

const gib = int64(1) << 30

func free(id string, gbFree int64) WorkerMemory {
	return WorkerMemory{NodeID: id, CapacityBytes: gbFree * gib, FreeBytes: gbFree * gib}
}

// The picker's rules, one row each.
func TestPickShards(t *testing.T) {
	cases := []struct {
		name    string
		need    int64
		layers  int
		workers []WorkerMemory
		shards  int
		nodes   []string
		wantErr []string // every fragment must appear in the refusal
	}{
		{name: "fits one worker: 1 part, on the freest",
			need: 20 * gib, workers: []WorkerMemory{free("a", 16), free("b", 24), free("c", 24)},
			shards: 1, nodes: []string{"b"}},
		{name: "too large for one worker: the smallest N that fits",
			need: 40 * gib, workers: []WorkerMemory{free("a", 24), free("b", 24), free("c", 24)},
			shards: 2, nodes: []string{"a", "b"}},
		{name: "a small worker pushes N up, not the pick onto it",
			need: 60 * gib, workers: []WorkerMemory{free("a", 24), free("b", 24), free("c", 8), free("d", 24)},
			shards: 3, nodes: []string{"a", "b", "d"}},
		{name: "uneven workers: N is the first count whose Nth-freest holds an equal part",
			need: 48 * gib, workers: []WorkerMemory{free("a", 40), free("b", 10), free("c", 16), free("d", 16)},
			shards: 3, nodes: []string{"a", "c", "d"}},
		{name: "never more parts than workers",
			need: 100 * gib, workers: []WorkerMemory{free("a", 24), free("b", 24)},
			wantErr: []string{"100.0 GB", "2 live worker(s)", "48.0 GB free in total", "a 24.0 GB", "b 24.0 GB", "even 2 part(s) need 50.0 GB each"}},
		{name: "never more parts than the model's layers",
			need: 30 * gib, layers: 2, workers: []WorkerMemory{free("a", 12), free("b", 12), free("c", 12)},
			wantErr: []string{"30.0 GB", "even 2 part(s) (capped at the model's 2 layers) need 15.0 GB each", "b has 12.0 GB"}},
		{name: "layers unknown: the worker count is the only cap",
			need: 30 * gib, workers: []WorkerMemory{free("a", 12), free("b", 12), free("c", 12)},
			shards: 3, nodes: []string{"a", "b", "c"}},
		{name: "layers above the worker count change nothing",
			need: 30 * gib, layers: 80, workers: []WorkerMemory{free("a", 12), free("b", 12), free("c", 12)},
			shards: 3, nodes: []string{"a", "b", "c"}},
		{name: "a part is rounded up, never down",
			need: 3*gib + 1, workers: []WorkerMemory{free("a", 1), free("b", 1), free("c", 1)},
			wantErr: []string{"no split fits"}},
		{name: "no live workers",
			need: gib, wantErr: []string{"no live workers"}},
		{name: "unknown size",
			need: 0, workers: []WorkerMemory{free("a", 24)}, wantErr: []string{"size is unknown"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := PickShards(c.need, c.layers, c.workers)
			if len(c.wantErr) > 0 {
				if err == nil {
					t.Fatalf("picked %+v, want a refusal", got)
				}
				for _, frag := range c.wantErr {
					if !strings.Contains(err.Error(), frag) {
						t.Errorf("refusal %q does not name %q", err, frag)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Shards != c.shards || !reflect.DeepEqual(got.Nodes, c.nodes) {
				t.Fatalf("picked %d on %v, want %d on %v", got.Shards, got.Nodes, c.shards, c.nodes)
			}
			if got.Why == "" || !strings.Contains(got.Why, gb(c.need)) {
				t.Errorf("the reason must name what the model needs: %q", got.Why)
			}
			if got.PartBytes*int64(got.Shards) < c.need {
				t.Errorf("%d parts of %d bytes do not hold %d", got.Shards, got.PartBytes, c.need)
			}
		})
	}
}

// The order the workers arrive in never changes the pick.
func TestPickShardsIsOrderIndependent(t *testing.T) {
	a := []WorkerMemory{free("a", 24), free("b", 8), free("c", 24)}
	b := []WorkerMemory{free("c", 24), free("a", 24), free("b", 8)}
	pa, _ := PickShards(40*gib, 0, a)
	pb, _ := PickShards(40*gib, 0, b)
	if !reflect.DeepEqual(pa, pb) {
		t.Fatalf("%+v vs %+v", pa, pb)
	}
	if a[1].NodeID != "b" {
		t.Fatal("the caller's slice was reordered")
	}
}

// The leader's side of the rule: a create request's count is the caller's, or
// the catalog's default — never derived from the workers that happen to be up.
func TestShardCountCallerWins(t *testing.T) {
	entry := models.Entry{ID: "m", SizeBytes: 400 * gib, Sharding: models.ShardingSpec{Required: true, DefaultShards: 2}}
	for _, c := range []struct {
		n     int
		nodes []string
		want  int
	}{
		{n: 1, want: 1}, // would never fit one worker: still the caller's call
		{n: 7, want: 7},
		{n: 3, nodes: []string{"a", "b"}, want: 2}, // named nodes are the shape
		{n: 0, want: 2}, // no count = the catalog default, not a pick
	} {
		got, err := shardCountFor(entry, c.n, c.nodes)
		if err != nil || got != c.want {
			t.Errorf("shardCountFor(n=%d nodes=%v) = %d, %v; want %d", c.n, c.nodes, got, err, c.want)
		}
	}
	if _, err := shardCountFor(models.Entry{ID: "m"}, 0, nil); err == nil {
		t.Error("no count and no catalog default must be refused, not picked")
	}
}

func TestShardNeedBytes(t *testing.T) {
	if got := ShardNeedBytes(models.Entry{SizeBytes: 10 * gib}); got != lifecycle.Footprint(10*gib) || got != 12*gib {
		t.Errorf("sized entry: %d", got)
	}
	if got := ShardNeedBytes(models.Entry{Hardware: models.HardwareSpec{MinRAMGB: 8, MinVRAMGB: 16}}); got != 16*gib {
		t.Errorf("hardware floor: %d", got)
	}
	if got := ShardNeedBytes(models.Entry{}); got != 0 {
		t.Errorf("unknown: %d", got)
	}
}

// Free memory comes from rows the leader already has: what a worker
// registered, under the lifecycle's reserve, less what is placed on it.
func TestWorkerMemoryFacts(t *testing.T) {
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, now := context.Background(), time.Now()
	for _, n := range []store.Node{
		{ID: "local", RAMGB: 512, Address: "127.0.0.1:8080", State: "ready", LastHeartbeat: now},
		{ID: "gpu", RAMGB: 256, Address: "192.0.2.1:8081", State: "ready", LastHeartbeat: now,
			HardwareJSON: `{"RAMGB":256,"GPUs":[{"Name":"x","VRAMGB":50},{"Name":"x","VRAMGB":50}]}`},
		{ID: "cpu", RAMGB: 100, Address: "192.0.2.2:8081", State: "ready", LastHeartbeat: now},
		{ID: "busy", RAMGB: 100, Address: "192.0.2.3:8081", State: "ready", LastHeartbeat: now},
		{ID: "stale", RAMGB: 100, Address: "192.0.2.4:8081", State: "ready", LastHeartbeat: now.Add(-5 * time.Minute)},
		{ID: "draining", RAMGB: 100, Address: "192.0.2.5:8081", State: "draining", LastHeartbeat: now},
	} {
		if err := st.Nodes().Upsert(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	cat := []models.Entry{{ID: "gang", SizeBytes: 50 * gib}, {ID: "old", SizeBytes: 500 * gib}}
	// "busy" holds an installed model (placed under its engine name) and half
	// of a two-part gang; "cpu" holds the other half, and the gang's own
	// placement row on its coordinator must not be counted again.
	if err := st.Models().Upsert(ctx, store.Model{ID: "small", CatalogID: "small", Source: "ollama:small:latest", SizeBytes: 10 * gib}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []store.Placement{
		{NodeID: "busy", ModelID: "small:latest", Status: "ready"},
		{NodeID: "cpu", ModelID: "gang", Status: "ready"},
		{NodeID: "cpu", ModelID: "never-heard-of", Status: "ready"},
	} {
		if err := st.Placements().Upsert(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []store.Shard{
		{ID: "g-c", ModelID: "gang", Role: "coordinator", NodeID: "cpu"},
		{ID: "g-0", ModelID: "gang", Role: "rpc", NodeID: "cpu"},
		{ID: "g-1", ModelID: "gang", Role: "rpc", NodeID: "busy"},
		{ID: "o-c", ModelID: "old", Role: "coordinator", NodeID: "gpu"}, // the gang being replaced: ignored
	} {
		if err := st.Shards().Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	got, err := WorkerMemoryFacts(ctx, st, cat, "old", 20, time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]WorkerMemory{}
	for _, w := range got {
		byID[w.NodeID] = w
	}
	if len(byID) != 3 {
		t.Fatalf("live workers = %v, want gpu, cpu, busy (never local, stale or draining)", got)
	}
	want := map[string]WorkerMemory{
		"gpu":  {NodeID: "gpu", CapacityBytes: 80 * gib, FreeBytes: 80 * gib},                           // 100 GB of cards, not 256 GB of RAM
		"cpu":  {NodeID: "cpu", CapacityBytes: 80 * gib, ResidentBytes: 30 * gib, FreeBytes: 50 * gib},  // half of 60
		"busy": {NodeID: "busy", CapacityBytes: 80 * gib, ResidentBytes: 42 * gib, FreeBytes: 38 * gib}, // half of 60 + 12
	}
	for id, w := range want {
		if byID[id] != w {
			t.Errorf("%s: got %+v, want %+v", id, byID[id], w)
		}
	}
}

// The same rule through the create path itself, with a fleet the picker would
// have sized differently: three live workers that could each hold the model
// whole. The leader still asks for exactly the caller's count, or the
// catalog's default when none was sent.
func TestCreateShardedNeverPicksACount(t *testing.T) {
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if err := st.Nodes().Upsert(ctx, store.Node{ID: id, RAMGB: 512, Address: "192.0.2.9:8081", State: "ready", LastHeartbeat: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	o := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
	entry := models.Entry{ID: "m", SizeBytes: gib, Source: models.SourceSpec{Type: "file", Path: "/nonexistent.gguf"},
		Sharding: models.ShardingSpec{Required: true, DefaultShards: 5}}
	for _, c := range []struct {
		n    int
		want string
	}{
		{n: 4, want: "need 4 ready workers, have 3"},
		{n: 0, want: "need 5 ready workers, have 3"},
	} {
		err := o.CreateSharded(ctx, entry, c.n, nil, Parallelism{})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("count %d: got %v, want %q", c.n, err, c.want)
		}
	}
}
