package controlplane

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// ---- a tiny CA, because the whole row is about who signed what -------------

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "opod test node CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

// nodeCert issues a worker certificate whose SPIFFE SAN names nodeID.
func (ca *testCA) nodeCert(t *testing.T, nodeID string, serial int64) tls.Certificate {
	t.Helper()
	return ca.issue(t, serial, "spiffe://opod.test/opod/node/"+nodeID)
}

func (ca *testCA) issue(t *testing.T, serial int64, uris ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "worker"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	for _, raw := range uris {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.URIs = append(tmpl.URIs, u)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// ---- the harness -----------------------------------------------------------

// mtlsLeader starts a TLS leader with keys REQUIRED (the managed shape) and a
// node CA in its auth snapshot, and returns a client factory.
func mtlsLeader(t *testing.T, mode string, ca *testCA, revoked ...string) (*Server, *httptest.Server, func(certs ...tls.Certificate) *http.Client) {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Auth.RequireKeys = true
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, log, nil)

	snap := &AuthSnapshot{Revision: "r1", RequireKeys: true, NodeMTLS: mode, NodeCertCA: ca.pem, RevokedCerts: revoked}
	if err := srv.applyAuthSnapshot(context.Background(), snap); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewUnstartedServer(srv.routes())
	// The same shape the real listener builds (server.go): the handshake ASKS
	// for a client certificate and verifies it if one is given, and the demand
	// lives on the join routes.
	ts.TLS = &tls.Config{ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: x509.NewCertPool()}
	ts.TLS.ClientCAs.AddCert(ca.cert)
	ts.StartTLS()
	t.Cleanup(ts.Close)

	client := func(certs ...tls.Certificate) *http.Client {
		pool := x509.NewCertPool()
		pool.AddCert(ts.Certificate())
		tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: certs, MinVersion: tls.VersionTLS12}}
		return &http.Client{Transport: tr, Timeout: 10 * time.Second}
	}
	return srv, ts, client
}

func registerAs(t *testing.T, c *http.Client, url, nodeID, bearer string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(RegisterRequest{ID: nodeID, Hostname: nodeID, OS: "linux", Arch: "amd64", Address: "10.0.0.1:8081"})
	req, _ := http.NewRequest(http.MethodPost, url+"/admin/v1/nodes/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(out)
}

// ---- the properties --------------------------------------------------------

// A worker with a certificate joins with NO shared secret at all — which is the
// point of the row. Before this, the only thing that identified a worker was a
// token it held, and a copy of that token registered as that node from anywhere.
func TestAWorkerWithACertificateJoinsWithNoToken(t *testing.T) {
	ca := newTestCA(t)
	srv, ts, client := mtlsLeader(t, auth.MTLSAllow, ca)
	code, body := registerAs(t, client(ca.nodeCert(t, "n_a", 10)), ts.URL, "n_a", "")
	if code != http.StatusOK {
		t.Fatalf("register with a certificate and no token = %d: %s", code, body)
	}
	if n, _ := srv.store.Nodes().Get(context.Background(), "n_a"); n == nil {
		t.Fatal("the node was not recorded")
	}
	// And the same call without either credential is still refused: the
	// certificate is what changed, not the requirement.
	if code, _ := registerAs(t, client(), ts.URL, "n_b", ""); code != http.StatusUnauthorized {
		t.Errorf("no certificate and no token = %d, want 401", code)
	}
}

// A certificate cannot register as another node. The token path binds a KEY to
// an id; this binds the process, and the refusal names what it claimed.
func TestACertificateCannotRegisterAsAnotherNode(t *testing.T) {
	ca := newTestCA(t)
	_, ts, client := mtlsLeader(t, auth.MTLSAllow, ca)
	code, body := registerAs(t, client(ca.nodeCert(t, "n_a", 11)), ts.URL, "n_somebody_else", "")
	if code != http.StatusForbidden {
		t.Fatalf("registering as another node = %d: %s", code, body)
	}
	if !bytes.Contains([]byte(body), []byte("n_somebody_else")) || !bytes.Contains([]byte(body), []byte("n_a")) {
		t.Errorf("the refusal names neither side: %s", body)
	}
}

// A revoked certificate cannot join, and the answer says so. This is the whole
// difference between an identity and a password: the CA signed it and the leader
// refuses it anyway.
func TestARevokedCertificateCannotJoin(t *testing.T) {
	ca := newTestCA(t)
	_, ts, client := mtlsLeader(t, auth.MTLSAllow, ca, "2a") // serial 42
	code, body := registerAs(t, client(ca.nodeCert(t, "n_a", 42)), ts.URL, "n_a", "")
	if code != http.StatusForbidden {
		t.Fatalf("a revoked certificate = %d: %s", code, body)
	}
	if !bytes.Contains([]byte(body), []byte("revoked")) {
		t.Errorf("the refusal does not say revoked: %s", body)
	}
	// The same worker with a fresh certificate gets in, so what was refused is
	// the certificate and not the node.
	if code, body := registerAs(t, client(ca.nodeCert(t, "n_a", 43)), ts.URL, "n_a", ""); code != http.StatusOK {
		t.Errorf("a re-issued certificate = %d: %s", code, body)
	}
}

// `require` refuses a join that carries a perfectly good TOKEN and no
// certificate, and says which of the two is missing.
func TestRequireRefusesATokenOnlyJoin(t *testing.T) {
	ca := newTestCA(t)
	srv, ts, client := mtlsLeader(t, auth.MTLSRequire, ca)
	plain, rec, _ := auth.Generate("node", "node", "")
	if err := srv.store.APIKeys().Create(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	code, body := registerAs(t, client(), ts.URL, "n_a", plain)
	if code != http.StatusUnauthorized {
		t.Fatalf("a token-only join under require = %d: %s", code, body)
	}
	if !bytes.Contains([]byte(body), []byte("certificate")) {
		t.Errorf("the refusal does not mention a certificate: %s", body)
	}
	// The same token works the moment the snapshot goes back to allow — the
	// mode is a policy a fleet can reverse without restarting a leader.
	if err := srv.applyAuthSnapshot(context.Background(), &AuthSnapshot{
		Revision: "r2", RequireKeys: true, NodeMTLS: auth.MTLSAllow, NodeCertCA: ca.pem,
	}); err != nil {
		t.Fatal(err)
	}
	if code, body := registerAs(t, client(), ts.URL, "n_a", plain); code != http.StatusOK {
		t.Errorf("a token-only join under allow = %d: %s", code, body)
	}
}

// A certificate from another CA is not an identity. httptest's listener verifies
// if given, so the handshake itself refuses it — which is the layer that should.
func TestACertificateFromAnotherCAIsNotAnIdentity(t *testing.T) {
	ca, other := newTestCA(t), newTestCA(t)
	_, ts, client := mtlsLeader(t, auth.MTLSAllow, ca)
	body, _ := json.Marshal(RegisterRequest{ID: "n_a", Hostname: "n_a"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/v1/nodes/register", bytes.NewReader(body))
	if _, err := client(other.nodeCert(t, "n_a", 99)).Do(req); err == nil {
		t.Error("a certificate from an unknown CA completed the handshake")
	}
}

// A certificate this CA signed for something that is NOT a node carries no node
// identity, so it authenticates nothing — a leader must not turn every workload
// certificate in an operator's mesh into a worker.
func TestANonNodeCertificateAuthenticatesNothing(t *testing.T) {
	ca := newTestCA(t)
	_, ts, client := mtlsLeader(t, auth.MTLSAllow, ca)
	c := client(ca.issue(t, 77, "spiffe://opod.test/ns/default/sa/some-app"))
	if code, body := registerAs(t, c, ts.URL, "n_a", ""); code != http.StatusUnauthorized {
		t.Errorf("a non-node certificate registered a node: %d %s", code, body)
	}
}

// The MANAGER's own calls are untouched by `require`. They present an admin
// token and no certificate, and a require that reached them would lock the
// control plane out of the leader it had just configured.
func TestRequireDoesNotReachTheManagersCalls(t *testing.T) {
	ca := newTestCA(t)
	srv, ts, client := mtlsLeader(t, auth.MTLSRequire, ca)
	plain, rec, _ := auth.Generate("admin", "admin", "")
	if err := srv.store.APIKeys().Create(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	res, err := client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(res.Body)
		t.Errorf("the manager's own call under require = %d: %s", res.StatusCode, out)
	}
}

// A snapshot asking for mTLS with no CA, or with a CA that does not parse, stays
// OFF. The alternative is a leader in `require` with nothing to verify against,
// which refuses every worker in the endpoint for a reason nobody is looking at.
func TestMTLSWithoutAUsableCAStaysOff(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	for _, c := range []struct{ name, mode, ca string }{
		{"require with no CA", auth.MTLSRequire, ""},
		{"allow with no CA", auth.MTLSAllow, ""},
		{"require with junk", auth.MTLSRequire, "-----BEGIN CERTIFICATE-----\nnot base64\n-----END CERTIFICATE-----"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := srv.applyAuthSnapshot(context.Background(), &AuthSnapshot{Revision: "x", NodeMTLS: c.mode, NodeCertCA: c.ca}); err != nil {
				t.Fatal(err)
			}
			if got := srv.nodeMTLS(); got.Mode != auth.MTLSOff {
				t.Errorf("mode = %q, want off", got.Mode)
			}
		})
	}
}

// With the snapshot silent, nothing about any of this is reachable: no
// certificate is asked for, and a token joins as it always has.
func TestWithNoSnapshotPolicyTheJoinIsUnchanged(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Auth.RequireKeys = true
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if got := srv.nodeMTLS().Mode; got != auth.MTLSOff {
		t.Fatalf("a leader with no snapshot is in mode %q", got)
	}
	if srv.nodeMTLS().ClientCAs() != nil {
		t.Error("a leader with no snapshot would ask for a client certificate")
	}
	plain, rec, _ := auth.Generate("node", "node", "")
	if err := srv.store.APIKeys().Create(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.routes()) // plain http, as an unmanaged leader serves
	defer ts.Close()
	if code, body := registerAs(t, ts.Client(), ts.URL, "n_a", plain); code != http.StatusOK {
		t.Errorf("a plain token join = %d: %s", code, body)
	}
}
