package agent

// The worker's client for its leader (R9.5 / R9.6): when the leader speaks TLS
// with a certificate a control plane minted for it, OPOD_LEADER_CA names the PEM
// the worker trusts — the same cert, mounted from the endpoint's Secret. No CA =
// the system roots (a public leader behind a real certificate), and plain http
// leaders are untouched.
//
// The second half is the worker's OWN certificate (R9.6): with OPOD_NODE_CERT
// and OPOD_NODE_KEY the client presents it on every call to the leader, which is
// how a worker is identified by the process it is rather than by a token it
// holds. The leader reads its node id from the certificate's SPIFFE SAN. Absent,
// nothing changes: the join token is the credential, as it has always been.

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

// NewLeaderClient builds the HTTP client the agent talks to its leader with.
func NewLeaderClient(caFile string, timeout time.Duration) (*http.Client, error) {
	return NewLeaderClientWithIdentity(caFile, "", "", timeout)
}

// NewLeaderClientWithIdentity is NewLeaderClient plus the worker's own keypair
// (R9.6). certFile and keyFile empty = the old behaviour exactly.
//
// A keypair that does not load is a hard error and not a warning: a worker told
// to present an identity, silently falling back to its token, is the one outcome
// an operator who turned mTLS on must never get — on a leader in `require` it
// would look like a join that hangs, and on one in `allow` like mTLS working
// when it is not.
func NewLeaderClientWithIdentity(caFile, certFile, keyFile string, timeout time.Duration) (*http.Client, error) {
	c := &http.Client{Timeout: timeout}
	var cfg *tls.Config
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("leader CA %s: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("leader CA %s: no certificate in the file", caFile)
		}
		cfg = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			return nil, fmt.Errorf("worker certificate: OPOD_NODE_CERT and OPOD_NODE_KEY are both needed (got cert=%q key=%q)", certFile, keyFile)
		}
		crt, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("worker certificate %s: %w", certFile, err)
		}
		if cfg == nil {
			cfg = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		cfg.Certificates = []tls.Certificate{crt}
	}
	if cfg == nil {
		return c, nil
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = cfg
	c.Transport = tr
	return c, nil
}
