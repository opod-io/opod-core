package leader

// Cache isolation (PLAN T26.8, feature "cache_salt").
//
// An engine's prefix cache — on the card, or in a tier behind it — lets one
// caller's request reuse a prefix another caller computed, and the time to
// first token shows that it did: someone sent the same text. The policy's
// routing.cacheIsolation says whose requests may share:
//
//   team  every key of one team shares, teams never do; a key with no team is
//         its own team (the admission flow's rule, admission_classes.go)
//   key   each key alone
//   none  every caller shares — a single-tenant endpoint that wants every hit
//
// Empty, or a value this binary does not know, is "team".
//
// The leader turns the caller into a salt — an HMAC of the team or key id
// under the endpoint's admin token, never the id itself, so the salt says
// nothing on the wire — and the router puts it on the request, where the
// driver of an engine that keys its cache by a salt sends it as `cache_salt`
// (vLLM, SGLang, TensorRT-LLM), and mixes it into the prefix pins. An engine
// with no salt (llama.cpp) shares its cache whatever the mode; the manager
// says so in its advisories. A request with no key — an endpoint that does
// not require keys — carries no salt and shares with every other such
// request: there is no caller to separate.
//
// The admin token is the one secret every front of an endpoint holds: the
// leader, and each `--role gateway` door, which needs it to mirror the
// registry. They derive the same salt for the same caller, so a request is
// keyed alike whichever front took it. A leader with no admin token (a bare
// `opod up`) salts under a key drawn at start: its salts change when it
// restarts, which costs one cold prefix per caller and separates no less.
//
// Not salted yet: the prefix-cache block index (prefixindex.go). Workers
// re-hash the engine's cache events from their token ids, which carry no
// salt, so with prefixBlockWeight on a worker is credited for blocks it holds
// under another caller's salt: a cold prefill there, never a shared one.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/opod-io/opod-sdk/adminapi"

	"github.com/opod-io/opod/internal/auth"
)

// cacheSaltState is the mode the policy set and the key salts are made with.
type cacheSaltState struct {
	mode    atomic.Pointer[string]
	keyOnce sync.Once
	key     []byte
}

// cacheIsolationMode normalises a policy value: "none", "key", or "team" for
// anything else, the empty string included.
func cacheIsolationMode(v string) (mode string, known bool) {
	switch v = strings.ToLower(strings.TrimSpace(v)); v {
	case adminapi.CacheIsolationNone, adminapi.CacheIsolationKey, adminapi.CacheIsolationTeam:
		return v, true
	case "":
		return adminapi.CacheIsolationTeam, true
	}
	return adminapi.CacheIsolationTeam, false
}

// setCacheIsolation applies the policy's mode and reports the one in force.
func (s *Server) setCacheIsolation(v string) (mode string, known bool) {
	mode, known = cacheIsolationMode(v)
	s.cacheSalt.mode.Store(&mode)
	return mode, known
}

// cacheIsolation is the mode in force: "team" until a policy says otherwise.
func (s *Server) cacheIsolation() string {
	if m := s.cacheSalt.mode.Load(); m != nil {
		return *m
	}
	return adminapi.CacheIsolationTeam
}

// saltKey is the HMAC key: the admin token, or a random key per process.
func (s *Server) saltKey() []byte {
	s.cacheSalt.keyOnce.Do(func() {
		if t := strings.TrimSpace(s.cfg.Auth.AdminToken); t != "" {
			s.cacheSalt.key = []byte(t)
			return
		}
		k := make([]byte, 32)
		_, _ = rand.Read(k) // crypto/rand.Read never fails (Go 1.24+)
		s.cacheSalt.key = k
	})
	return s.cacheSalt.key
}

// callerOf is who is calling, from the key the auth middleware attached and
// the team the auth snapshot gave it: two map reads, no store.
func (s *Server) callerOf(ctx context.Context) (keyID, team string) {
	k := auth.KeyFrom(ctx)
	if k == nil {
		return "", ""
	}
	if m := s.authf.ranks.Load(); m != nil {
		team = (*m)[k.ID].team
	}
	return k.ID, team
}

// requestCacheSalt is the salt for this request's caller under the mode in
// force, or "" (mode none, or no key).
func (s *Server) requestCacheSalt(ctx context.Context) string {
	mode := s.cacheIsolation()
	if mode == adminapi.CacheIsolationNone {
		return ""
	}
	keyID, team := s.callerOf(ctx)
	if keyID == "" {
		return ""
	}
	subject := "key\x00" + keyID
	if mode == adminapi.CacheIsolationTeam && team != "" {
		subject = "team\x00" + team
	}
	return cacheSalt(s.saltKey(), subject)
}

// cacheSalt is 128 bits of HMAC-SHA256 over a versioned subject, as hex:
// never empty (vLLM refuses an empty salt) and never the id it was made from.
func cacheSalt(key []byte, subject string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("opod-cache-salt/v1\x00" + subject))
	return hex.EncodeToString(m.Sum(nil)[:16])
}
