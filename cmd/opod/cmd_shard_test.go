package main

import (
	"errors"
	"reflect"
	"testing"

	"github.com/opod-io/opod/internal/scheduler"
)

// A shape the operator named is sent exactly as typed: the picker is not even
// consulted, however badly the shape fits the fleet.
func TestShardShapeExplicitIsNeverChanged(t *testing.T) {
	never := func() (scheduler.ShardPick, error) {
		t.Fatal("the picker ran although the operator named a shape")
		return scheduler.ShardPick{}, nil
	}
	for _, c := range []struct {
		name   string
		n      int
		nodes  []string
		tp, pp int
	}{
		{name: "a count", n: 5},
		{name: "a count of one", n: 1},
		{name: "named nodes", nodes: []string{"node-a", "node-b"}},
		{name: "a count and named nodes", n: 3, nodes: []string{"node-a"}},
		{name: "a tensor split", tp: 2},
		{name: "a pipeline split", pp: 4},
	} {
		n, nodes, why, err := shardShape(c.n, c.nodes, c.tp, c.pp, never)
		if err != nil || n != c.n || !reflect.DeepEqual(nodes, c.nodes) || why != "" {
			t.Errorf("%s: got n=%d nodes=%v why=%q err=%v; want it untouched", c.name, n, nodes, why, err)
		}
	}
}

// Only a bare `shard create <model>` takes the picker's answer, and it goes out
// as an explicit node list with the reason to print.
func TestShardShapeBareCreateAsksThePicker(t *testing.T) {
	n, nodes, why, err := shardShape(0, nil, 0, 0, func() (scheduler.ShardPick, error) {
		return scheduler.ShardPick{Shards: 2, Nodes: []string{"node-a", "node-b"}, Why: "because"}, nil
	})
	if err != nil || n != 2 || !reflect.DeepEqual(nodes, []string{"node-a", "node-b"}) || why != "because" {
		t.Fatalf("got n=%d nodes=%v why=%q err=%v", n, nodes, why, err)
	}
	refusal := errors.New("no split fits")
	if _, _, _, err := shardShape(0, nil, 0, 0, func() (scheduler.ShardPick, error) { return scheduler.ShardPick{}, refusal }); !errors.Is(err, refusal) {
		t.Fatalf("a refusal must reach the operator, got %v", err)
	}
}
