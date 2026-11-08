package leader

// A conversation keeps its worker (ADR-109, PLAN T28.3, feature
// "session_routing").
//
// A request names its conversation with X-Opod-Session, else the OpenAI
// `user` field; the router then pins the conversation, not the key, so two
// conversations under one key each land where their own cache is. Under
// routing.session.signedRoute every 200 answer to a request with a session
// carries X-Opod-Session-Route: the worker that served it, its boot id, the
// model and an expiry, HMAC-signed with the key every door of the endpoint
// already holds (the admin token, as the cache salt is). A door that sees it
// echoed verifies it and prefers that worker, so a conversation that
// alternates doors keeps its worker. A bad signature, an expiry, another
// model or a worker that restarted (another boot id) is a miss, never an
// error. Under routing.session.turnPriority admission ranks a flow's waiters
// by their session's turns (admission_classes.go).

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opod-io/opod-core/internal/router"
	"github.com/opod-io/opod-core/pkg/adminapi"
)

const (
	// SessionHeader names a request's conversation.
	SessionHeader = "X-Opod-Session"
	// SessionRouteHeader carries the signed route token, on an answer and,
	// echoed, on the next request.
	SessionRouteHeader = "X-Opod-Session-Route"
	// sessionTurnWindow is how far back a session's turns are counted.
	sessionTurnWindow = 120 * time.Second
	// sessionRouteDefaultTTL is a route token's life when no TTL is set.
	sessionRouteDefaultTTL = 5 * time.Minute
)

// sessionPolicy is routing.session as applied (nil = none).
type sessionPolicy struct {
	p atomic.Pointer[adminapi.PolicySession]
}

func (s *Server) sessionPolicyNow() adminapi.PolicySession {
	if p := s.session.p.Load(); p != nil {
		return *p
	}
	return adminapi.PolicySession{}
}

// applySessionPolicy is called with each policy snapshot.
func (s *Server) applySessionPolicy(p *adminapi.PolicySession) {
	s.session.p.Store(p)
	s.admission.turnPriority.Store(p != nil && p.TurnPriority)
}

// sessionOf reads the request's conversation id: the header, else the body's
// OpenAI `user` field. "" = none.
func sessionOf(r *http.Request, body []byte) string {
	if id := strings.TrimSpace(r.Header.Get(SessionHeader)); id != "" {
		return id
	}
	var b struct {
		User string `json:"user"`
	}
	if bytes.Contains(body, []byte(`"user"`)) {
		_ = json.Unmarshal(body, &b)
	}
	return strings.TrimSpace(b.User)
}

// withSession attaches the request's session, and the worker its echoed
// route token names when the token verifies.
func (s *Server) withSession(r *http.Request, body []byte, model string) *http.Request {
	id := sessionOf(r, body)
	if id == "" {
		return r
	}
	pol := s.sessionPolicyNow()
	ctx := router.WithSession(r.Context(), id, time.Duration(pol.TTLSec)*time.Second)
	if pol.SignedRoute {
		if node := s.verifyRoute(ctx, r.Header.Get(SessionRouteHeader), router.SessionFrom(ctx), model, time.Now()); node != "" {
			ctx = router.WithRoutedNode(ctx, node)
		}
	}
	return r.WithContext(ctx)
}

// routeToken is what a route token says.
type routeToken struct {
	Node    string `json:"n"`
	Boot    string `json:"b"`
	Model   string `json:"m"`
	Session string `json:"s"` // the hashed session it was minted for
	Expiry  int64  `json:"e"` // unix seconds
}

func (s *Server) signRoute(payload []byte) []byte {
	m := hmac.New(sha256.New, s.saltKey())
	m.Write([]byte("opod-session-route/v1\x00"))
	m.Write(payload)
	return m.Sum(nil)
}

func (s *Server) mintRoute(t routeToken) string {
	payload, _ := json.Marshal(t)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(s.signRoute(payload))
}

// verifyRoute returns the worker a route token names, or "" when it does not
// verify: a bad signature, past its expiry, another model or session, or a
// worker whose process changed since (boot id, ADR-042).
func (s *Server) verifyRoute(ctx context.Context, token, session, model string, now time.Time) string {
	if token == "" {
		return ""
	}
	p, sig, ok := strings.Cut(token, ".")
	if !ok {
		return ""
	}
	enc := base64.RawURLEncoding
	payload, err1 := enc.DecodeString(p)
	mac, err2 := enc.DecodeString(sig)
	if err1 != nil || err2 != nil || !hmac.Equal(mac, s.signRoute(payload)) {
		return ""
	}
	var t routeToken
	if json.Unmarshal(payload, &t) != nil || t.Node == "" || now.Unix() > t.Expiry || t.Model != model || t.Session != session {
		return ""
	}
	n, err := s.store.Nodes().Get(ctx, t.Node)
	if err != nil || n == nil || n.BootID != t.Boot {
		return ""
	}
	return t.Node
}

// routeWriter sets the route token on a 200 answer to a request with a
// session, from the worker the router noted, before the header goes out.
type routeWriter struct {
	http.ResponseWriter
	s       *Server
	ctx     context.Context
	model   string
	written bool
}

func (w *routeWriter) WriteHeader(code int) {
	if !w.written {
		w.written = true
		if code == http.StatusOK {
			w.s.setRoute(w.ResponseWriter.Header(), w.ctx, w.model)
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *routeWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush passes through, so a stream is not held back by the wrapper.
func (w *routeWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection beneath.
func (w *routeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) setRoute(h http.Header, ctx context.Context, model string) {
	session := router.SessionFrom(ctx)
	node := router.NodeFrom(ctx)
	if session == "" || node == "" || strings.Contains(node, ":") {
		return // no session, no worker, or a gang (its key is not a node)
	}
	n, err := s.store.Nodes().Get(ctx, node)
	if err != nil || n == nil {
		return
	}
	ttl := time.Duration(s.sessionPolicyNow().TTLSec) * time.Second
	if ttl <= 0 {
		ttl = sessionRouteDefaultTTL
	}
	h.Set(SessionRouteHeader, s.mintRoute(routeToken{Node: node, Boot: n.BootID, Model: model, Session: session, Expiry: time.Now().Add(ttl).Unix()}))
}

// wrapRoute is w with the route token set on its answer, when the policy asks
// for one and the request has a session.
func (s *Server) wrapRoute(w http.ResponseWriter, r *http.Request, model string) http.ResponseWriter {
	if !s.sessionPolicyNow().SignedRoute || router.SessionFrom(r.Context()) == "" {
		return w
	}
	return &routeWriter{ResponseWriter: w, s: s, ctx: r.Context(), model: model}
}

// sessionTurns counts each session's recent requests, for turn priority.
type sessionTurns struct {
	mu   sync.Mutex
	seen map[string][]time.Time // hashed session → its requests in the window
	n    int                    // notes since the last sweep
}

// sessionTurnsMax bounds what one session keeps: past the cap, turns count as
// the cap anyway (turnPriorityCap).
const sessionTurnsMax = 8

// note records a request of session at now and returns its turn in the
// window, this one included; 0 for no session.
func (t *sessionTurns) note(session string, now time.Time) int {
	if session == "" {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen == nil {
		t.seen = map[string][]time.Time{}
	}
	ts := append(t.seen[session], now)
	cut := 0
	for cut < len(ts) && (now.Sub(ts[cut]) > sessionTurnWindow || len(ts)-cut > sessionTurnsMax) {
		cut++
	}
	ts = ts[cut:]
	t.seen[session] = ts
	if t.n++; t.n >= 1024 { // drop sessions that went quiet
		t.n = 0
		for k, v := range t.seen {
			if now.Sub(v[len(v)-1]) > sessionTurnWindow {
				delete(t.seen, k)
			}
		}
	}
	return len(ts)
}
