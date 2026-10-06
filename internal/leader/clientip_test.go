package leader

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The forwarding headers rewrite RemoteAddr for the LOG, most specific first,
// and a header that does not hold an address leaves the kernel's peer alone.
func TestForwardedClientIP(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		peer    string
		want    string
	}{
		{"no header keeps the peer", nil, "10.0.0.1:5555", "10.0.0.1:5555"},
		{"x-forwarded-for leftmost", map[string]string{"X-Forwarded-For": "203.0.113.7, 10.0.0.9"}, "10.0.0.1:5555", "203.0.113.7"},
		{"x-real-ip beats xff", map[string]string{"X-Real-IP": "203.0.113.8", "X-Forwarded-For": "203.0.113.7"}, "10.0.0.1:5555", "203.0.113.8"},
		{"true-client-ip beats both", map[string]string{"True-Client-IP": "203.0.113.9", "X-Real-IP": "203.0.113.8", "X-Forwarded-For": "203.0.113.7"}, "10.0.0.1:5555", "203.0.113.9"},
		{"host:port is accepted", map[string]string{"X-Real-IP": "203.0.113.10:443"}, "10.0.0.1:5555", "203.0.113.10"},
		{"garbage keeps the peer", map[string]string{"X-Real-IP": "not-an-ip"}, "10.0.0.1:5555", "10.0.0.1:5555"},
		{"empty xff entry keeps the peer", map[string]string{"X-Forwarded-For": " , 10.0.0.9"}, "10.0.0.1:5555", "10.0.0.1:5555"},
		{"ipv6 is an address too", map[string]string{"X-Real-IP": "2001:db8::1"}, "10.0.0.1:5555", "2001:db8::1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got string
			h := forwardedClientIP(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = r.RemoteAddr
			}))
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			req.RemoteAddr = c.peer
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got != c.want {
				t.Fatalf("RemoteAddr = %q, want %q", got, c.want)
			}
		})
	}
}

// THE INVARIANT: whatever the headers claim, realRemoteAddr answers the TCP
// peer. Every trust decision in this package reads that one — the loopback
// gate on the bootstrap admin key and the audit actor — so a spoofed header
// must never reach it.
func TestRealRemoteAddrIgnoresForwardingHeaders(t *testing.T) {
	var got string
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = realRemoteAddr(r)
	})
	h := stashRemoteAddr(forwardedClientIP(inner))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = "127.0.0.1:4242"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Header.Set("True-Client-IP", "203.0.113.9")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "127.0.0.1:4242" {
		t.Fatalf("realRemoteAddr = %q, want the TCP peer 127.0.0.1:4242 — a forwarding header reached a trust decision", got)
	}
}
