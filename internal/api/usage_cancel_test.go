package api

// A request the caller abandons is still a request the gateway served part of,
// and the usage stream is the record of it. These tests hold the rule that the
// usage row never rides the request's own context: by the time a cancellation
// is noticed that context is done, and a write made with it is refused.

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// cancelEngine streams the way every real producer does: it sends what it has,
// stops sending once the request context is done, and closes the channel
// without a final event.
type cancelEngine struct {
	deltas []string
}

func (e *cancelEngine) Name() string                           { return "fake" }
func (e *cancelEngine) Endpoint() string                       { return "" }
func (e *cancelEngine) Health(context.Context) error           { return nil }
func (e *cancelEngine) List(context.Context) ([]string, error) { return nil, nil }
func (e *cancelEngine) Delete(context.Context, string) error   { return nil }
func (e *cancelEngine) Unload(context.Context, string) error   { return nil }
func (e *cancelEngine) Pull(context.Context, string, func(string, int64, int64)) error {
	return nil
}

func (e *cancelEngine) Chat(ctx context.Context, _ engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	out := make(chan engines.StreamEvent)
	go func() {
		defer close(out)
		for _, d := range e.deltas {
			select {
			case out <- engines.StreamEvent{Delta: d}:
			case <-ctx.Done():
				return
			}
		}
		<-ctx.Done() // the answer is not finished; only the caller leaving ends it
	}()
	return out, nil
}

func usageTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// waitForUsage polls for the first usage row: the handler finishes after the
// client has already gone, so the test cannot join on it.
func waitForUsage(t *testing.T, st store.Store) store.Usage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := st.Usage().Recent(context.Background(), 10)
		if err != nil {
			t.Fatalf("Recent: %v", err)
		}
		if len(rows) > 0 {
			return rows[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no usage row was written for a request the client cancelled mid-stream")
	return store.Usage{}
}

func TestCancelledStreamStillRecordsUsage(t *testing.T) {
	st := usageTestStore(t)
	h := &Handler{Engine: &cancelEngine{deltas: []string{"Hel", "lo"}}, Store: st, Default: "m"}
	srv := httptest.NewServer(http.HandlerFunc(h.ChatCompletions))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	// Read until the first content delta, then hang up mid-answer.
	sc := bufio.NewScanner(resp.Body)
	sawContent := false
	for sc.Scan() {
		if strings.Contains(sc.Text(), `"content":"Hel"`) {
			sawContent = true
			break
		}
	}
	if !sawContent {
		t.Fatal("the stream never produced a content delta to cancel after")
	}
	cancel()

	row := waitForUsage(t, st)
	if row.Outcome != "cancelled" {
		t.Fatalf("outcome = %q, want cancelled", row.Outcome)
	}
	if row.Model != "m" || row.Protocol != "openai" {
		t.Fatalf("row = %+v, want model m over openai", row)
	}
}

// The write itself: a context that is already done must not stop the row.
func TestRecordUsageSurvivesADoneContext(t *testing.T) {
	st := usageTestStore(t)
	h := &Handler{Engine: &cancelEngine{}, Store: st}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.recordUsage(ctx, "openai", "m", &engines.Usage{PromptTokens: 3, CompletionTokens: 4}, time.Millisecond, "cancelled")

	rows, err := st.Usage().Recent(context.Background(), 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(rows) != 1 || rows[0].PromptTokens != 3 || rows[0].CompletionTokens != 4 {
		t.Fatalf("rows = %+v, want the one row written under a done context", rows)
	}
}

// A caller that leaves while a non-streamed answer is being gathered gets a
// cancelled row, not an "ok" with no tokens.
func TestCancelledAggregateIsNotRecordedAsOK(t *testing.T) {
	st := usageTestStore(t)
	h := &Handler{Engine: &cancelEngine{deltas: []string{"Hel"}}, Store: st, Default: "m"}

	ctx, cancel := context.WithCancel(context.Background())
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ChatCompletions(httptest.NewRecorder(), req)
	}()
	time.Sleep(50 * time.Millisecond) // let the handler reach the stream
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not return after its caller left")
	}

	row := waitForUsage(t, st)
	if row.Outcome != "cancelled" {
		t.Fatalf("outcome = %q, want cancelled", row.Outcome)
	}
}
