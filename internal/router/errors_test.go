package router

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorClass
	}{
		{"nil → generic", nil, ClassGeneric},
		{"sentinel context_length", ErrContextLength, ClassContextLength},
		{"wrapped context_length", fmt.Errorf("wrapping: %w", ErrContextLength), ClassContextLength},
		{"sentinel content_policy", ErrContentPolicy, ClassContentPolicy},
		{"wrapped content_policy", fmt.Errorf("upstream said: %w", ErrContentPolicy), ClassContentPolicy},

		// Substring heuristics — patterns each upstream tends to emit.
		{"llama.cpp n_ctx", errors.New("n_ctx exceeded by 200 tokens"), ClassContextLength},
		{"OpenAI maximum context length", errors.New("This model's maximum context length is 8192 tokens"), ClassContextLength},
		{"vLLM too many tokens", errors.New("decode: too many tokens in prompt"), ClassContextLength},
		{"Anthropic context_length_exceeded", errors.New("context_length_exceeded: prompt is 33001 tokens"), ClassContextLength},

		{"Anthropic refusal phrase", errors.New("I'm unable to help with that request"), ClassContentPolicy},
		{"OpenAI content_policy_violation", errors.New("content_policy_violation: detected unsafe input"), ClassContentPolicy},
		{"Bedrock guardrail", errors.New("guardrail intervened: input flagged"), ClassContentPolicy},

		{"unrelated error stays generic", errors.New("connection refused"), ClassGeneric},
		{"generic 500", errors.New("internal server error"), ClassGeneric},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyError(c.err)
			if got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

func TestMaybeSwapTypedChain_SwapsOnTypedError(t *testing.T) {
	r := &Router{}
	chains := FallbackChains{
		Generic:       []string{"g1", "g2"},
		ContextLength: []string{"long-1", "long-2"},
		ContentPolicy: []string{"perm-1"},
	}
	// A real (no-op-when-no-provider) span just to satisfy the type;
	// SetAttributes is fine on it.
	_, span := tracer.Start(t.Context(), "test")
	defer span.End()

	t.Run("context-length error swaps", func(t *testing.T) {
		initial := []string{"primary", "g1", "g2"}
		after := r.maybeSwapTypedChain(initial, chains, ErrContextLength, "chat", 0, span)
		want := []string{"primary", "long-1", "long-2"}
		if !sameOrder(after, want) {
			t.Errorf("after = %v, want %v", after, want)
		}
	})
	t.Run("content-policy error swaps", func(t *testing.T) {
		initial := []string{"primary", "g1", "g2"}
		after := r.maybeSwapTypedChain(initial, chains, ErrContentPolicy, "chat", 0, span)
		want := []string{"primary", "perm-1"}
		if !sameOrder(after, want) {
			t.Errorf("after = %v, want %v", after, want)
		}
	})
	t.Run("generic error keeps generic chain", func(t *testing.T) {
		initial := []string{"primary", "g1", "g2"}
		after := r.maybeSwapTypedChain(initial, chains, errors.New("random 500"), "chat", 0, span)
		if !sameOrder(after, initial) {
			t.Errorf("generic should not swap, got %v", after)
		}
	})
}

func TestFallbackChains_PickFor(t *testing.T) {
	chains := FallbackChains{
		Generic:       []string{"g1", "g2"},
		ContextLength: []string{"long-1"},
		// ContentPolicy intentionally empty — should fall back to Generic.
	}
	if got := chains.PickFor(ClassContextLength); len(got) != 1 || got[0] != "long-1" {
		t.Errorf("context-length pick: %v", got)
	}
	if got := chains.PickFor(ClassContentPolicy); len(got) != 2 || got[0] != "g1" {
		t.Errorf("content-policy pick should fall back to generic: %v", got)
	}
	if got := chains.PickFor(ClassGeneric); len(got) != 2 || got[0] != "g1" {
		t.Errorf("generic pick: %v", got)
	}
}
