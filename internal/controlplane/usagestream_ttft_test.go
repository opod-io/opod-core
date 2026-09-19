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

	"github.com/opod-io/opod-sdk/adminapi"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// The leader has stored time-to-first-token per request since R15.13, and until
// opod-sdk v0.2.1 carried `ttft_ms` the usage stream could not say it — so no
// manager ever saw a TTFT. A streamed answer's value travels; a non-streamed
// one (0 = not measured) is omitted rather than reported as "0 ms".
func TestUsageStreamCarriesTTFT(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	for _, ttft := range []int{180, 0} {
		if err := st.Usage().Record(ctx, store.Usage{TS: time.Now(), Model: "m", Protocol: "chat", Outcome: "ok", TTFTMS: ttft}); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	srv.usageStream(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/usage/stream?after=0&limit=10", nil))
	var batch adminapi.UsageBatch
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil || len(batch.Events) != 2 {
		t.Fatalf("usage stream: %v %s", err, rec.Body.String())
	}
	if batch.Events[0].Data.TTFTMS != 180 {
		t.Fatalf("a streamed answer's TTFT did not reach the stream: %+v", batch.Events[0].Data)
	}
	var raw struct {
		Events []struct {
			Data map[string]json.RawMessage `json:"data"`
		} `json:"events"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	if _, sent := raw.Events[1].Data["ttft_ms"]; sent {
		t.Fatalf("an unmeasured TTFT must be omitted, not sent as 0: %s", rec.Body.String())
	}
}
