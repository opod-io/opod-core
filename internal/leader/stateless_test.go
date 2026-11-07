package leader

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opod-io/opod-core/internal/config"
	"github.com/opod-io/opod-core/internal/store"
)

// TestStatelessLeader (P12-1): the event stream's batches carry the boot stamp,
// the event table is a bounded ring, and the usage queue keys its records by
// the same boot.
func TestStatelessLeader(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, log, nil)

	for i := 0; i < 30; i++ {
		if err := st.EventLog().Append("test", "s", nil); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.EventLog().Trim(ctx, 10); err != nil || n != 20 {
		t.Fatalf("event trim: n=%d err=%v", n, err)
	}
	rows, _ := st.EventLog().After(ctx, 0, 100)
	if len(rows) != 10 || rows[0].ID != 21 {
		t.Fatalf("ring must keep the newest ids: %d rows, first id %d", len(rows), rows[0].ID)
	}

	rec := httptest.NewRecorder()
	srv.eventLogStream(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/events/stream?after=0&limit=5", nil))
	var body struct {
		Boot int64 `json:"boot"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Boot != bootUnix || body.Boot == 0 {
		t.Fatalf("events: boot stamp missing: %v %s", err, rec.Body.String())
	}
	if srv.usage.Boot() != bootUnix {
		t.Fatalf("usage queue boot = %d, want the stream's %d", srv.usage.Boot(), bootUnix)
	}
}
