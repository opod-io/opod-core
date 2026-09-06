package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// TestStatelessLeader (P12-1): the stream batches carry the boot stamp, and the
// usage + event tables are bounded rings.
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
		if err := st.Usage().Record(ctx, store.Usage{TS: time.Now(), Model: "m", Protocol: "chat", Outcome: "ok"}); err != nil {
			t.Fatal(err)
		}
		if err := st.EventLog().Append("test", "s", nil); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.Usage().Trim(ctx, 10); err != nil || n != 20 {
		t.Fatalf("usage trim: n=%d err=%v", n, err)
	}
	if n, err := st.EventLog().Trim(ctx, 10); err != nil || n != 20 {
		t.Fatalf("event trim: n=%d err=%v", n, err)
	}
	rows, _ := st.EventLog().After(ctx, 0, 100)
	if len(rows) != 10 || rows[0].ID != 21 {
		t.Fatalf("ring must keep the newest ids: %d rows, first id %d", len(rows), rows[0].ID)
	}

	for _, path := range []string{"/admin/v1/usage/stream", "/admin/v1/events/stream"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path+"?after=0&limit=5", nil)
		if path == "/admin/v1/usage/stream" {
			srv.usageStream(rec, req)
		} else {
			srv.eventLogStream(rec, req)
		}
		var body struct {
			Boot int64 `json:"boot"`
			Next int64 `json:"next"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Boot != bootUnix || body.Boot == 0 {
			t.Fatalf("%s: boot stamp missing: %v %s", path, err, rec.Body.String())
		}
	}
}
