package auth

import (
	"net/http"
	"testing"
	"time"
)

func TestHMAC_RoundTrip(t *testing.T) {
	const token = "sk-orc-secret"
	const nodeID = "n-abc"

	req, _ := http.NewRequest(http.MethodPost, "http://example/v1/process/start", nil)
	SignRequest(req, nodeID, token)

	got, err := VerifyRequest(req, func(id string) (string, error) {
		if id != nodeID {
			t.Errorf("lookup called with %q, want %q", id, nodeID)
		}
		return token, nil
	})
	if err != nil {
		t.Fatalf("VerifyRequest: %v", err)
	}
	if got != nodeID {
		t.Errorf("nodeID = %q, want %q", got, nodeID)
	}
}

func TestHMAC_RejectsTampering(t *testing.T) {
	const token = "sk-orc-secret"

	req, _ := http.NewRequest(http.MethodPost, "http://example/v1/process/start", nil)
	SignRequest(req, "n1", token)

	// Tamper with the method by reusing the signature on a different request.
	req2, _ := http.NewRequest(http.MethodDelete, "http://example/v1/process/start", nil)
	req2.Header.Set(HMACHeader, req.Header.Get(HMACHeader))

	if _, err := VerifyRequest(req2, func(string) (string, error) { return token, nil }); err == nil {
		t.Error("expected verification failure when method changes")
	}

	// Tamper with the path the same way.
	req3, _ := http.NewRequest(http.MethodPost, "http://example/v1/admin/wipe", nil)
	req3.Header.Set(HMACHeader, req.Header.Get(HMACHeader))
	if _, err := VerifyRequest(req3, func(string) (string, error) { return token, nil }); err == nil {
		t.Error("expected verification failure when path changes")
	}
}

func TestHMAC_RejectsWrongSecret(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://example/x", nil)
	SignRequest(req, "n1", "real-secret")

	_, err := VerifyRequest(req, func(string) (string, error) { return "different-secret", nil })
	if err == nil {
		t.Error("expected mismatch when verifier holds the wrong secret")
	}
}

func TestHMAC_RejectsExpiredTimestamp(t *testing.T) {
	const token = "secret"
	now := time.Now()

	req, _ := http.NewRequest(http.MethodGet, "http://example/x", nil)
	SignRequest(req, "n1", token)

	// Pretend we're verifying 10 minutes later.
	_, err := verifyRequest(req, func(string) (string, error) { return token, nil },
		now.Add(10*time.Minute), defaultMaxSkew)
	if err == nil {
		t.Error("expected ts-skew rejection past the 5-min window")
	}
}

func TestHMAC_RejectsMissingHeader(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://example/x", nil)
	if _, err := VerifyRequest(req, func(string) (string, error) { return "x", nil }); err == nil {
		t.Error("expected error when X-Opod-Auth header is missing")
	}
}

func TestHMAC_UnknownNodeRejected(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://example/x", nil)
	SignRequest(req, "n-unknown", "secret")
	_, err := VerifyRequest(req, func(string) (string, error) {
		return "", nil // lookup returns "" → unknown
	})
	if err == nil {
		t.Error("expected error when lookup returns empty token")
	}
}
