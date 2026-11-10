package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod-core/internal/config"
)

// T24.102: a leader whose certificate its manager issued names the leader's
// service, never localhost. This host reads the very file the leader serves,
// so the local client accepts that certificate, byte for byte, whatever it
// names — and any other certificate is "a leader answered", never "no leader
// answering", which would send a command to its offline path.
func TestTheLocalClientPinsThisHostsLeaderCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	writeCert := func(name string, der []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	u, _ := url.Parse(srv.URL)
	viaLocalhost := "https://localhost:" + u.Port() // the name the certificate does not carry
	get := func(certFile string) error {
		cfg := &config.Config{TLSCert: certFile, TLSKey: "key.pem"}
		resp, err := localLeaderClient(cfg, 5*time.Second).Get(viaLocalhost + "/admin/v1/shards")
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}

	ours := writeCert("ours.pem", srv.Certificate().Raw)
	if err := get(ours); err != nil {
		t.Fatalf("this host's own leaf, dialled as localhost, must be accepted: %v", err)
	}

	stranger := writeCert("stranger.pem", selfSigned(t))
	err := get(stranger)
	var notOurs *notOurCertError
	if !errors.As(err, &notOurs) {
		t.Fatalf("a certificate that is not the file's must be refused by name, got %v", err)
	}

	// Through adminCallT the refusal is an answer, not errUnreachable.
	t.Setenv("OPOD_TOKEN", "k")
	t.Setenv("OPOD_BASE_URL", "")
	cfg := &config.Config{TLSCert: stranger, TLSKey: "key.pem", Listen: "localhost:" + u.Port()}
	_, callErr := adminCallT(context.Background(), cfg, "GET", "/admin/v1/shards", nil, 5*time.Second)
	if callErr == nil || errors.Is(callErr, errUnreachable) || localLeaderDown(callErr) {
		t.Fatalf("a leader that answered with another certificate is not 'no leader answering': %v", callErr)
	}
	if !strings.Contains(callErr.Error(), "answered") || !strings.Contains(callErr.Error(), stranger) {
		t.Fatalf("the message must say a leader answered and name the file: %v", callErr)
	}
}

// selfSigned is a certificate that is not the test server's.
func selfSigned(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "leader"}, DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}
