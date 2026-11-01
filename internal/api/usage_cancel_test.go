package api

// A request the caller abandons is still a request the gateway served part of,
// and its usage record says so. These tests hold the rule that a cancellation
// is recorded as one: by the time it is noticed the request's context is done,
// and nothing that records the request may depend on it.

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
	usagelog "github.com/opod-io/opod/internal/usage"
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

func usageTestStore(t testing.TB) store.Store {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// usageSink collects what a handler hands to its Usage func.
type usageSink chan usagelog.Record

func newUsageSink() usageSink             { return make(usageSink, 64) }
func (s usageSink) add(r usagelog.Record) { s <- r }

// waitForUsage waits for the first record: the handler finishes after the
// client has already gone, so the test cannot join on it.
func waitForUsage(t *testing.T, s usageSink) usagelog.Record {
	t.Helper()
	select {
	case r := <-s:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no usage record for a request the client cancelled mid-stream")
	}
	return usagelog.Record{}
}

func TestCancelledStreamStillRecordsUsage(t *testing.T) {
	sink := newUsageSink()
	h := &Handler{Engine: &cancelEngine{deltas: []string{"Hel", "lo"}}, Store: usageTestStore(t), Usage: sink.add, Default: "m"}
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

	row := waitForUsage(t, sink)
	if row.Outcome != "cancelled" {
		t.Fatalf("outcome = %q, want cancelled", row.Outcome)
	}
	if row.Model != "m" || row.Protocol != "openai" {
		t.Fatalf("row = %+v, want model m over openai", row)
	}
}

// The record itself: a context that is already done must not stop it.
func TestRecordUsageSurvivesADoneContext(t *testing.T) {
	sink := newUsageSink()
	h := &Handler{Engine: &cancelEngine{}, Store: usageTestStore(t), Usage: sink.add}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.recordUsage(ctx, "openai", "m", &engines.Usage{PromptTokens: 3, CompletionTokens: 4}, time.Millisecond, "cancelled")

	if r := waitForUsage(t, sink); r.PromptTokens != 3 || r.CompletionTokens != 4 || r.RequestID == "" {
		t.Fatalf("record = %+v, want the one recorded under a done context, with a request id", r)
	}
}

// A caller that leaves while a non-streamed answer is being gathered gets a
// cancelled row, not an "ok" with no tokens.
func TestCancelledAggregateIsNotRecordedAsOK(t *testing.T) {
	sink := newUsageSink()
	h := &Handler{Engine: &cancelEngine{deltas: []string{"Hel"}}, Store: usageTestStore(t), Usage: sink.add, Default: "m"}

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

	row := waitForUsage(t, sink)
	if row.Outcome != "cancelled" {
		t.Fatalf("outcome = %q, want cancelled", row.Outcome)
	}
}
