package agent

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

	"github.com/opod-io/opod-core/internal/engines"
)

// chatGate is an engine whose Chat answers only when the test lets it, with
// the error the test chose.
type chatGate struct {
	engines.Engine
	release chan error
	calls   chan string
}

func (c *chatGate) Name() string { return "trtllm" }
func (c *chatGate) Chat(ctx context.Context, req engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	c.calls <- req.Model
	err := <-c.release
	if err != nil {
		return nil, err
	}
	ch := make(chan engines.StreamEvent, 1)
	ch <- engines.StreamEvent{Done: true}
	close(ch)
	return ch, nil
}

func newGate() *chatGate {
	return &chatGate{release: make(chan error, 4), calls: make(chan string, 4)}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// PLAN T32.60: TensorRT-LLM listed its model ~70 s before its first
// generation returned. The models are reported only once one has.
func TestModelsAreReportedOnlyAfterTheFirstGenerationReturns(t *testing.T) {
	eng := newGate()
	w := NewWarmup(eng, quiet())
	if w.Observe([]string{"Qwen/Qwen2.5-0.5B-Instruct"}) {
		t.Fatal("a listed model was reported before any generation")
	}
	if got := <-eng.calls; got != "Qwen/Qwen2.5-0.5B-Instruct" {
		t.Fatalf("warm-up asked for %q, want the listed model", got)
	}
	if w.Observe([]string{"Qwen/Qwen2.5-0.5B-Instruct"}) {
		t.Fatal("reported while the warm-up is still in flight")
	}
	if ok, why := w.Ready(); ok || !strings.Contains(why, "warming up") {
		t.Fatalf("readyz before the warm-up returned: ok=%v why=%q", ok, why)
	}
	eng.release <- nil
	select {
	case <-w.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("no signal when the warm-up returned")
	}
	if !w.Observe([]string{"Qwen/Qwen2.5-0.5B-Instruct"}) {
		t.Fatal("not reported after the warm-up returned")
	}
	select {
	case m := <-eng.calls:
		t.Fatalf("a second warm-up was sent (%q) for an engine already warm", m)
	default:
	}
}

// An engine that answers with an error has still answered: an embeddings-only
// model refuses a chat, and that refusal went through the request path.
func TestAnEngineThatRefusesTheWarmupHasAnswered(t *testing.T) {
	eng := newGate()
	w := NewWarmup(eng, quiet())
	w.Observe([]string{"embed-model"})
	<-eng.calls
	eng.release <- engines.Upstream("vllm", "POST /v1/chat/completions", http.StatusBadRequest, []byte(`{"error":"not a chat model"}`))
	<-w.Done()
	if !w.Observe([]string{"embed-model"}) {
		t.Fatal("an engine that answered 400 was held as not ready")
	}
}

// No answer at all is tried again, and Reset (the engine went away) makes the
// next engine warm up from the start.
func TestNoAnswerIsTriedAgainAndANewEngineWarmsAgain(t *testing.T) {
	eng := newGate()
	w := NewWarmup(eng, quiet())
	w.Observe([]string{"m"})
	<-eng.calls
	eng.release <- engines.Unreachable("trtllm", "http://127.0.0.1:8000", errors.New("connection refused"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, why := w.Ready(); strings.Contains(why, "last try") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the failed warm-up left no reason")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if w.Observe([]string{"m"}) {
		t.Fatal("reported although the warm-up got no answer")
	}
	<-eng.calls // the retry
	eng.release <- nil
	<-w.Done()
	if !w.Observe([]string{"m"}) {
		t.Fatal("not reported after the retry answered")
	}
	w.Reset()
	if w.Observe([]string{"m"}) {
		t.Fatal("after Reset the next engine must warm up again")
	}
	<-eng.calls
	eng.release <- nil
	<-w.Done()
}

// /readyz says not ready, and why, until the warm-up returns.
func TestReadyzWaitsForTheWarmup(t *testing.T) {
	eng := newGate()
	w := NewWarmup(eng, quiet())
	join := &JoinState{}
	join.note("register", http.StatusOK, nil)
	srv := &Server{Engine: healthyEngine{eng}, Join: join, Warm: w}
	get := func() (int, string) {
		rec := httptest.NewRecorder()
		srv.readyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec.Code, rec.Body.String()
	}
	w.Observe([]string{"m"})
	<-eng.calls
	if code, body := get(); code != http.StatusServiceUnavailable || !strings.Contains(body, "warming up") {
		t.Fatalf("before the warm-up: %d %q", code, body)
	}
	eng.release <- nil
	<-w.Done()
	if code, body := get(); code != http.StatusOK {
		t.Fatalf("after the warm-up: %d %q", code, body)
	}
}

type healthyEngine struct{ *chatGate }

func (healthyEngine) Health(context.Context) error { return nil }

// listAndChat lists one model and answers chats through a gate.
type listAndChat struct {
	*chatGate
}

func (listAndChat) List(context.Context) ([]string, error) { return []string{"m"}, nil }

// The wiring: the heartbeat itself sends "nothing loaded" until the warm-up
// returns, then the model.
func TestTheHeartbeatHoldsTheModelUntilTheWarmupReturns(t *testing.T) {
	gate := newGate()
	eng := listAndChat{gate}
	w := NewWarmup(eng, quiet())
	beat := func(a *Agent) error { a.Warm = w; _, err := a.Heartbeat(context.Background()); return err }
	if body := capturedPost(t, beat, eng, &Aliases{}, Capabilities{}); string(body["loaded_models"]) != "[]" {
		t.Fatalf("before the warm-up: want loaded_models [], got %s", body["loaded_models"])
	}
	<-gate.calls
	gate.release <- nil
	<-w.Done()
	if body := capturedPost(t, beat, eng, &Aliases{}, Capabilities{}); string(body["loaded_models"]) != `["m"]` {
		t.Fatalf("after the warm-up: want loaded_models [\"m\"], got %s", body["loaded_models"])
	}
}
