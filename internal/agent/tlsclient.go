package agent

// The worker's client for its leader (R9.5 / R9.6 first step): when the
// leader speaks TLS with a certificate a control plane minted for it,
// OPOD_LEADER_CA names the PEM the worker trusts — the same cert, mounted
// from the endpoint's Secret. No CA = the system roots (a public leader
// behind a real certificate), and plain http leaders are untouched.

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
	c := &http.Client{Timeout: timeout}
	if caFile == "" {
		return c, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("leader CA %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("leader CA %s: no certificate in the file", caFile)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	c.Transport = tr
	return c, nil
}
