package agent

// How a worker stops (T31.6): it lets the requests already running on it
// finish before its engine goes.
//
// A stop comes from an autoscaler parking the worker, a rolling update or a
// deleted pod, and none of them asks whether the worker is idle: an
// autoscaler parks because the ENDPOINT's demand fell, not this worker's. The
// goodbye (agent.Goodbye) already keeps new requests away; without a drain the
// engine then died with the process and took every stream still on it — a
// long answer cut in the middle, from nothing but a scale-in.
//
// Two kinds of work can be in flight, and the worker waits for both, under one
// bound:
//
//   - requests through this worker's own API (a whole worker: the leader
//     proxies to the engine through it). http.Server.Shutdown waits for them;
//   - connections a PEER holds to a process the leader started here (a gang).
//     The leader dials a gang's head process directly, never through this
//     API, and the head holds its parts' rpc-servers (or Ray's workers) for as
//     long as it serves. So a gang member waits while any remote peer still
//     holds a connection to a port it listens on: the head's streams end, the
//     leader's idle connection to the head closes (openaicompat keeps one
//     2 s), the head process stops, and its parts see their last connection go.
//
// The peer wait is bounded, never exact, in two cases: a part stopped on its
// own while its head serves on (one pod evicted) is held by the head for the
// whole bound, and a pod on the host's network sees the host's sockets, so
// another process's connections hold it too. Both end at the bound.

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultDrain is the bound when the manager sets none: what a stop waited for
// requests in flight before the bound was configurable.
const DefaultDrain = 10 * time.Second

// MaxDrain is the longest bound a worker accepts. A manager gives the pod a
// grace period longer than the bound; past an hour that is a stuck pod, not a
// drain.
const MaxDrain = time.Hour

// ParseDrain reads OPOD_DRAIN_SECONDS: "" is DefaultDrain, "0" stops at once,
// a whole number of seconds up to MaxDrain is the bound.
func ParseDrain(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultDrain, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || time.Duration(n)*time.Second > MaxDrain {
		return DefaultDrain, fmt.Errorf("OPOD_DRAIN_SECONDS=%q: want whole seconds from 0 to %d", s, int(MaxDrain/time.Second))
	}
	return time.Duration(n) * time.Second, nil
}

// peerPoll is how often a draining gang member looks at its connections.
const peerPoll = 500 * time.Millisecond

// waitForPeers blocks until no remote peer holds a connection to a port this
// network namespace listens on (other than skipPort, the worker's own API,
// which the leader keeps a pooled connection to), or until the deadline. It
// reads /proc/net/tcp and tcp6, so off Linux it returns at once: there is no
// gang off Linux.
func waitForPeers(deadline time.Time, skipPort int) (held int) {
	for {
		held = 0
		for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			held += peersHolding(b, skipPort)
		}
		if held == 0 || !time.Now().Before(deadline) {
			return held
		}
		time.Sleep(min(peerPoll, time.Until(deadline)))
	}
}

// TCP states in /proc/net/tcp (include/net/tcp_states.h).
const (
	tcpEstablished = "01"
	tcpListen      = "0A"
)

// peersHolding counts the established connections in one /proc/net/tcp{,6}
// table whose local port is a port the table also LISTENS on, from a remote
// address that is not this pod, leaving out skipPort. This pod is loopback and
// every address the table shows as a local one: a gang's head dials its OWN
// rpc-server by the pod's address, not by loopback, and that connection lives
// exactly as long as the head's engine — counted, the head waited out the whole
// bound on every park (design-partner cell, 2026-10-28). An outbound
// connection has an ephemeral local port nobody listens on.
func peersHolding(table []byte, skipPort int) int {
	type conn struct {
		port   int
		remote net.IP
	}
	listens := map[int]bool{}
	self := map[string]bool{}
	var est []conn
	for _, line := range bytes.Split(table, []byte("\n"))[1:] {
		f := strings.Fields(string(line))
		if len(f) < 4 {
			continue
		}
		local, port, ok := procAddr(f[1])
		if !ok {
			continue
		}
		if !local.IsUnspecified() {
			self[local.String()] = true
		}
		switch f[3] {
		case tcpListen:
			listens[port] = true
		case tcpEstablished:
			if ip, _, ok := procAddr(f[2]); ok {
				est = append(est, conn{port, ip})
			}
		}
	}
	n := 0
	for _, c := range est {
		if listens[c.port] && c.port != skipPort && !c.remote.IsLoopback() && !self[c.remote.String()] {
			n++
		}
	}
	return n
}

// procAddr decodes one "ADDR:PORT" of /proc/net/tcp: the address is hex in
// 32-bit words of host (little-endian) order, the port is plain hex.
func procAddr(s string) (net.IP, int, bool) {
	addr, portHex, ok := strings.Cut(s, ":")
	if !ok {
		return nil, 0, false
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return nil, 0, false
	}
	raw, err := hex.DecodeString(addr)
	if err != nil || (len(raw) != net.IPv4len && len(raw) != net.IPv6len) {
		return nil, 0, false
	}
	for i := 0; i < len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	return net.IP(raw), int(port), true
}
