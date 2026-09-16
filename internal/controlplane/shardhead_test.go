package controlplane

// R15.14 · the gang's head is the rank the control plane named, not the host
// with the most RAM. D4: the coordinator is a WORKER rank, never the leader —
// so a managed gang that asks for "local" is refused rather than quietly
// putting the coordinator back on the leader.

import (
	"context"
	"strings"
	"testing"
)

func TestManagedGangRefusesALeaderLocalHead(t *testing.T) {
	s := &Server{}
	err := s.CreateShards(context.Background(), CreateShardsRequest{ModelID: "m", Head: "local"})
	if err == nil {
		t.Fatal("a managed gang accepted head=local: the coordinator would run on the leader (D4)")
	}
	if !strings.Contains(err.Error(), "worker rank") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
}
