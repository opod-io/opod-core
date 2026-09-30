package gateway

// A door's calls to its leader, when that leader serves TLS with a certificate
// a control plane minted for it.
//
// The mirror, the pusher and the spend poller each dialled the leader with a
// bare http.Client, which trusts the system roots — and a minted certificate is
// in nobody's roots. A worker has been given the leader's certificate as
// OPOD_LEADER_CA since the listener learned TLS; a door had not, so on an
// install with per-endpoint TLS no door could read its registry. It did not
// even fail: the image's wait-for-leader loop asked with the same missing
// trust, silently, until the liveness probe killed the container (found on a
// cluster, 2026-09-29).

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// LeaderTransport is a transport that trusts exactly the certificate in caFile.
// Empty caFile = nil, nil: the default transport, as before.
func LeaderTransport(caFile string) (http.RoundTripper, error) {
	if caFile == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("leader CA %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("leader CA %s: no certificate in the file", caFile)
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return t, nil
}

// Trust makes the mirror's calls go through rt. nil leaves it as it was.
func (m *Mirror) Trust(rt http.RoundTripper) {
	if rt != nil {
		m.http.Transport = rt
	}
}

// Trust makes the pusher's calls go through rt. nil leaves it as it was.
func (p *Pusher) Trust(rt http.RoundTripper) {
	if rt != nil {
		p.http.Transport = rt
	}
}

// Trust makes the spend poller's calls go through rt. nil leaves it as it was.
func (s *Spend) Trust(rt http.RoundTripper) {
	if rt != nil {
		s.http.Transport = rt
	}
}
