package leader

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod-core/internal/config"
	"github.com/opod-io/opod-core/internal/router"
	"github.com/opod-io/opod-core/internal/store"
	"github.com/opod-io/opod-core/pkg/adminapi"
)

func sessionDoor(t *testing.T, st store.Store) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Auth.AdminToken = "shared-admin-token" // every door of an endpoint holds it
	srv := NewServer(cfg, st, &downEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	srv.applySessionPolicy(&adminapi.PolicySession{TTLSec: 300, SignedRoute: true})
	return srv
}

// ADR-109 §3: a conversation that alternates two doors keeps its worker —
// the token one door minted routes the next turn at the other.
func TestAConversationAlternatingDoorsKeepsItsWorker(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "w1", Hostname: "w1", State: "ready", BootID: "boot-1", LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	doorA, doorB := sessionDoor(t, st), sessionDoor(t, st)

	// Turn 1 at door A: the answer carries the route token.
	r1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r1.Header.Set(SessionHeader, "conv-42")
	r1 = doorA.withSession(r1, nil, "m")
	r1 = r1.WithContext(router.WithTrace(r1.Context()))
	router.NoteNode(r1.Context(), "w1")
	rec := httptest.NewRecorder()
	w := doorA.wrapRoute(rec, r1, "m")
	w.WriteHeader(http.StatusOK)
	token := rec.Header().Get(SessionRouteHeader)
	if token == "" {
		t.Fatal("a 200 answer to a request with a session carried no route token")
	}

	// Turn 2 at door B, echoing it: routed to w1.
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r2.Header.Set(SessionHeader, "conv-42")
	r2.Header.Set(SessionRouteHeader, token)
	r2 = doorB.withSession(r2, nil, "m")
	if got := doorB.verifyRoute(ctx, token, router.SessionFrom(r2.Context()), "m", time.Now()); got != "w1" {
		t.Fatalf("door B read the token as %q, want w1", got)
	}
}

// A forged, expired, other-model, other-session or stale-boot token is a
// miss, never an error.
func TestARouteTokenThatDoesNotVerifyIsAMiss(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.Nodes().Upsert(ctx, store.Node{ID: "w1", Hostname: "w1", State: "ready", BootID: "boot-1", LastHeartbeat: time.Now()})
	srv := sessionDoor(t, st)
	sess := router.HashSession("conv")
	now := time.Now()
	good := srv.mintRoute(routeToken{Node: "w1", Boot: "boot-1", Model: "m", Session: sess, Expiry: now.Add(time.Minute).Unix()})
	if srv.verifyRoute(ctx, good, sess, "m", now) != "w1" {
		t.Fatal("a good token did not verify")
	}
	p, _, _ := strings.Cut(good, ".")
	for name, tc := range map[string]struct {
		token, session, model string
		at                    time.Time
	}{
		"forged":        {p + ".AAAA", sess, "m", now},
		"expired":       {good, sess, "m", now.Add(2 * time.Minute)},
		"other model":   {good, sess, "other", now},
		"other session": {good, router.HashSession("someone else"), "m", now},
		"garbage":       {"not-a-token", sess, "m", now},
		"stale boot":    {srv.mintRoute(routeToken{Node: "w1", Boot: "boot-0", Model: "m", Session: sess, Expiry: now.Add(time.Minute).Unix()}), sess, "m", now},
	} {
		if got := srv.verifyRoute(ctx, tc.token, tc.session, tc.model, tc.at); got != "" {
			t.Errorf("%s: verified as %q", name, got)
		}
	}
	if got := (&Server{cfg: &config.Config{Auth: config.AuthConfig{AdminToken: "another-endpoint"}}, store: st}).verifyRoute(ctx, good, sess, "m", now); got != "" {
		t.Errorf("a token from another endpoint's key verified as %q", got)
	}
}

// ADR-109 §5, by the admission queue alone: inside one flow a turn-5 request
// leaves before a turn-1 one, a turn-1 request held 8 s leaves before both,
// and two flows still alternate.
func TestTurnPriorityInsideAFlowIsCapped(t *testing.T) {
	now := time.Now()
	var q drrQueue
	t1 := &heldReq{flow: "a", turns: 1, heldAt: now, done: make(chan struct{})}
	t5 := &heldReq{flow: "a", turns: 5, heldAt: now, done: make(chan struct{})}
	q.push(t1)
	q.push(t5)
	rank := func(h *heldReq) float64 { return turnRank(h, now) }
	if got := q.popRanked(rank); got != t5 {
		t.Fatal("a later turn of an active session did not leave first")
	}
	q.push(t5)
	old := &heldReq{flow: "a", turns: 1, heldAt: now.Add(-8 * time.Second), done: make(chan struct{})}
	q.push(old)
	if got := q.popRanked(rank); got != old {
		t.Fatal("a first turn held 8 s did not outrank later turns")
	}
	// Two flows still alternate: b's request leaves before a's second.
	var q2 drrQueue
	a1 := &heldReq{flow: "a", turns: 4, heldAt: now, done: make(chan struct{})}
	a2 := &heldReq{flow: "a", turns: 4, heldAt: now, done: make(chan struct{})}
	b1 := &heldReq{flow: "b", turns: 1, heldAt: now, done: make(chan struct{})}
	q2.push(a1)
	q2.push(a2)
	q2.push(b1)
	first, second := q2.popRanked(rank), q2.popRanked(rank)
	if first.flow == second.flow {
		t.Fatalf("turn priority broke the round between flows: %s then %s", first.flow, second.flow)
	}
}

func TestSessionTurnsCountTheWindow(t *testing.T) {
	var tr sessionTurns
	now := time.Now()
	if n := tr.note("", now); n != 0 {
		t.Fatalf("no session: %d", n)
	}
	for i := 1; i <= 3; i++ {
		if n := tr.note("s", now.Add(time.Duration(i)*time.Second)); n != i {
			t.Fatalf("turn %d counted as %d", i, n)
		}
	}
	if n := tr.note("s", now.Add(5*time.Minute)); n != 1 {
		t.Fatalf("after the window a session starts again at 1, got %d", n)
	}
}

// /loadz's session block (T28.3): absent while routing.session is off, zeros
// when it is on and nothing names a session, and the counts once requests do.
func TestLoadzCountsSessionsAndTheirPins(t *testing.T) {
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := sessionDoor(t, st)
	now := time.Now()

	srv.applySessionPolicy(nil)
	if got := srv.sessionLoad(now); got != nil {
		t.Fatalf("routing.session off: %+v, want no block", got)
	}
	srv.applySessionPolicy(&adminapi.PolicySession{TTLSec: 300, SignedRoute: true})
	if got := srv.sessionLoad(now); got == nil || *got != (adminapi.SessionLoad{}) {
		t.Fatalf("on, nothing sent: %+v, want zeros", got)
	}

	plain := router.WithTrace(context.Background())
	srv.countSession(plain, now) // no session: not counted
	sess := router.WithTrace(router.WithSession(context.Background(), "conv-1", time.Minute))
	srv.countSession(sess, now)
	echoed := router.WithTrace(router.WithRoutedNode(router.WithSession(context.Background(), "conv-2", time.Minute), "w1"))
	srv.countSession(echoed, now)
	got := srv.sessionLoad(now)
	if got.Requests1m != 2 || got.RouteTokens1m != 1 || got.PinHits1m != 0 || got.PinMoves1m != 0 {
		t.Fatalf("counted %+v, want 2 requests, 1 route token, no pin outcome", *got)
	}
	if later := srv.sessionLoad(now.Add(61 * time.Second)); later.Requests1m != 0 {
		t.Fatalf("a minute later: %+v, want the window empty", *later)
	}
}
