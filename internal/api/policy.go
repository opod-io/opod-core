package api

import (
	"sync/atomic"

	"github.com/opod-io/opod/internal/cache"
	"github.com/opod-io/opod/internal/guardrails"
	"github.com/opod-io/opod/internal/models"
)

// Policy is the request-path policy a Handler serves under: the catalog that
// bounds metric labels, the rate-limit buckets recordUsage reconciles against,
// the guardrail registry and the response cache. It is an immutable value the
// owner (the leader's controlplane) builds at boot and swaps atomically when a
// mounted policy snapshot changes — a request in flight finishes under the
// policy it started with. The zero value means "no policy": no catalog, no
// buckets, no guardrails, no cache. There are no package-level globals (P13-8).
type Policy struct {
	Catalog    []models.Entry
	Buckets    *BucketStore
	Guardrails *guardrails.Registry // nil = none configured
	Cache      cache.Cache          // nil = caching disabled
}

// WithGuardrails returns a copy with the registry replaced (nil clears it).
func (p *Policy) WithGuardrails(r *guardrails.Registry) *Policy {
	c := *p
	c.Guardrails = r
	return &c
}

// modelInCatalog reports whether model is a known catalog id — used to bound
// Prometheus label cardinality: a client-supplied string that never resolved
// is not safe as a label value.
func (p *Policy) modelInCatalog(model string) bool {
	for _, e := range p.Catalog {
		if e.ID == model {
			return true
		}
	}
	return false
}

var noPolicy = &Policy{}

// Policy returns the active policy (never nil).
func (h *Handler) Policy() *Policy {
	if p := h.policy.Load(); p != nil {
		return p
	}
	return noPolicy
}

// SetPolicy installs a policy atomically; nil resets to none.
func (h *Handler) SetPolicy(p *Policy) { h.policy.Store(p) }

var _ atomic.Pointer[Policy]
