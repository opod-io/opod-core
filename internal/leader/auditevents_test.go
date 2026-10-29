package leader

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/auth"
)

// TestAdminCallsRideTheEventStream (P12-3): a state-changing /admin/v1 call
// becomes an admin.call lifecycle event with actor + status; reads and the
// worker heartbeat do not.
func TestAdminCallsRideTheEventStream(t *testing.T) {
	srv, ts := newPolicyTestServer(t, nil)
	do := func(method, path, body string) int {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	// keys optional at the gateway never means an open admin surface
	if c := do(http.MethodGet, "/admin/v1/nodes", ""); c != http.StatusUnauthorized {
		t.Fatalf("/admin/v1 without a key must be 401 even with requireKeys=false, got %d", c)
	}
	adminPlain, adminRec, _ := auth.Generate("cp-admin", "admin", "")
	if err := srv.store.APIKeys().Create(context.Background(), adminRec); err != nil {
		t.Fatal(err)
	}
	do = func(method, path, body string) int {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if strings.HasPrefix(path, "/admin/") {
			req.Header.Set("Authorization", "Bearer "+adminPlain)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := do(http.MethodGet, "/admin/v1/nodes", ""); c != http.StatusOK {
		t.Fatalf("admin key must open /admin/v1: %d", c)
	}
	do(http.MethodPost, "/admin/v1/nodes/heartbeat", `{"id":"n-none"}`)
	do(http.MethodDelete, "/admin/v1/models/nothing-here", "")

	rows, err := srv.store.EventLog().After(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var adminCalls int
	for _, e := range rows {
		switch e.Type {
		case "admin.call":
			adminCalls++
			if strings.HasPrefix(e.Subject, "GET ") || strings.Contains(e.Subject, "heartbeat") {
				t.Fatalf("reads/heartbeats must not be audit events: %+v", e)
			}
			if e.Subject != "DELETE /admin/v1/models/nothing-here" || e.Data["actor"] != "cp-admin" || e.Data["status"] == nil {
				t.Fatalf("admin.call shape: %+v", e)
			}
		}
	}
	if adminCalls != 1 {
		t.Fatalf("want 1 admin.call, got %d in %d events", adminCalls, len(rows))
	}
}
