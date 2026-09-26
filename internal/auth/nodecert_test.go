package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// certWith builds a leaf certificate with the given URI SANs and serial. It is
// never verified here — these tests are about what a leader READS off a
// certificate, and the verifying is the TLS stack's job.
func certWith(t *testing.T, serial *big.Int, uris ...string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "n_from_the_cn"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"n_from_dns"},
	}
	for _, raw := range uris {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.URIs = append(tmpl.URIs, u)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A node id is read from a SPIFFE URI SAN and from nothing else.
//
// The CN and the DNS name are deliberately populated with plausible node ids in
// every case below: if either were ever read, these tests would pass for the
// wrong reason and a leader would accept any certificate its CA had signed for
// any purpose as a worker identity.
func TestNodeIDComesOnlyFromASpiffeSAN(t *testing.T) {
	cases := []struct {
		name string
		uris []string
		want string
	}{
		{"a node id", []string{"spiffe://opod.local/opod/node/n_worker_3"}, "n_worker_3"},
		{"beside another spiffe id", []string{"spiffe://opod.local/ns/default/sa/x", "spiffe://opod.local/opod/node/n_a"}, "n_a"},
		{"no SANs at all", nil, ""},
		{"a spiffe id that is not a node", []string{"spiffe://opod.local/ns/default/sa/worker"}, ""},
		{"the node path not at the start", []string{"spiffe://opod.local/team/opod/node/n_a"}, ""},
		{"a deeper path under node", []string{"spiffe://opod.local/opod/node/n_a/part/1"}, ""},
		{"an empty id", []string{"spiffe://opod.local/opod/node/"}, ""},
		{"https, not spiffe", []string{"https://opod.local/opod/node/n_a"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NodeIDFromCert(certWith(t, big.NewInt(1), c.uris...)); got != c.want {
				t.Errorf("NodeIDFromCert = %q, want %q", got, c.want)
			}
		})
	}
	if got := NodeIDFromCert(nil); got != "" {
		t.Errorf("a nil certificate = %q", got)
	}
}

// A serial as three tools print it is one serial. An operator copies it out of
// `openssl x509 -serial` (uppercase), out of a browser (colon-separated) or out
// of our own log line, and a revocation must take effect in all three cases.
func TestASerialIsComparableHoweverItWasPrinted(t *testing.T) {
	// A real CA's serial does not fit in an int64; this one does not either.
	big1, ok := new(big.Int).SetString("7f3a1b2c4d5e6f708192a3b4c5d6e7f801020304", 16)
	if !ok {
		t.Fatal("bad test serial")
	}
	cert := certWith(t, big1)
	want := "7f3a1b2c4d5e6f708192a3b4c5d6e7f801020304"
	if got := SerialHex(cert); got != want {
		t.Fatalf("SerialHex = %q, want %q", got, want)
	}
	for _, spelling := range []string{
		"7F3A1B2C4D5E6F708192A3B4C5D6E7F801020304",
		"7f:3a:1b:2c:4d:5e:6f:70:81:92:a3:b4:c5:d6:e7:f8:01:02:03:04",
		"0x7f3a1b2c4d5e6f708192a3b4c5d6e7f801020304",
		"  7f3a1b2c4d5e6f708192a3b4c5d6e7f801020304  ",
		"007f3a1b2c4d5e6f708192a3b4c5d6e7f801020304",
	} {
		if got := NormaliseSerial(spelling); got != want {
			t.Errorf("NormaliseSerial(%q) = %q, want %q", spelling, got, want)
		}
	}
	// And the revoked set built from any of those spellings matches the cert.
	set := RevokedSet([]string{"7F:3A:1B:2C:4D:5E:6F:70:81:92:A3:B4:C5:D6:E7:F8:01:02:03:04"})
	if !set[SerialHex(cert)] {
		t.Error("a revocation written in one spelling did not match the certificate")
	}
	// An empty or zero entry is not a revocation of everything.
	if s := RevokedSet([]string{"", "0", "0x0"}); len(s) != 0 {
		t.Errorf("empty entries became revocations: %v", s)
	}
}

// The identity comes from the VERIFIED chain, never from what the client offered.
func TestIdentityComesFromTheVerifiedChain(t *testing.T) {
	cert := certWith(t, big.NewInt(42), "spiffe://opod.local/opod/node/n_a")
	// Offered but not verified: no identity. This is the case a leader must not
	// believe — the handshake is configured to verify if given, so an
	// unverifiable certificate never gets a chain, and reading PeerCertificates
	// instead would hand an attacker a free identity.
	r := &http.Request{TLS: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}}
	id, err := VerifiedNodeIdentity(r, nil)
	if err != nil || id.Present {
		t.Errorf("an unverified certificate produced %+v (err %v)", id, err)
	}
	// Verified: the identity and the serial.
	r = &http.Request{TLS: &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}}
	id, err = VerifiedNodeIdentity(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !id.Present || id.NodeID != "n_a" || id.Serial != "2a" {
		t.Errorf("identity = %+v", id)
	}
	// Plain HTTP: nothing, and no error — a leader with mTLS off serves plain.
	if id, err := VerifiedNodeIdentity(&http.Request{}, nil); err != nil || id.Present {
		t.Errorf("a plain request produced %+v (err %v)", id, err)
	}
}

// A revoked certificate is refused even though the CA signed it — and the answer
// names the node, so the refusal is actionable rather than a mystery 403.
func TestARevokedCertificateIsRefusedWithItsIdentity(t *testing.T) {
	cert := certWith(t, big.NewInt(255), "spiffe://opod.local/opod/node/n_gone")
	r := &http.Request{TLS: &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}}
	id, err := VerifiedNodeIdentity(r, RevokedSet([]string{"ff"}))
	if !errors.Is(err, ErrCertRevoked) {
		t.Fatalf("err = %v, want ErrCertRevoked", err)
	}
	if id.NodeID != "n_gone" || id.Serial != "ff" {
		t.Errorf("the refusal did not name the node: %+v", id)
	}
}

// An unrecognised mode is OFF, never require: a typo in a policy file must not
// lock a fleet out of its own leaders.
func TestAnUnknownModeIsOff(t *testing.T) {
	for _, in := range []string{"", "off", "OFF", " off ", "yes", "true", "1", "requre", "mtls"} {
		if got := ParseMTLSMode(in); got != MTLSOff {
			t.Errorf("ParseMTLSMode(%q) = %q, want off", in, got)
		}
	}
	if got := ParseMTLSMode(" Allow "); got != MTLSAllow {
		t.Errorf("allow = %q", got)
	}
	if got := ParseMTLSMode("REQUIRE"); got != MTLSRequire {
		t.Errorf("require = %q", got)
	}
}
