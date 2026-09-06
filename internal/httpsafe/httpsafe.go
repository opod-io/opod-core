// Package httpsafe builds HTTP clients that can optionally refuse to connect
// to private network targets — defense in depth against SSRF when an outbound
// URL (a webhook, a guardrail endpoint, a probe target) may originate from a
// less-trusted source.
//
// The guard runs in the dialer's Control callback, so it inspects the actual
// resolved IP about to be dialed (after DNS). That closes the DNS-rebinding
// hole a hostname-only check would leave open: a name that resolves to a
// public IP on the first lookup and 169.254.169.254 on the second is still
// caught.
package httpsafe

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// NewClient returns an *http.Client with the given timeout. When blockPrivate
// is true the client refuses to dial loopback, link-local, unspecified, or
// private (RFC-1918 / RFC-4193 ULA / RFC-6598 CGNAT) addresses. When false it
// returns an ordinary client and behaves exactly as before.
func NewClient(timeout time.Duration, blockPrivate bool) *http.Client {
	if !blockPrivate {
		return &http.Client{Timeout: timeout}
	}
	d := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   blockPrivateControl,
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           d.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
}

// blockPrivateControl is a net.Dialer Control func that rejects connections to
// non-public addresses. It runs once per dialed address with the post-DNS IP.
func blockPrivateControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("httpsafe: refusing to dial non-IP address %q", address)
	}
	if isPrivate(ip) {
		return fmt.Errorf("httpsafe: refusing to dial private address %s (block_private_targets is on)", ip)
	}
	return nil
}

// isPrivate reports whether ip is in a range Opod should not reach when SSRF
// protection is enabled.
func isPrivate(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsPrivate() {
		// ip.IsPrivate covers RFC-1918 (10/8, 172.16/12, 192.168/16) and
		// RFC-4193 ULA (fc00::/7). Loopback/link-local catch 127/8, ::1,
		// 169.254/16 (incl. the cloud metadata IP), and fe80::/10.
		return true
	}
	// RFC-6598 carrier-grade NAT (100.64.0.0/10) isn't covered by IsPrivate.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	return false
}
