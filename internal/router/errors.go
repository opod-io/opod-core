package router

import (
	"errors"
	"strings"
)

// Sentinel errors classify why a primary candidate failed so the router
// can pick a typed fallback list (`fallback_on_context_length`,
// `fallback_on_content_policy`) instead of the generic chain.
//
// Engine drivers and egress adapters wrap their native error shapes
// with these whenever they can recognize them; the heuristic classifier
// below covers everything else (string matching on the error message).
var (
	// ErrContextLength means the prompt + max_tokens exceeded the
	// model's context window. Long-context variants are typically a
	// good fallback target.
	ErrContextLength = errors.New("context_length_exceeded")

	// ErrContentPolicy means the upstream refused the request on
	// content-policy grounds (Anthropic refusal, OpenAI policy stop,
	// Bedrock guardrail). A more permissively-aligned open-weight model
	// is typically a good fallback target.
	ErrContentPolicy = errors.New("content_policy_violation")
)

// ErrorClass is the typed bucket a failure falls into. ClassGeneric is
// the default — anything we can't pin to a more specific bucket.
type ErrorClass int

const (
	ClassGeneric ErrorClass = iota
	ClassContextLength
	ClassContentPolicy
)

// String renders the class as a short tag suitable for trace
// attributes / metric labels. Matches the planning doc's documented
// values: "generic", "context-length", "content-policy".
func (c ErrorClass) String() string {
	switch c {
	case ClassContextLength:
		return "context-length"
	case ClassContentPolicy:
		return "content-policy"
	default:
		return "generic"
	}
}

// ClassifyError returns the ErrorClass that best matches `err`. Order
// of precedence:
//
//  1. errors.Is against the typed sentinels (engine drivers / egress
//     adapters wrap their native shapes).
//  2. Substring match on the error message — broader, catches the
//     "I didn't bother wrapping" path. Substrings chosen to match the
//     phrasings llama.cpp / vLLM / Ollama / Anthropic / OpenAI actually
//     emit; new patterns can be added without breaking callers because
//     the function is conservative — it returns ClassGeneric when
//     nothing matches.
//
// Returns ClassGeneric for `nil`; callers should check the err
// independently.
func ClassifyError(err error) ErrorClass {
	if err == nil {
		return ClassGeneric
	}
	if errors.Is(err, ErrContextLength) {
		return ClassContextLength
	}
	if errors.Is(err, ErrContentPolicy) {
		return ClassContentPolicy
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "context length"),
		strings.Contains(msg, "context_length_exceeded"),
		strings.Contains(msg, "maximum context length"),
		strings.Contains(msg, "n_ctx"),
		strings.Contains(msg, "too many tokens"),
		strings.Contains(msg, "max input length"):
		return ClassContextLength
	case strings.Contains(msg, "content policy"),
		strings.Contains(msg, "content_policy_violation"),
		strings.Contains(msg, "refused to answer"),
		strings.Contains(msg, "unable to help"),
		strings.Contains(msg, "guardrail"),
		strings.Contains(msg, "policy violation"):
		return ClassContentPolicy
	}
	return ClassGeneric
}
