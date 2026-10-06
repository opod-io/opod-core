package leader

// A stale auth snapshot, and how old the one in force is (ADR-085).
//
// The leader authenticates from the snapshot mounted beside it and never asks
// anyone whether a key is valid, so a manager outage does not stop serving —
// and does not stop revocation either, because anyone with RBAC on the Secret
// can append a tombstone. What an outage does do is let the snapshot grow old.
// Whether an old snapshot should still admit keys is a trade between
// availability and revocation that only the endpoint's owner can make, so it
// is a setting with no default judgement: policy auth.maxSnapshotAgeSec.
//
//   - 0, or a snapshot with no issuedAt: fail-static — exactly the behaviour
//     before the field existed. Serving continues on whatever snapshot is held.
//   - > 0 and issuedAt older than that: the gateway refuses keyed traffic with
//     a reason a person can read, until a fresher snapshot lands.
//
// The age is visible whether or not it is bounded (/loadz auth_snapshot), so
// nobody finds a week-old auth file by being breached.

import (
	"fmt"
	"net/http"
	"time"
)

// authSnapshotStatus is the auth snapshot as /loadz reports it. Core-local
// until the next SDK tag carries it on adminapi.Load, with these JSON names.
//
// /loadz is unauthenticated (it is a probe, served on the probe port too).
// Nothing here is a credential: a revision string, a timestamp, counts and a
// bound — the same grade of fact as plan_revision beside it.
type authSnapshotStatus struct {
	Revision string `json:"revision"`
	// IssuedAt and AgeSec are absent when the snapshot does not say when it
	// was written: its age is unknown, and no bound is enforced on it.
	IssuedAt string `json:"issued_at,omitempty"`
	AgeSec   *int64 `json:"age_s,omitempty"`
	// MaxAgeSec is the policy bound; 0 = fail-static.
	MaxAgeSec int64 `json:"max_age_s"`
	// Stale is true while the gateway refuses keyed traffic for it.
	Stale       bool `json:"stale"`
	Keys        int  `json:"keys"`
	RevokedKeys int  `json:"revoked_keys"`
}

// authSnapshotState reports the snapshot in force, or nil when none has been
// applied (a standalone leader).
func (s *Server) authSnapshotState(now time.Time) *authSnapshotStatus {
	s.authf.mu.Lock()
	present, rev, keys, revoked := s.authf.present, s.authf.revision, s.authf.keys, s.authf.revoked
	s.authf.mu.Unlock()
	if !present {
		return nil
	}
	out := &authSnapshotStatus{Revision: rev, MaxAgeSec: s.policy.maxAuthAgeSec.Load(), Keys: keys, RevokedKeys: revoked}
	if ns := s.authf.issuedAt.Load(); ns != 0 {
		issued := time.Unix(0, ns)
		age := int64(max(now.Sub(issued), 0) / time.Second)
		out.IssuedAt = issued.UTC().Format(time.RFC3339)
		out.AgeSec = &age
	}
	out.Stale = s.staleAuthReason(now) != ""
	return out
}

// staleAuthReason says why keyed traffic is refused for the snapshot's age,
// or "" when it is not: no bound, no known issue time, or still inside it.
//
// A snapshot stamped in the future (the manager's clock ahead of this one) is
// age 0, never stale: the bound protects against a snapshot nobody refreshed,
// and a clock skew is not that.
func (s *Server) staleAuthReason(now time.Time) string {
	bound := s.policy.maxAuthAgeSec.Load()
	ns := s.authf.issuedAt.Load()
	if bound <= 0 || ns == 0 {
		return ""
	}
	issued := time.Unix(0, ns)
	age := now.Sub(issued)
	if age <= time.Duration(bound)*time.Second {
		return ""
	}
	return fmt.Sprintf("this endpoint's auth snapshot was issued %s ago (at %s), past its limit of %ds "+
		"(policy auth.maxSnapshotAgeSec): API keys are refused until a fresher snapshot arrives; retry later",
		age.Truncate(time.Second), issued.UTC().Format(time.RFC3339), bound)
}

// staleAuthGate refuses /v1 traffic while the auth snapshot is past its bound
// and keys are required.
//
// Why 503 and not 401 or 403: the client did nothing wrong. Its key may well
// be valid — the leader just can no longer vouch for ANY key, because the list
// it would check against is older than the owner allows. A 401 would tell the
// caller to rotate a key that is fine; 503 + Retry-After says "this endpoint,
// not you, and not for ever", and clears by itself when the snapshot is
// re-issued. The body is OpenAI-shaped like every other refusal.
//
// Where it sits: on /v1 only, and BEFORE load accounting and key evaluation.
//   - /v1 only, because the admin surface authenticates with the manager's
//     token, the local admin key and node tokens — none of them snapshot keys.
//     Gating it would refuse every worker heartbeat and turn a stale snapshot
//     into the total outage ADR-085 §2 forbids.
//   - Before trackLoad, because a refusal by policy is not demand: counted,
//     it would land in unavailable_1m — the autoscaler's wake signal — and
//     start GPUs for traffic the leader is refusing on purpose.
//   - Before the key check, because a key cannot be judged against a list the
//     owner has declared too old to trust.
//
// Keyless endpoints (requireKeys off) are untouched: there is no key to
// revoke, so there is nothing for staleness to protect.
func (s *Server) staleAuthGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.requireKeys() {
			if why := s.staleAuthReason(time.Now()); why != "" {
				w.Header().Set("Retry-After", "30")
				writeJSONError(w, http.StatusServiceUnavailable, why)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
