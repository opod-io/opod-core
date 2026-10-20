package leader

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opod-io/opod-sdk/adminapi"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// PLAN T11.31: a sleeper whose card a manager gave away while it slept was
// resumed in place by the first request — in 0.44 s, without asking anyone —
// and the card held two claims until the manager's next look. A worker the
// policy marks is never resumed by a request: the request is held by the
// admission hold, and served once the manager has the worker serving again.
func TestAMarkedSleeperIsHeldNotResumed(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Router.HeartbeatMaxAgeSeconds = 30
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	var resumes atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/model/resume" {
			resumes.Add(1)
			_, _ = w.Write([]byte(`{"status":"resumed"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer worker.Close()
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "w1", Hostname: "w1", State: "ready", Address: strings.TrimPrefix(worker.URL, "http://"), WorkerToken: "wtok", LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	heartbeat := func(sleeping bool) {
		t.Helper()
		body := `{"id":"w1","loaded_models":["m"],"sleeping":false}`
		if sleeping {
			body = `{"id":"w1","loaded_models":["m"],"sleeping":true}`
		}
		rec := httptest.NewRecorder()
		srv.heartbeatNode(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/heartbeat", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
		}
	}
	heartbeat(true)
	srv.applyPolicySnapshot(&PolicySnapshot{
		Revision:  "r1",
		Admission: adminapi.PolicyAdmission{HoldMs: 5000},
		Wake:      adminapi.PolicyWake{NoResumeInPlace: []string{"w1"}},
	})

	if srv.wakeForRequest(ctx, "m") {
		t.Fatal("a marked sleeper must not be woken for a request")
	}
	if n := resumes.Load(); n != 0 {
		t.Fatalf("the leader resumed a marked sleeper in place (%d calls)", n)
	}

	// The request goes on to the gate and is HELD there — the wake hold —
	// until the manager has the worker serving (here: it resumed it after
	// moving its card) and its heartbeat says so.
	type result struct {
		release func()
		why     string
	}
	out := make(chan result, 1)
	go func() {
		release, why := srv.admit(ctx, "m", time.Now())
		out <- result{release, why}
	}()
	select {
	case r := <-out:
		t.Fatalf("the request was not held: %q", r.why)
	case <-time.After(150 * time.Millisecond):
	}
	heartbeat(false)
	select {
	case r := <-out:
		if r.why != "" {
			t.Fatalf("held, then refused once a worker could take it: %q", r.why)
		}
		r.release()
	case <-time.After(3 * time.Second):
		t.Fatal("the held request was never served after the worker came back")
	}
	if n := resumes.Load(); n != 0 {
		t.Fatalf("the leader resumed a marked sleeper in place (%d calls)", n)
	}

	// The mark lifted: the same sleeper is resumed by a request again.
	heartbeat(true)
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "r2"})
	if !srv.wakeForRequest(ctx, "m") || resumes.Load() != 1 {
		t.Fatalf("with no mark the request wake resumes the sleeper (%d calls)", resumes.Load())
	}
}
