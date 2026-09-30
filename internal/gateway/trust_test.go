package gateway

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A door reads its leader's registry over TLS only when it was given the
// leader's certificate — and with it, it does. Found on a cluster: with
// per-endpoint TLS no door had ever been given one.
func TestADoorTrustsItsLeadersCertificate(t *testing.T) {
	leader := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer leader.Close()
	ca := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leader.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	get := func(rt http.RoundTripper) error {
		c := &http.Client{}
		if rt != nil {
			c.Transport = rt
		}
		req, _ := http.NewRequestWithContext(context.Background(), "GET", leader.URL+"/healthz", nil)
		resp, err := c.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := get(nil); err == nil {
		t.Fatal("a minted certificate is in nobody's roots: the bare client must refuse it")
	}
	rt, err := LeaderTransport(ca)
	if err != nil {
		t.Fatal(err)
	}
	if err := get(rt); err != nil {
		t.Fatalf("with the leader's certificate the door reaches it: %v", err)
	}
	// Each of the three loops takes the trust.
	m, p, s := NewMirror(leader.URL, "t", nil), NewPusher(leader.URL, "t", "d"), NewSpend(leader.URL, "t")
	m.Trust(rt)
	p.Trust(rt)
	s.Trust(rt)
	for name, c := range map[string]*http.Client{"mirror": m.http, "pusher": p.http, "spend": s.http} {
		if c.Transport != rt {
			t.Errorf("%s does not use the leader's trust", name)
		}
	}
	// No file named: nothing changes. A file that holds no certificate: refused.
	if rt, err := LeaderTransport(""); rt != nil || err != nil {
		t.Errorf("no CA = the default transport: %v %v", rt, err)
	}
	bad := filepath.Join(t.TempDir(), "bad.crt")
	_ = os.WriteFile(bad, []byte("not a certificate"), 0o600)
	if _, err := LeaderTransport(bad); err == nil {
		t.Error("a CA file with no certificate in it must be refused")
	}
}
