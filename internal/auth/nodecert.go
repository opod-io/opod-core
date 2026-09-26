package auth

// Worker identity from a CLIENT CERTIFICATE (R9.6, ADR-005's P1).
//
// WHAT WAS WRONG WITH ONLY HAVING HMAC. A worker proves who it is by holding a
// join token: a bearer secret on the way to the leader, and the HMAC key on the
// way back. The token is minted once, mounted in a Secret, and lives as long as
// the endpoint. It cannot be rotated without restarting the worker, a copy of it
// is enough to register as that node from anywhere on the network, and nothing
// about it says which PROCESS is holding it. For an operator whose fleet is
// audited, "the worker is whoever holds the string" is the part that fails a
// review, and it is the reason ADR-005 wrote mTLS down as the P1 the day HMAC
// shipped.
//
// WHAT THIS DOES. A worker's calls to its leader may carry a client certificate
// that the leader's node CA signed. The identity is in the handshake — nothing
// moves in the request body — and the leader reads the node id out of the
// certificate's SPIFFE URI SAN:
//
//	spiffe://<trust domain>/opod/node/<node id>
//
// That is the ONE shape an identity is read from. A DNS SAN, a common name and
// an email are all ignored, deliberately: a CN is free text that an operator's
// existing PKI hands out for other reasons, and reading an id out of it would
// make any certificate from a CA this leader trusts into a worker identity.
// SPIFFE's URI form is the one an issuer has to set on purpose.
//
// THREE MODES, and they live in the auth snapshot rather than the environment
// (adminapi.AuthSnapshot.NodeMTLS) because a fleet moves to mTLS one endpoint at
// a time and has to be able to move back without restarting a leader that is
// serving:
//
//   - off     — certificates are ignored. The join token is the credential, as
//     it has always been. This is the default and the GA path.
//   - allow   — a worker that presents a certificate is authenticated by it; one
//     that does not falls back to its token. This is how a fleet migrates.
//   - require — a register or heartbeat without a verified certificate is
//     refused. Only the join surface is affected: a chat client on the same
//     listener has no certificate and must not need one.
//
// WHY `require` IS NOT `tls.RequireAndVerifyClientCert`. One listener serves
// both the gateway and the join surface (there is one port, by design). Demanding
// a certificate in the handshake would refuse every prompt in the fleet to secure
// the join path. So the handshake asks for a certificate and verifies it IF one
// is offered (tls.VerifyClientCertIfGiven), and the requirement is enforced on
// the two node routes, where it belongs.
//
// REVOCATION travels in the same snapshot, as serial numbers. A leader must be
// able to refuse a certificate its own CA signed — that is the whole difference
// between an identity and a password — and the only thing already on the leader's
// side of the wire is the file it watches. A CRL or OCSP endpoint would put the
// manager on the request path, which ADR-001 does not allow.

import (
	"crypto/x509"
	"errors"
	"net/http"
	"strings"
)

// MTLS modes, as they are spelled in the auth snapshot.
const (
	MTLSOff     = "off"
	MTLSAllow   = "allow"
	MTLSRequire = "require"
)

// spiffeNodePath is the path component that makes a SPIFFE id a node identity:
// spiffe://<trust domain>/opod/node/<id>.
const spiffeNodePath = "/opod/node/"

// ErrCertRevoked is returned when the certificate verified fine and its serial
// is on the snapshot's revoked list. It is separate from "no identity" because
// the two are different answers to an operator: one is a worker that was never
// let in, the other is one that was and has been turned off.
var ErrCertRevoked = errors.New("certificate revoked")

// ParseMTLSMode normalises what a snapshot said. Anything unrecognised is OFF,
// never require: a typo in a policy file must not lock a fleet out of its own
// leaders, and the doctor row says which mode is in force.
func ParseMTLSMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case MTLSAllow:
		return MTLSAllow
	case MTLSRequire:
		return MTLSRequire
	default:
		return MTLSOff
	}
}

// NodeIDFromCert reads the node id out of a verified peer certificate, or "" if
// the certificate carries no node identity.
//
// Only a SPIFFE URI SAN is read — see the note at the top of this file for why
// the CN is not.
func NodeIDFromCert(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	for _, u := range cert.URIs {
		if u == nil || u.Scheme != "spiffe" {
			continue
		}
		i := strings.Index(u.Path, spiffeNodePath)
		if i != 0 {
			continue
		}
		id := strings.Trim(u.Path[len(spiffeNodePath):], "/")
		// A trailing path ("…/node/a/b") is not an id: an issuer that meant a
		// node said a node, and guessing at the first segment would let a
		// workload id be read as one.
		if id == "" || strings.Contains(id, "/") {
			continue
		}
		return id
	}
	return ""
}

// SerialHex is a certificate serial as the revoked list spells it: lowercase
// hex, no separators, no leading zeros. Both sides compute it the same way, so
// an operator can copy the number out of `openssl x509 -serial` (which prints
// uppercase) and it still matches — see NormaliseSerial.
func SerialHex(cert *x509.Certificate) string {
	if cert == nil || cert.SerialNumber == nil {
		return ""
	}
	return NormaliseSerial(cert.SerialNumber.Text(16))
}

// NormaliseSerial makes an operator-supplied serial comparable: case, "0x",
// colons and spaces are all how one tool or another prints the same number.
func NormaliseSerial(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "0x")
	s = strings.NewReplacer(":", "", " ", "", "-", "").Replace(s)
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "0"
	}
	return s
}

// NodeIdentity is what a leader learns about the process that called it.
type NodeIdentity struct {
	NodeID string // from the certificate's SPIFFE SAN
	Serial string // normalised, for the revoked list and the audit row
	// Present is false when the call carried no verified client certificate at
	// all, which in "allow" means fall back to the token.
	Present bool
}

// VerifiedNodeIdentity reads the identity off a request's TLS state.
//
// It trusts r.TLS.VerifiedChains rather than PeerCertificates: the handshake is
// configured with VerifyClientCertIfGiven, so a certificate that failed to
// verify never reaches a handler — but reading the verified chain rather than
// the offered leaf is the difference between "the CA vouched for this" and "the
// client sent this", and that distinction is exactly what the mode is about.
//
// revoked is the snapshot's list; the serials are normalised by the caller.
func VerifiedNodeIdentity(r *http.Request, revoked map[string]bool) (NodeIdentity, error) {
	if r == nil || r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		return NodeIdentity{}, nil
	}
	leaf := r.TLS.VerifiedChains[0][0]
	id := NodeIDFromCert(leaf)
	if id == "" {
		return NodeIdentity{}, nil
	}
	ni := NodeIdentity{NodeID: id, Serial: SerialHex(leaf), Present: true}
	if revoked[ni.Serial] {
		return ni, ErrCertRevoked
	}
	return ni, nil
}

// RevokedSet turns a snapshot's list into the map the check uses, normalising
// every entry so an operator's spelling does not decide whether a revocation
// takes effect.
func RevokedSet(serials []string) map[string]bool {
	if len(serials) == 0 {
		return nil
	}
	out := make(map[string]bool, len(serials))
	for _, s := range serials {
		if n := NormaliseSerial(s); n != "" && n != "0" {
			out[n] = true
		}
	}
	return out
}
