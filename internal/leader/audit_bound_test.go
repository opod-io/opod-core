package leader

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opod-io/opod/internal/store"
)

// T25.9: the audit table holds actions, not traffic. The worker heartbeat (one
// every five seconds per worker) and every read used to write a row, and a
// managed leader's store is in memory, so the table grew for as long as the
// leader lived. Heartbeats and reads write nothing; a change writes one row;
// and the table is a ring like usage and events.
func TestTheAuditTableHoldsActionsNotTraffic(t *testing.T) {
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := s.auditMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	call := func(method, path string) {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
	}
	ctx := context.Background()
	count := func() int {
		rows, err := st.Audit().Recent(ctx, 1_000_000)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	for range 500 {
		call(http.MethodPost, "/admin/v1/nodes/heartbeat")
		call(http.MethodGet, "/admin/v1/nodes")
	}
	if n := count(); n != 0 {
		t.Fatalf("500 heartbeats and 500 reads wrote %d audit rows, want 0", n)
	}
	call(http.MethodPost, "/admin/v1/models/m1/load")
	if n := count(); n != 1 {
		t.Fatalf("one change wrote %d audit rows, want 1", n)
	}
	for range 30 {
		call(http.MethodDelete, "/admin/v1/keys/k1")
	}
	if _, err := st.Audit().Trim(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 10 {
		t.Fatalf("after a trim to 10 the table holds %d rows", n)
	}
}
