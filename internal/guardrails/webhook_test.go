package guardrails

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebhook_Allow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"action":"allow"}`))
	}))
	defer srv.Close()
	w := NewWebhook(WebhookConfig{ID: "test", Mode: ModePre, URL: srv.URL})
	act, err := w.Check(context.Background(), []byte(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if act.Kind != "allow" {
		t.Errorf("Kind = %q, want allow", act.Kind)
	}
}

func TestWebhook_Block(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"action":"block","reason":"PII detected"}`))
	}))
	defer srv.Close()
	w := NewWebhook(WebhookConfig{ID: "test", Mode: ModePre, URL: srv.URL})
	act, _ := w.Check(context.Background(), []byte(`{"messages":[]}`))
	if act.Kind != "block" {
		t.Errorf("Kind = %q, want block", act.Kind)
	}
	if !strings.Contains(act.Reason, "PII") {
		t.Errorf("Reason = %q, want PII mention", act.Reason)
	}
}

func TestWebhook_Rewrite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"action":"rewrite","replacement":{"messages":[{"role":"user","content":"[REDACTED]"}]}}`))
	}))
	defer srv.Close()
	w := NewWebhook(WebhookConfig{ID: "test", Mode: ModePre, URL: srv.URL})
	act, _ := w.Check(context.Background(), []byte(`{"messages":[{"role":"user","content":"My SSN is 123-45-6789"}]}`))
	if act.Kind != "rewrite" {
		t.Errorf("Kind = %q, want rewrite", act.Kind)
	}
	if !strings.Contains(string(act.NewBody), "[REDACTED]") {
		t.Errorf("NewBody missing replacement: %s", string(act.NewBody))
	}
}

func TestWebhook_FailOpen(t *testing.T) {
	// Bad URL → request errors. With FailOpen=true the guardrail
	// returns Allow rather than blocking the call.
	w := NewWebhook(WebhookConfig{
		ID: "test", Mode: ModePre,
		URL:      "http://127.0.0.1:1", // refused
		FailOpen: true,
		Timeout:  200 * time.Millisecond,
	})
	act, _ := w.Check(context.Background(), []byte(`{}`))
	if act.Kind != "allow" {
		t.Errorf("fail-open Kind = %q, want allow", act.Kind)
	}
}

func TestWebhook_FailClosed(t *testing.T) {
	w := NewWebhook(WebhookConfig{
		ID: "test", Mode: ModePre,
		URL:      "http://127.0.0.1:1",
		FailOpen: false,
		Timeout:  200 * time.Millisecond,
	})
	act, _ := w.Check(context.Background(), []byte(`{}`))
	if act.Kind != "block" {
		t.Errorf("fail-closed Kind = %q, want block", act.Kind)
	}
}

func TestChain_IsEmpty(t *testing.T) {
	if !(*Chain)(nil).IsEmpty() {
		t.Error("nil chain should be empty")
	}
	c := NewChain()
	if !c.IsEmpty() {
		t.Error("empty chain should be empty")
	}
	w := NewWebhook(WebhookConfig{Mode: ModePre, URL: "http://x"})
	c = NewChain(w)
	if c.IsEmpty() {
		t.Error("non-empty chain should not be empty")
	}
}
