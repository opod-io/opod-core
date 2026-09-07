package controlplane

import "testing"

// The waking 503 (and every other 503) is "unavailable", not the client's fault.
func TestErrorTypeFollowsStatus(t *testing.T) {
	cases := map[int]string{400: "invalid_request", 401: "authentication_error", 403: "permission_error", 404: "not_found", 429: "rate_limit_exceeded", 500: "server_error", 503: "unavailable"}
	for status, want := range cases {
		if got := errorTypeFor(status); got != want {
			t.Fatalf("%d: got %q want %q", status, got, want)
		}
	}
}
