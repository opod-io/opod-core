package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Worker auth uses HMAC-SHA256 with the per-node `worker_token` as the
// shared secret. The token is exchanged once at join time and never
// transmitted afterward — only signatures travel.
//
// Signing format (canonical string, "\n"-separated):
//
//	v1
//	<METHOD uppercase>
//	<PATH (no query string)>
//	<unix timestamp seconds>
//
// Signature: hex(HMAC-SHA256(token, canonical))
//
// Header set by the caller:
//
//	X-Opod-Auth: v=1,id=<nodeID>,ts=<unix>,sig=<hex>
//
// Receiver:
//  1. Parse the header (reject if malformed)
//  2. Reject if |now - ts| > MaxSkew (default 5 min) — replay window
//  3. Resolve secret for nodeID (worker: own token; leader: store lookup)
//  4. Recompute signature; constant-time compare
//
// Why no body signing: file uploads stream (multi-GB) and we don't want
// to buffer them into memory just to sign. The trust assumption is that
// the data plane runs over a trusted overlay (LAN / Tailscale) — HMAC
// gives integrity for the *control message* (which endpoint, by whom,
// when) and prevents trivial token sniffing.
const (
	HMACHeader     = "X-Opod-Auth"
	hmacVersion    = "v1"
	defaultMaxSkew = 5 * time.Minute
)

// SignRequest stamps an X-Opod-Auth header on req using token as the HMAC
// secret. Idempotent — calling twice replaces the header.
func SignRequest(req *http.Request, nodeID, token string) {
	ts := time.Now().Unix()
	sig := computeHMAC(token, req.Method, req.URL.Path, ts)
	req.Header.Set(HMACHeader, fmt.Sprintf("v=1,id=%s,ts=%d,sig=%s", nodeID, ts, sig))
}

// VerifyRequest pulls the X-Opod-Auth header off r and verifies it against
// the secret returned by lookup(nodeID). Returns the authenticated nodeID
// on success, or an error.
//
// lookup is called with the nodeID from the header. Returning ("", _) means
// the node is unknown and the request must be rejected.
func VerifyRequest(r *http.Request, lookup func(nodeID string) (token string, err error)) (string, error) {
	return verifyRequest(r, lookup, time.Now(), defaultMaxSkew)
}

func verifyRequest(r *http.Request, lookup func(nodeID string) (string, error), now time.Time, maxSkew time.Duration) (string, error) {
	raw := r.Header.Get(HMACHeader)
	if raw == "" {
		return "", errors.New("missing " + HMACHeader + " header")
	}
	parts, err := parseHMACHeader(raw)
	if err != nil {
		return "", fmt.Errorf("parse header: %w", err)
	}
	tsInt, perr := strconv.ParseInt(parts["ts"], 10, 64)
	if perr != nil {
		return "", fmt.Errorf("bad ts: %v", perr)
	}
	// Compare in integer seconds. Converting the attacker-supplied ts
	// to a time.Duration overflows for |skew| > ~292 years and can wrap
	// negative, bypassing the window check.
	maxSkewSecs := int64(maxSkew / time.Second)
	nowSecs := now.Unix()
	if tsInt < nowSecs-maxSkewSecs || tsInt > nowSecs+maxSkewSecs {
		return "", fmt.Errorf("ts %d outside the ±%s window of now — clock issue or replay attempt", tsInt, maxSkew)
	}
	token, lerr := lookup(parts["id"])
	if lerr != nil {
		return "", fmt.Errorf("lookup secret: %w", lerr)
	}
	if token == "" {
		return "", fmt.Errorf("unknown node id %q", parts["id"])
	}
	expected := computeHMAC(token, r.Method, r.URL.Path, tsInt)
	if !hmac.Equal([]byte(expected), []byte(parts["sig"])) {
		return "", errors.New("signature mismatch")
	}
	return parts["id"], nil
}

func computeHMAC(secret, method, path string, ts int64) string {
	canonical := strings.Join([]string{
		hmacVersion,
		strings.ToUpper(method),
		path,
		strconv.FormatInt(ts, 10),
	}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

// parseHMACHeader splits "v=1,id=X,ts=Y,sig=Z" into a map. Returns an error
// when required keys are missing or duplicated.
func parseHMACHeader(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			return nil, fmt.Errorf("malformed field %q", kv)
		}
		k := strings.TrimSpace(kv[:eq])
		v := strings.TrimSpace(kv[eq+1:])
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("duplicate field %q", k)
		}
		out[k] = v
	}
	for _, k := range []string{"v", "id", "ts", "sig"} {
		if _, ok := out[k]; !ok {
			return nil, fmt.Errorf("missing field %q", k)
		}
	}
	if out["v"] != "1" {
		return nil, fmt.Errorf("unsupported version %q", out["v"])
	}
	return out, nil
}
