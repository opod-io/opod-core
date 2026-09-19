package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// A worker answers /v1/model/load when the model is pulled AND loaded, which on
// a cold cache takes minutes. The leader sent it on the client meant for small
// control calls (60 s), so a cold pull was cut at the leader — "context deadline
// exceeded" to the operator — while the worker carried on pulling.
func TestALeaderDrivenLoadOutlastsTheControlCallBudget(t *testing.T) {
	const pull = 300 * time.Millisecond
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(pull): // the pull
			_, _ = io.WriteString(w, `{"status":"ready"}`)
		case <-r.Context().Done():
		}
	}))
	defer worker.Close()

	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "w1", Address: strings.TrimPrefix(worker.URL, "http://"),
		WorkerToken: "tok", State: store.NodeStateReady, LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	o := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
	o.HTTP = &http.Client{Timeout: pull / 10} // the control-call budget, scaled down with the pull

	entry := models.Entry{ID: "m", Source: models.SourceSpec{Type: "huggingface", Repo: "example/M"}}
	if err := o.PlaceOnNodes(ctx, entry, []string{"w1"}, false, false); err != nil {
		t.Fatalf("a load that takes longer than a control call was cut by the leader: %v", err)
	}

	// The caller still decides when to give up: its context, not a client timeout.
	short, cancel := context.WithTimeout(ctx, pull/10)
	defer cancel()
	if err := o.PlaceOnNodes(short, entry, []string{"w1"}, false, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the caller's deadline must end the load, got %v", err)
	}
}
