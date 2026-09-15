package controlplane

// R9.5 / R9.6 first step: with OPOD_TLS_CERT/KEY the leader's one listener
// speaks TLS, and a worker trusts it through OPOD_LEADER_CA.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

func selfSigned(t *testing.T, dir string) (crt, key string) {
	t.Helper()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "opod-ep-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"opod-ep-test"}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	kb, _ := x509.MarshalECPrivateKey(priv)
	crt, key = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	_ = os.WriteFile(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return crt, key
}

func TestLeaderListenerSpeaksTLSWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	crt, key := selfSigned(t, dir)
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.TLSCert, cfg.TLSKey = crt, key
	cfg.DataDir = dir
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if srv.Addr() == "" {
		t.Fatal("the server never bound")
	}
	if !srv.TLS() {
		t.Fatal("TLS() reports the configured listener")
	}
	// A worker's client with the CA reaches /healthz over https.
	trusting, err := agent.NewLeaderClient(crt, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := trusting.Get("https://" + srv.Addr() + "/healthz")
	if err != nil {
		t.Fatalf("https with the leader CA: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz over TLS: %s", resp.Status)
	}
	// Without the CA the self-signed certificate is refused — the worker
	// never trusts a leader it was not handed the certificate of.
	plain, _ := agent.NewLeaderClient("", 5*time.Second)
	if _, err := plain.Get("https://" + srv.Addr() + "/healthz"); err == nil {
		t.Fatal("a client without the CA must refuse the self-signed leader")
	}
	// And plain http on a TLS listener is not a leader answer.
	if resp, err := plain.Get("http://" + srv.Addr() + "/healthz"); err == nil && resp.StatusCode == 200 {
		resp.Body.Close()
		t.Fatal("the TLS listener answered plain http with 200")
	}
	if _, err := agent.NewLeaderClient(filepath.Join(dir, "missing.pem"), time.Second); err == nil {
		t.Fatal("a missing CA file is an error, not a silent system-roots client")
	}
}
