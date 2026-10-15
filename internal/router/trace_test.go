package router

import (
	"context"
	"testing"
)

func TestRouteTrace(t *testing.T) {
	if NodeFrom(context.Background()) != "" {
		t.Fatal("no trace installed → empty")
	}
	NoteNode(context.Background(), "n_x") // no slot: must not panic
	ctx := WithTrace(context.Background())
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	NoteNode(child, "n_1")
	NoteNode(child, "n_2") // a fallback dispatch overwrites
	if got := NodeFrom(ctx); got != "n_2" {
		t.Fatalf("parent ctx sees the child's note, got %q", got)
	}
}

// A trace installed early (the leader's admission gate installs one before
// the API handler does) is the one the handler's WithTrace keeps, so both
// read the worker the router noted.
func TestWithTraceKeepsAnEarlierTrace(t *testing.T) {
	early := WithTrace(context.Background())
	late := WithTrace(early)
	NoteNode(late, "w1")
	if got := NodeFrom(early); got != "w1" {
		t.Fatalf("the earlier holder of the trace must read the node the router noted, got %q", got)
	}
}
