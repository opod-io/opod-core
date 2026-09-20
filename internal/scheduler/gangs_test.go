package scheduler

import (
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/store"
)

// Two gangs of one model must not collide on the shards table's primary key.
// The ids used to be built from the model alone, so a second gang's create
// wrote over the first gang's rows — which is why a second create could only
// ever be a REPLACE.
func TestGangShardIDsCarryTheGang(t *testing.T) {
	a := gangShardID("llama-70b", "g0", "rpc-0")
	b := gangShardID("llama-70b", "g1", "rpc-0")
	if a == b {
		t.Fatalf("two gangs' part ids collide: %q", a)
	}
	if !strings.HasPrefix(a, agent.GangProcessPrefix("llama-70b", "g0")) {
		t.Errorf("a part id must sit under its gang's process prefix: %q vs %q",
			a, agent.GangProcessPrefix("llama-70b", "g0"))
	}
}

// The orphan sweep stops processes by prefix. It must match its own gang and
// NOTHING else: sweeping by the model's prefix to make room for one gang would
// stop a sibling gang's parts, which are serving requests at that moment.
func TestGangProcessMatchingIsExact(t *testing.T) {
	const model = "llama-70b"
	g0 := gangShardID(model, "g0", "rpc-0")
	g1 := gangShardID(model, "g1", "rpc-0")

	if !agent.IsGangProcess(g0, model, "g0") {
		t.Error("a gang must recognise its own part")
	}
	if agent.IsGangProcess(g1, model, "g0") {
		t.Error("g0's sweep must not match g1's part — that stops a serving gang")
	}
	if agent.IsGangProcess(g0, "other-model", "g0") {
		t.Error("another model's gang must never match")
	}
	// The model-wide prefix still covers every gang: that is what the whole
	// model's teardown uses.
	for _, id := range []string{g0, g1} {
		if !strings.HasPrefix(id, agent.ShardProcessPrefix(model)) {
			t.Errorf("%q must sit under the model prefix", id)
		}
	}
}

// Gang ids land inside process ids that are matched by prefix, so a '-' in one
// would make "g1" a prefix of "g1-2" and a teardown of one take the other.
func TestGangIDRefusesASeparator(t *testing.T) {
	for _, bad := range []string{"", "g-1", "G1", "g_1", "g.1", strings.Repeat("g", 33)} {
		if err := validGangID(bad); err == nil {
			t.Errorf("gang id %q must be refused", bad)
		}
	}
	for _, good := range []string{"g0", "g1", "blue", "a7"} {
		if err := validGangID(good); err != nil {
			t.Errorf("gang id %q must be accepted: %v", good, err)
		}
	}
}

// Two gangs may share a worker, and rpcPortBase is a catalog scalar they both
// start from — so a port is free or taken PER NODE, and a port handed out
// earlier in the same create is taken even though nothing is stored yet.
func TestPortAllocatorNeverRepeatsOnANode(t *testing.T) {
	a := &portAllocator{used: map[string]map[int]bool{}}
	if got := a.take("n1", 50052); got != 50052 {
		t.Fatalf("first take on a free node: %d", got)
	}
	if got := a.take("n1", 50052); got != 50053 {
		t.Fatalf("a port handed out in this same create is taken: got %d", got)
	}
	if got := a.take("n1", 50052); got != 50054 {
		t.Fatalf("and again: got %d", got)
	}
	// A different node is a different port space.
	if got := a.take("n2", 50052); got != 50052 {
		t.Fatalf("another node's ports are its own: got %d", got)
	}
}

// A model's gangs each hold a FULL copy of the weights, so the fleet really
// does carry the model's size once per gang. Dividing one model's bytes over
// both gangs' nodes made every node look half as full as it is, and the shard
// picker then sized the next gang against memory that was not free.
func TestGangKeysSeparateTwoCopiesOfOneModel(t *testing.T) {
	groups := store.GroupGangs([]store.Shard{
		{ModelID: "m", GangID: "g0", Role: "rpc", NodeID: "n1"},
		{ModelID: "m", GangID: "g0", Role: "rpc", NodeID: "n2"},
		{ModelID: "m", GangID: "g1", Role: "rpc", NodeID: "n3"},
		{ModelID: "m", GangID: "g1", Role: "rpc", NodeID: "n4"},
	})
	if len(groups) != 2 {
		t.Fatalf("two gangs, two groups: %v", groups)
	}
	for key, parts := range groups {
		if len(parts) != 2 {
			t.Errorf("gang %v holds 2 parts, got %d", key, len(parts))
		}
	}
}
