package httpsafe

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsPrivate(t *testing.T) {
	private := []string{
		"127.0.0.1", "::1", // loopback
		"10.1.2.3", "172.16.0.1", "192.168.0.1", // RFC-1918
		"169.254.169.254", // link-local (cloud metadata)
		"100.64.0.1",      // RFC-6598 CGNAT
		"0.0.0.0",         // unspecified
		"fc00::1",         // ULA
	}
	public := []string{
		"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111",
	}
	for _, s := range private {
		if !isPrivate(net.ParseIP(s)) {
			t.Errorf("isPrivate(%s) = false; want true", s)
		}
	}
	for _, s := range public {
		if isPrivate(net.ParseIP(s)) {
			t.Errorf("isPrivate(%s) = true; want false", s)
		}
	}
}

// A guarded client must refuse to reach a loopback test server; an unguarded
// client must succeed against the same server.
func TestNewClient_BlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	guarded := NewClient(2*time.Second, true)
	if resp, err := guarded.Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("guarded client reached a loopback server; expected it to be blocked")
	}

	open := NewClient(2*time.Second, false)
	resp, err := open.Get(srv.URL)
	if err != nil {
		t.Fatalf("unguarded client failed against loopback: %v", err)
	}
	resp.Body.Close()
}
