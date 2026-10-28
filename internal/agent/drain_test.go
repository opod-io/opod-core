package agent

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseDrain(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{"", DefaultDrain, false},
		{"  ", DefaultDrain, false},
		{"0", 0, false},
		{"120", 120 * time.Second, false},
		{"3600", time.Hour, false},
		{"3601", DefaultDrain, true},
		{"-1", DefaultDrain, true},
		{"2m", DefaultDrain, true},
		{"1.5", DefaultDrain, true},
	} {
		got, err := ParseDrain(tc.in)
		if (err != nil) != tc.bad || got != tc.want {
			t.Errorf("ParseDrain(%q) = %v, %v; want %v, error %v", tc.in, got, err, tc.want, tc.bad)
		}
	}
}

// A /proc/net/tcp table as Linux prints it (addresses in host-order hex).
// Ports: 0x1F91 = 8081 (the worker's own API), 0xC350 = 50052 (an rpc-server),
// 0x1F99 = 8089 (the engine on localhost).
const procTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1F91 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0 100 0 0 10 0
   1: 00000000:C350 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2 1 0 100 0 0 10 0
   2: 0100007F:1F99 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3 1 0 100 0 0 10 0
   3: 0A0200C0:1F91 0B0200C0:D431 01 00000000:00000000 00:00000000 00000000     0        0 4 1 0 100 0 0 10 0
   4: 0100007F:1F99 0100007F:B5A2 01 00000000:00000000 00:00000000 00000000     0        0 5 1 0 100 0 0 10 0
   5: 0A0200C0:B5A3 0C0200C0:1F90 01 00000000:00000000 00:00000000 00000000     0        0 6 1 0 100 0 0 10 0
`

func TestPeersHoldingCountsOnlyARemotePeerOnAListenedPort(t *testing.T) {
	// Row 3: the leader's pooled connection to the worker API — skipped by port.
	// Row 4: the worker talking to its own engine — loopback.
	// Row 5: this worker's own outbound connection (an ephemeral local port).
	if n := peersHolding([]byte(procTCP), 8081); n != 0 {
		t.Fatalf("no peer holds this worker: got %d", n)
	}
	// The head holding the rpc-server: a remote peer on a listened port.
	held := procTCP + "   6: 0A0200C0:C350 0D0200C0:9C41 01 00000000:00000000 00:00000000 00000000     0        0 7 1 0 100 0 0 10 0\n"
	if n := peersHolding([]byte(held), 8081); n != 1 {
		t.Fatalf("the head's connection to the rpc-server is a peer: got %d", n)
	}
	// Without the skip the leader's pooled API connection would count too.
	if n := peersHolding([]byte(held), 0); n != 2 {
		t.Fatalf("got %d, want 2", n)
	}
}

func TestPeersHoldingReadsIPv6Tables(t *testing.T) {
	const listen = "00000000000000000000000000000000:C350 00000000000000000000000000000000:0000 0A"
	const loop6 = "00000000000000000000000001000000:C350 00000000000000000000000001000000:A000 01"  // ::1
	const mapped = "0000000000000000FFFF00000100007F:C350 0000000000000000FFFF00000100007F:A001 01" // ::ffff:127.0.0.1
	const remote = "0000000000000000FFFF00000A0200C0:C350 0000000000000000FFFF00000D0200C0:A002 01" // ::ffff:192.0.2.13
	row := func(i int, s string) string { return fmt.Sprintf("   %d: %s 0 0 0 0 0 0 0\n", i, s) }
	table := "header\n" + row(0, listen) + row(1, loop6) + row(2, mapped)
	if n := peersHolding([]byte(table), 0); n != 0 {
		t.Fatalf("loopback in either spelling is not a peer: got %d", n)
	}
	if n := peersHolding([]byte(table+row(3, remote)), 0); n != 1 {
		t.Fatalf("a remote IPv4-mapped peer is a peer: got %d", n)
	}
}

// startDraining serves handler under Server.Start with the given bound and
// returns the address, the stop, and Start's result.
func startDraining(t *testing.T, drain time.Duration, handler http.HandlerFunc) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	s := &Server{Drain: drain}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx, addr, handler) }()
	for i := 0; ; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			_ = c.Close()
			break
		}
		if i > 200 {
			t.Fatal("server never listened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return addr, cancel, done
}

// streamOf answers a stream of n lines, one every gap.
func streamOf(n int, gap time.Duration, started chan<- struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		for i := 0; i < n; i++ {
			fmt.Fprintf(w, "data: %d\n", i)
			f.Flush()
			if i == 0 {
				close(started)
			}
			time.Sleep(gap)
		}
		fmt.Fprint(w, "data: [DONE]\n")
	}
}

func readStream(addr string) (string, error) {
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var b strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		b.WriteString(sc.Text() + "\n")
	}
	return b.String(), sc.Err()
}

func TestAStopLetsTheStreamInFlightFinish(t *testing.T) {
	started := make(chan struct{})
	addr, stop, done := startDraining(t, 5*time.Second, streamOf(5, 100*time.Millisecond, started))
	got := make(chan string, 1)
	go func() { s, _ := readStream(addr); got <- s }()
	<-started
	stop() // the SIGTERM
	select {
	case s := <-got:
		if !strings.Contains(s, "[DONE]") {
			t.Fatalf("the stream in flight was cut by the stop: %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never ended")
	}
	if err := <-done; err != nil {
		t.Fatalf("a drained stop returns nil: %v", err)
	}
	if _, err := net.Dial("tcp", addr); err == nil {
		t.Fatal("a stopped worker must take no new connection")
	}
}

func TestAStopCutsWhatOutlivesTheBound(t *testing.T) {
	for _, drain := range []time.Duration{0, 200 * time.Millisecond} {
		started := make(chan struct{})
		addr, stop, done := startDraining(t, drain, streamOf(100, 100*time.Millisecond, started))
		got := make(chan string, 1)
		go func() { s, _ := readStream(addr); got <- s }()
		<-started
		t0 := time.Now()
		stop()
		select {
		case s := <-got:
			if strings.Contains(s, "[DONE]") {
				t.Fatalf("drain %v: a 10 s stream finished under the bound", drain)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("drain %v: the stream outlived its bound", drain)
		}
		<-done
		if el := time.Since(t0); el > drain+time.Second {
			t.Fatalf("drain %v: the stop took %v", drain, el)
		}
	}
}
