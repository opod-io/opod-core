package api

import (
	"testing"
	"time"
)

func TestKeyPool_SetDedupAndTrim(t *testing.T) {
	p := NewKeyPool()
	p.Set("groq", []string{" k1 ", "k2", "", "k1", "k2"})
	got := p.Candidates("groq")
	if len(got) != 2 || got[0] != "k1" || got[1] != "k2" {
		t.Fatalf("dedup/trim failed: %v", got)
	}
	if p.HasKeys("missing") {
		t.Error("HasKeys should be false for unconfigured vendor")
	}
	if p.Candidates("missing") != nil {
		t.Error("Candidates for unconfigured vendor should be nil")
	}
}

func TestKeyPool_RoundRobinSpreadsLoad(t *testing.T) {
	p := NewKeyPool()
	p.Set("groq", []string{"a", "b", "c"})
	// Each call advances the cursor, so the lead key rotates a, b, c, a…
	want := []string{"a", "b", "c", "a"}
	for i, w := range want {
		got := p.Candidates("groq")
		if got[0] != w {
			t.Errorf("call %d: lead key = %q, want %q (full=%v)", i, got[0], w, got)
		}
	}
}

func TestKeyPool_PenalizeParksAndRevives(t *testing.T) {
	now := time.Unix(1000, 0)
	p := NewKeyPool()
	p.now = func() time.Time { return now }
	p.Set("groq", []string{"a", "b"})

	p.Penalize("groq", "a", 60*time.Second)
	// "a" is parked → only "b" is live.
	got := p.Candidates("groq")
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("parked key still offered: %v", got)
	}

	// Advance past the cooldown → "a" comes back.
	now = now.Add(61 * time.Second)
	got = p.Candidates("groq")
	if len(got) != 2 {
		t.Fatalf("revived key not offered after cooldown: %v", got)
	}
}

func TestKeyPool_AllParkedReturnsSoonestFirst(t *testing.T) {
	now := time.Unix(1000, 0)
	p := NewKeyPool()
	p.now = func() time.Time { return now }
	p.Set("groq", []string{"a", "b"})

	p.Penalize("groq", "a", 90*time.Second) // free later
	p.Penalize("groq", "b", 30*time.Second) // free sooner

	// Every key is cooling down — we still return them, soonest-free first,
	// so the caller can try rather than hard-fail.
	got := p.Candidates("groq")
	if len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Fatalf("all-parked order wrong: %v (want [b a])", got)
	}
}

func TestKeyPool_PenalizeDefaultDuration(t *testing.T) {
	now := time.Unix(1000, 0)
	p := NewKeyPool()
	p.now = func() time.Time { return now }
	p.Set("groq", []string{"a", "b"})

	p.Penalize("groq", "a", 0) // 0 → default 30s
	now = now.Add(29 * time.Second)
	if got := p.Candidates("groq"); len(got) != 1 || got[0] != "b" {
		t.Fatalf("default cooldown not applied: %v", got)
	}
	now = now.Add(2 * time.Second) // now 31s in
	if got := p.Candidates("groq"); len(got) != 2 {
		t.Fatalf("default cooldown should have expired: %v", got)
	}
}
