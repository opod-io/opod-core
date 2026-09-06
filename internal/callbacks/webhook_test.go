package callbacks

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhook_DeliversAndSigns(t *testing.T) {
	var got struct {
		mu    sync.Mutex
		body  []byte
		sig   string
		count int32
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.mu.Lock()
		got.body = b
		got.sig = r.Header.Get("X-Opod-Signature")
		got.mu.Unlock()
		atomic.AddInt32(&got.count, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	w := NewWebhook(WebhookConfig{
		ID:     "test-hook",
		URL:    srv.URL,
		Secret: "topsecret",
		Events: []string{"usage", "test"},
	}, slog.Default())
	defer w.Close(context.Background())

	w.Send(context.Background(), Event{
		Kind:      "usage",
		Timestamp: time.Now(),
		Payload:   map[string]any{"model": "qwen3-14b", "prompt_tokens": float64(10)},
	})

	// Allow the worker time to deliver. 200ms is plenty against the
	// in-process httptest server.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&got.count) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(&got.count) == 0 {
		t.Fatal("webhook never received the event")
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	var decoded Event
	if err := json.Unmarshal(got.body, &decoded); err != nil {
		t.Fatalf("body not valid event JSON: %v", err)
	}
	if decoded.Kind != "usage" {
		t.Errorf("kind = %q, want usage", decoded.Kind)
	}
	if !VerifySignature("topsecret", got.body, got.sig) {
		t.Errorf("signature did not verify: %q", got.sig)
	}
	if VerifySignature("wrong-key", got.body, got.sig) {
		t.Error("signature verified with the wrong secret — HMAC is broken")
	}
}

// TestWebhook_QueueFullDropsRatherThanBlocks proves the hot path
// can't be stalled by a slow receiver. We use a controlled "park
// the worker" channel — easier to clean up than an actually-blocking
// httptest handler.
func TestWebhook_QueueFullDropsRatherThanBlocks(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	// NOTE: no `defer srv.Close()` here. A defer runs when the test function
	// returns, i.e. BEFORE t.Cleanup — and httptest.Server.Close blocks until
	// every in-flight request completes, while the handler above is parked on
	// <-release, which only closes in Cleanup. That ordering deadlocked the
	// whole package for the 10-minute test timeout on CI. Release first, then
	// close the webhook, then the server.

	w := NewWebhook(WebhookConfig{
		ID:      "slow",
		URL:     srv.URL,
		QueueSz: 2, // tight on purpose
	}, slog.Default())
	t.Cleanup(func() {
		close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = w.Close(ctx)
		srv.Close()
	})

	// Push more events than the queue can hold. None of these calls
	// should block — Send is non-blocking by contract.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			w.Send(context.Background(), Event{Kind: "usage", Payload: map[string]any{"i": i}})
		}
		close(done)
	}()
	select {
	case <-done:
		// good — Send returned for all 50 iterations
	case <-time.After(time.Second):
		t.Fatal("Send blocked the caller — hot path is not safe")
	}
}

// TestSubscribesEventFilter — when events is configured, only the
// listed kinds subscribe.
func TestSubscribesEventFilter(t *testing.T) {
	w := &Webhook{events: map[string]bool{"usage": true}, url: "https://x"}
	if !w.Subscribes("usage") {
		t.Error("usage should subscribe")
	}
	if w.Subscribes("audit") {
		t.Error("audit should NOT subscribe when events=[usage]")
	}
}

func TestSubscribes_AllByDefault(t *testing.T) {
	w := &Webhook{url: "https://x"} // events == nil
	for _, k := range []string{"usage", "audit", "fallback", "test"} {
		if !w.Subscribes(k) {
			t.Errorf("default sink should subscribe to %q", k)
		}
	}
}
