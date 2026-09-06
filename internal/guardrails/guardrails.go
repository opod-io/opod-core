// Package guardrails defines the plug-in interface for content checks
// and ships drivers (currently: generic webhook). A guardrail can run
// before the engine sees the request (`pre`), after the response is
// known (`post`), or in observe-only mode (`logging_only`) where it
// records the action but doesn't intervene.
package guardrails

import "context"

// Mode declares when a guardrail runs. The chain is walked in
// configuration order; the first `block` short-circuits.
type Mode string

const (
	ModePre         Mode = "pre"
	ModePost        Mode = "post"
	ModeLoggingOnly Mode = "logging_only"
)

// Action is a guardrail's verdict.
//
//   - ActionAllow: pass the (possibly-rewritten) payload through.
//   - ActionBlock: refuse the request; the gateway returns HTTP 403
//     guardrail_blocked with the Reason string.
//   - ActionRewrite: replace the payload with NewBody and continue.
//     The guardrail is responsible for producing a valid OpenAI-shape
//     body — Opod forwards it verbatim.
//   - ActionFlag: record the event but neither block nor rewrite.
//     Same effect as ActionAllow, intended for ModeLoggingOnly.
type Action struct {
	Kind    string // "allow" | "block" | "rewrite" | "flag"
	NewBody []byte // populated when Kind == "rewrite"
	Reason  string // populated when Kind == "block" or "flag"
}

// Allow, Block, Rewrite, Flag are constructors that keep call sites
// readable.
func Allow() Action              { return Action{Kind: "allow"} }
func Block(reason string) Action { return Action{Kind: "block", Reason: reason} }
func Rewrite(body []byte) Action { return Action{Kind: "rewrite", NewBody: body} }
func Flag(reason string) Action  { return Action{Kind: "flag", Reason: reason} }

// Guardrail is the small interface each driver implements.
type Guardrail interface {
	Name() string
	Mode() Mode
	// Check inspects the body (request body for pre / logging_only,
	// aggregated response body for post) and returns its verdict. A
	// non-nil error short-circuits the chain with the guardrail's
	// configured fail-open / fail-closed posture handled by the
	// caller — the interface itself doesn't mandate either.
	Check(ctx context.Context, body []byte) (Action, error)
}

// Chain holds the ordered guardrails for a single mode. The dispatcher
// (in the api package) walks the chain on every request.
type Chain struct {
	guards []Guardrail
}

// NewChain returns a chain in the given order. nil entries are
// dropped silently.
func NewChain(gs ...Guardrail) *Chain {
	out := &Chain{}
	for _, g := range gs {
		if g != nil {
			out.guards = append(out.guards, g)
		}
	}
	return out
}

// Guards returns the underlying ordered slice (used by the
// controlplane for "/admin/v1/guardrails" listing + tests).
func (c *Chain) Guards() []Guardrail {
	if c == nil {
		return nil
	}
	return c.guards
}

// IsEmpty reports whether the chain has any guards. Cheap so callers
// can short-circuit the hook entirely on the request hot path when no
// guardrails are configured.
func (c *Chain) IsEmpty() bool {
	return c == nil || len(c.guards) == 0
}

// Registry groups the three mode chains so the api handler can pull
// the right one for each hook point. Built once at server startup
// from the YAML config.
type Registry struct {
	Pre         *Chain
	Post        *Chain
	LoggingOnly *Chain
}

// IsEmpty reports whether every chain in the registry is empty —
// useful for the no-guardrails-configured short-circuit.
func (r *Registry) IsEmpty() bool {
	return r == nil ||
		(r.Pre.IsEmpty() && r.Post.IsEmpty() && r.LoggingOnly.IsEmpty())
}
