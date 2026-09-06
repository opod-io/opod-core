package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRouteStoreRoundtrip(t *testing.T) {
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// Unset → nil so the caller can fall back to the computed default.
	got, err := st.Route().Get(ctx)
	if err != nil {
		t.Fatalf("Get empty: %v", err)
	}
	if got != nil {
		t.Fatalf("unset chain = %v, want nil", got)
	}

	chain := []string{"groq/llama-3.3-70b", "gemini/gemini-2.0-flash", "qwen3.6-27b"}
	if err := st.Route().Set(ctx, chain); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err = st.Route().Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got, chain) {
		t.Fatalf("roundtrip = %v, want %v", got, chain)
	}

	// Set replaces (not appends), and persists order.
	repl := []string{"deepseek/deepseek-chat", "qwen3.6-27b"}
	if err := st.Route().Set(ctx, repl); err != nil {
		t.Fatalf("Set replace: %v", err)
	}
	got, _ = st.Route().Get(ctx)
	if !reflect.DeepEqual(got, repl) {
		t.Fatalf("after replace = %v, want %v", got, repl)
	}

	// nil clears back to "unset" (empty stored → Get yields empty, length 0).
	if err := st.Route().Set(ctx, nil); err != nil {
		t.Fatalf("Set nil: %v", err)
	}
	got, _ = st.Route().Get(ctx)
	if len(got) != 0 {
		t.Fatalf("after reset = %v, want empty", got)
	}
}
