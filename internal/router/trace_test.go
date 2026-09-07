package router

import (
	"context"
	"testing"
)

func TestRouteTrace(t *testing.T) {
	if NodeFrom(context.Background()) != "" {
		t.Fatal("no trace installed → empty")
	}
	noteNode(context.Background(), "n_x") // no slot: must not panic
	ctx := WithTrace(context.Background())
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	noteNode(child, "n_1")
	noteNode(child, "n_2") // a fallback dispatch overwrites
	if got := NodeFrom(ctx); got != "n_2" {
		t.Fatalf("parent ctx sees the child's note, got %q", got)
	}
}
