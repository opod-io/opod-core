package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// countingBody counts the bytes read off the wire.
type countingBody struct {
	io.Reader
	n int
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.n += n
	return n, err
}

// finishingEngine answers with its deltas and a Done event.
type finishingEngine struct{ cancelEngine }

func (e *finishingEngine) Chat(context.Context, engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	out := make(chan engines.StreamEvent, len(e.deltas)+1)
	for _, d := range e.deltas {
		out <- engines.StreamEvent{Delta: d}
	}
	out <- engines.StreamEvent{Done: true, Reason: "stop"}
	close(out)
	return out, nil
}

// A chat POST through the whole /v1 chain — allowlist, rate limit, quota,
// handler — reads its body off the wire exactly once (PLAN T15.2): three
// middlewares used to hold three copies of it.
func TestABodyIsReadOnceThroughTheChain(t *testing.T) {
	st := usageTestStore(t)
	ctx := context.Background()
	key := &store.APIKey{ID: "k_once", Hash: "h", Name: "u", Scope: "user", UserID: "u", AllowedModels: []string{"m"}}
	if err := st.APIKeys().Create(ctx, *key); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Engine: &finishingEngine{cancelEngine{deltas: []string{"Hel", "lo"}}}, Store: st, Default: "m"}
	chain := ModelAllowMiddleware(st)(RateLimitMiddleware(NewBucketStore())(QuotaMiddleware(st)(http.HandlerFunc(h.ChatCompletions))))

	body := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("hi ", 2000) + `"}]}`
	wire := &countingBody{Reader: strings.NewReader(body)}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", wire).WithContext(auth.WithTestKey(ctx, key))
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("Hello")) {
		t.Fatalf("the answer did not come back: %s", rec.Body.String())
	}
	if wire.n != len(body) {
		t.Fatalf("read %d bytes off the wire for a %d-byte body: the body is being re-read", wire.n, len(body))
	}
}
