package agent

// T24.34: a worker is ready when its leader accepted its last register or
// heartbeat AND its engine answers. A worker its leader refuses serves
// nothing, so a disruption budget must not count it.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func readyzOf(s *Server, query string) (int, string) {
	rec := httptest.NewRecorder()
	s.readyz(rec, httptest.NewRequest(http.MethodGet, "/readyz"+query, nil))
	return rec.Code, rec.Body.String()
}

func TestReadyzIsTheJoinAndTheEngine(t *testing.T) {
	refused := errors.New("node n1 is bound to a different key")
	cases := []struct {
		name   string
		join   func(*JoinState)
		engine error
		query  string
		code   int
		says   string
	}{
		{"not registered yet", nil, nil, "", 503, "not registered"},
		{"joined, engine up", func(j *JoinState) { j.note("register", 200, nil) }, nil, "", 200, "ok"},
		{"joined, engine down", func(j *JoinState) { j.note("heartbeat", 200, nil) }, errors.New("connection refused"), "", 503, "engine"},
		{"refused by its leader, engine up", func(j *JoinState) {
			j.note("register", 200, nil)
			j.note("heartbeat", 403, refused)
		}, nil, "", 503, "HTTP 403"},
		{"leader unreachable", func(j *JoinState) { j.note("heartbeat", 0, errors.New("dial tcp: connection refused")) }, nil, "", 503, "did not reach the leader"},
		// A gang part's engine is started by its leader later: the join alone.
		{"gang part, joined, no engine yet", func(j *JoinState) { j.note("register", 200, nil) }, errors.New("connection refused"), "?join=only", 200, "ok"},
		{"gang part, refused", func(j *JoinState) { j.note("heartbeat", 403, refused) }, nil, "?join=only", 503, "HTTP 403"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := &JoinState{}
			if c.join != nil {
				c.join(j)
			}
			s := &Server{Engine: &loadStub{healthErr: c.engine}, Join: j}
			code, body := readyzOf(s, c.query)
			if code != c.code || !strings.Contains(body, c.says) {
				t.Fatalf("readyz = %d %q, want %d saying %q", code, body, c.code, c.says)
			}
		})
	}
}

// The agent's own calls write the state /readyz serves: a leader that accepts
// and then refuses turns the worker not ready.
func TestTheLoopRecordsTheLeadersAnswer(t *testing.T) {
	f := newFakeLeader(t)
	a := newAgent(f)
	a.Join, a.HTTP = &JoinState{}, f.srv.Client()
	if err := a.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ok, why := a.Join.Joined(); !ok {
		t.Fatalf("an accepted register is joined: %s", why)
	}
	f.set(403, 403)
	_, _ = a.Heartbeat(context.Background())
	if ok, why := a.Join.Joined(); ok || !strings.Contains(why, "403") {
		t.Fatalf("a refused heartbeat is not joined, and says why: %v %q", ok, why)
	}
	f.set(200, 200)
	_, _ = a.Heartbeat(context.Background())
	if ok, _ := a.Join.Joined(); !ok {
		t.Fatal("an accepted heartbeat is joined again")
	}
}

// A server with no agent behind it has no join state and is never ready: a
// readiness that defaulted to "joined" is the defect this replaces.
func TestReadyzWithNoJoinStateIsNotReady(t *testing.T) {
	if code, _ := readyzOf(&Server{Engine: &loadStub{}}, ""); code != 503 {
		t.Fatalf("readyz with no join state = %d, want 503", code)
	}
}
