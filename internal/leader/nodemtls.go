package leader

// The join path's certificate gate (R9.6, ADR-005's P1).
//
// `POST /admin/v1/nodes/register` and `/heartbeat` are the two routes a worker
// calls, and until now the only thing that identified it was the join token it
// held. This middleware lets a CERTIFICATE be that identity instead: the leader
// reads the node id out of the client certificate its node CA signed, and the
// worker needs no shared secret to be believed.
//
// Three properties, and each one is a sentence the earlier design could not say:
//
//  1. THE CERTIFICATE NAMES THE NODE. A worker whose certificate says `n_a`
//     cannot register or heartbeat as `n_b`. The token path has a version of
//     this (first-use binding: the key that first registers an id owns it), but
//     it binds a KEY to an id — one leaked token still registers as the node it
//     was minted for, from anywhere. A certificate binds the PROCESS, and the
//     mismatch is refused at the door with the id it claimed.
//  2. A REVOKED CERTIFICATE CANNOT JOIN. That is the whole difference between an
//     identity and a password: the serial is on the list this leader watches, and
//     the answer is 403 with the reason, not a silent failure to converge.
//  3. `require` MEANS REQUIRE, on these two routes only. A chat client on the
//     same listener presents no certificate and must not need one, which is why
//     the handshake only asks (VerifyClientCertIfGiven) and the demand lives
//     here.
//
// The mode, the CA and the revoked list come from the auth snapshot the leader
// already watches, so all three change without a restart, and `off` — the
// default and the GA path — leaves every byte of this dormant.

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/opod-io/opod/internal/auth"
)

type nodeCertCtxKey struct{}

// nodeCertIdentity is the verified identity on this request, if any.
func nodeCertIdentity(ctx context.Context) auth.NodeIdentity {
	if v, ok := ctx.Value(nodeCertCtxKey{}).(auth.NodeIdentity); ok {
		return v
	}
	return auth.NodeIdentity{}
}

// joinPaths are the two routes a worker calls, and the only ones `require`
// applies to. It is a fixed list rather than a prefix: /admin/v1 also carries the
// MANAGER's calls, which present an admin token and no certificate, and a
// `require` that reached them would lock the manager out of the leader it
// just configured.
var joinPaths = map[string]bool{
	"/admin/v1/nodes/register":  true,
	"/admin/v1/nodes/heartbeat": true,
}

// nodeCertGate verifies the client certificate and enforces the snapshot's mode.
// It is mounted as the FIRST middleware of /admin/v1 — before the key middleware
// — for two reasons: in `require` a call with no certificate must be refused
// whatever token it carries, and a verified certificate has to be able to stand
// in for a key the caller does not have (which is the point: a worker joins with
// no shared secret).
func (s *Server) nodeCertGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pol := s.nodeMTLS()
		if pol.Mode == auth.MTLSOff {
			next.ServeHTTP(w, r)
			return
		}
		if joinPaths[r.URL.Path] && handshakedUnderOlderPolicy(r.Context(), pol.gen) {
			// The connection was handshaked before this CA or mode (T24.36):
			// it presented the certificate the old policy asked for, or none,
			// and keep-alive would keep it that way for as long as the worker
			// heartbeats. Close it after this answer so the worker re-dials and
			// its next handshake is verified against the policy in force. The
			// request itself is judged as usual: allow serves it on its token,
			// require refuses it below for want of a certificate.
			w.Header().Set("Connection", "close")
		}
		id, err := auth.VerifiedNodeIdentity(r, pol.Revoked)
		switch {
		case err == auth.ErrCertRevoked:
			s.log.Warn("join refused: revoked certificate", "node", id.NodeID, "serial", id.Serial)
			s.logEvent("node.cert-refused", id.NodeID, map[string]any{"reason": "revoked", "serial": id.Serial})
			writeJSONError(w, http.StatusForbidden, "certificate revoked (serial "+id.Serial+")")
			return
		case err != nil:
			writeJSONError(w, http.StatusForbidden, "client certificate: "+err.Error())
			return
		}
		if !id.Present && pol.Mode == auth.MTLSRequire && joinPaths[r.URL.Path] {
			// Deliberately says which of the two it is. "unauthorized" would
			// send an operator looking for a bad token when the worker's
			// certificate is the thing that is missing, and this endpoint has
			// asked for certificates.
			writeJSONError(w, http.StatusUnauthorized,
				"this endpoint requires a worker client certificate (nodeMtls=require) and the call presented none")
			return
		}
		if id.Present {
			ctx := context.WithValue(r.Context(), nodeCertCtxKey{}, id)
			// The certificate IS the credential: node scope, so the key
			// middleware asks for nothing and RequireScopeAny("admin","node")
			// lets the join through. A worker that points this certificate at
			// an admin-only route gets node scope and is refused there, which
			// is what a worker identity should be able to reach.
			ctx = auth.WithScope(ctx, "node")
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

// checkNodeCertMatches is the body check: the id a request claims against the id
// the certificate carries. Returns the error to answer with, or nil.
//
// Called by the two handlers rather than the middleware because only they have
// decoded the body, and the claim is in the body.
func (s *Server) checkNodeCertMatches(r *http.Request, claimed string) error {
	id := nodeCertIdentity(r.Context())
	if !id.Present || id.NodeID == claimed {
		return nil
	}
	s.log.Warn("join refused: certificate names another node", "certificate", id.NodeID, "claimed", claimed)
	s.logEvent("node.cert-refused", id.NodeID, map[string]any{"reason": "id-mismatch", "claimed": claimed, "serial": id.Serial})
	return errCertNodeMismatch{cert: id.NodeID, claimed: claimed}
}

type errCertNodeMismatch struct{ cert, claimed string }

func (e errCertNodeMismatch) Error() string {
	return "client certificate names node " + e.cert + ", the request claims " + e.claimed
}

// handshakeMTLSGen is the node-mTLS policy gen a TLS connection was handshaked
// under, plus one; zero means the connection recorded none (plain HTTP, or a
// listener this leader did not build), and such a connection is never closed
// for it — a re-dial could not bring it a certificate.
type handshakeMTLSGen struct{ v atomic.Uint64 }

type handshakeMTLSGenKey struct{}

// withHandshakeMTLSGen is the leader listener's ConnContext: every connection
// gets a slot its TLS handshake fills in. net/http hands this context to the
// handshake (ClientHelloInfo.Context) and derives every request's context from
// it, which is how the join gate learns what the handshake was asked.
func withHandshakeMTLSGen(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, handshakeMTLSGenKey{}, new(handshakeMTLSGen))
}

// recordHandshakeMTLSGen notes the policy gen a handshake was served under.
func recordHandshakeMTLSGen(ctx context.Context, gen uint64) {
	if h, ok := ctx.Value(handshakeMTLSGenKey{}).(*handshakeMTLSGen); ok {
		h.v.Store(gen + 1)
	}
}

// handshakedUnderOlderPolicy reports whether the request's connection was
// handshaked under a policy gen other than cur.
func handshakedUnderOlderPolicy(ctx context.Context, cur uint64) bool {
	h, ok := ctx.Value(handshakeMTLSGenKey{}).(*handshakeMTLSGen)
	if !ok {
		return false
	}
	v := h.v.Load()
	return v != 0 && v-1 != cur
}
