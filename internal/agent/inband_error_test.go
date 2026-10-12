package agent

// The worker's in-band error chunk is a wire between two versions of core:
// what writeSSE sends after its engine fails must read back, through the
// leader's stream reader, as the same kind of failure (PLAN T8.18).

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/engines/openaicompat"
)

// relay writes evs as the worker's stream and returns what the leader reads
// from it.
func relay(t *testing.T, evs ...engines.StreamEvent) (text string, last engines.StreamEvent) {
	t.Helper()
	in := make(chan engines.StreamEvent, len(evs))
	for _, ev := range evs {
		in <- ev
	}
	close(in)
	rec := httptest.NewRecorder()
	writeSSE(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), in, "m")

	out := make(chan engines.StreamEvent, 8)
	openaicompat.ConsumeStream(context.Background(), io.NopCloser(strings.NewReader(rec.Body.String())), out)
	var b strings.Builder
	for ev := range out {
		b.WriteString(ev.Delta)
		last = ev
	}
	return b.String(), last
}

func TestAWorkersEngineBreakReadsBackAsABreak(t *testing.T) {
	text, last := relay(t,
		engines.StreamEvent{Delta: "half"},
		engines.StreamEvent{Err: engines.StreamBroken("openai", io.ErrUnexpectedEOF)})
	if text != "half" || !errors.Is(last.Err, engines.ErrStreamBroken) || last.Done {
		t.Fatalf("want the text and then a broken stream, got %q, %+v", text, last)
	}
	if !strings.Contains(last.Err.Error(), "unexpected EOF") {
		t.Fatalf("the engine's own words must survive the hop: %v", last.Err)
	}
}

func TestAWorkersEngineErrorReadsBackAsItsOwnError(t *testing.T) {
	// The engine's in-band error, already read by the worker's reader, keeps
	// its message and type across the second hop — not wrapped twice.
	_, last := relay(t, engines.StreamEvent{Err: &engines.StreamErroredError{Engine: "openai", Type: "BadRequestError", Message: "prompt is too long"}})
	var got *engines.StreamErroredError
	if !errors.As(last.Err, &got) || got.Type != "BadRequestError" || got.Message != "prompt is too long" {
		t.Fatalf("want the engine's own error as sent, got %+v", last)
	}
	if errors.Is(last.Err, engines.ErrStreamBroken) {
		t.Fatal("an engine's own error is not a transport break: it must not be replayed")
	}
	// Any other failure is an error the leader shows, never a break.
	_, last = relay(t, engines.StreamEvent{Err: errors.New("decode openai chunk: bad")})
	if !errors.Is(last.Err, engines.ErrStreamErrored) || errors.Is(last.Err, engines.ErrStreamBroken) {
		t.Fatalf("want a shown, not replayed, error, got %+v", last)
	}
}
