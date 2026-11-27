package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/opod-io/opod-core/pkg/adminapi"
)

// capServer answers every chat line after a short think, states the cap that
// capFor returns for the number of answers already given, and records the
// most requests in flight at once — overall, and among those that started
// after `after` answers.
type capServer struct {
	think  time.Duration
	capFor func(answered int) string
	after  int

	mu       sync.Mutex
	inFlight int
	answered int
	peak     int
	peakLate int
}

func (c *capServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	c.mu.Lock()
	c.inFlight++
	c.peak = max(c.peak, c.inFlight)
	if c.answered >= c.after {
		c.peakLate = max(c.peakLate, c.inFlight)
	}
	c.mu.Unlock()
	time.Sleep(c.think)
	c.mu.Lock()
	c.inFlight--
	h := c.capFor(c.answered)
	c.answered++
	c.mu.Unlock()
	if h != "" {
		w.Header().Set(adminapi.HeaderAdmissionCap, h)
	}
	fmt.Fprint(w, `{"id":"chatcmpl-x","object":"chat.completion","choices":[]}`)
}

func (c *capServer) peaks() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak, c.peakLate
}

func runCap(t *testing.T, c *capServer, lines int, opt Options) Result {
	t.Helper()
	srv := httptest.NewServer(c)
	defer srv.Close()
	dir := t.TempDir()
	opt.Input = writeInput(t, dir, lines, func(int) string { return "m" })
	opt.BaseURL = srv.URL
	opt.Dir = filepath.Join(dir, "work")
	res, err := Run(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed != lines {
		t.Fatalf("every line is answered: %+v", res)
	}
	return res
}

// auto starts at 8 and rises to the cap plus a quarter — never past it. The
// answers take 200 ms: the record syncs each line before it is sent (a few ms
// on macOS), and a faster fake would measure that, not the limit.
func TestAutoConcurrencyRisesToTheCap(t *testing.T) {
	c := &capServer{think: 200 * time.Millisecond, capFor: func(int) string { return "20" }}
	runCap(t, c, 200, Options{Auto: true})
	if peak, _ := c.peaks(); peak < 21 || peak > 25 {
		t.Fatalf("a cap of 20 allows 25 in flight and the run should use more than the cap; peak %d", peak)
	}
}

// The margin over the cap is a quarter, at most 16: the leader's hold is
// shared by every class, and a margin that filled it would shed a second
// batch on arrival.
func TestAutoMarginIsAQuarterAtMost16(t *testing.T) {
	for _, tc := range []struct {
		cap       string
		max, want int
	}{
		{"1", 256, 2}, {"4", 256, 5}, {"20", 256, 25}, {"64", 256, 80}, {"100", 256, 116}, {"250", 256, 256},
	} {
		l := newLimiter(8, tc.max, true)
		l.observe(tc.cap)
		if l.limit != tc.want {
			t.Errorf("cap %s: limit %d, want %d", tc.cap, l.limit, tc.want)
		}
	}
	l := newLimiter(8, 256, false)
	if l.observe("100"); l.limit != 8 {
		t.Errorf("a fixed limit ignores the cap: %d", l.limit)
	}
}

// When the cap falls, the in-flight count falls with it.
func TestAutoConcurrencyFallsWithTheCap(t *testing.T) {
	c := &capServer{think: 100 * time.Millisecond, after: 150, capFor: func(n int) string {
		if n < 100 {
			return "20"
		}
		return "4"
	}}
	runCap(t, c, 300, Options{Auto: true})
	peak, late := c.peaks()
	if peak <= 8 {
		t.Fatalf("the cap of 20 must first raise the limit past its start of 8; peak %d", peak)
	}
	if late > 5 {
		t.Fatalf("after the cap fell to 4, at most 5 may be in flight; peak %d", late)
	}
}

// A fixed --concurrency ignores the header, and auto never passes its maximum.
func TestFixedConcurrencyIgnoresTheCap(t *testing.T) {
	c := &capServer{think: 20 * time.Millisecond, capFor: func(int) string { return "50" }}
	runCap(t, c, 60, Options{Concurrency: 3})
	if peak, _ := c.peaks(); peak > 3 {
		t.Fatalf("a fixed concurrency of 3 holds whatever the leader says; peak %d", peak)
	}
	c = &capServer{think: 20 * time.Millisecond, capFor: func(int) string { return "50" }}
	runCap(t, c, 120, Options{Auto: true, MaxConcurrency: 10})
	if peak, _ := c.peaks(); peak > 10 {
		t.Fatalf("auto never passes --max-concurrency 10; peak %d", peak)
	}
}

// shedServer sheds each line's first `sheds` attempts with the gate's marked
// 503 (or an unmarked one for the ids in plain), then answers.
type shedServer struct {
	sheds int
	plain map[string]bool
	mu    sync.Mutex
	hits  map[string]int
}

func (s *shedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		User string `json:"user"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &body)
	s.mu.Lock()
	s.hits[body.User]++
	n := s.hits[body.User]
	s.mu.Unlock()
	if s.plain[body.User] {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"message":"engine overloaded"}}`)
		return
	}
	if s.sheds < 0 || n <= s.sheds {
		w.Header().Set("Retry-After", "1")
		w.Header().Set(adminapi.HeaderAdmission, adminapi.AdmissionShed)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"message":"every worker slot for m is busy"}}`)
		return
	}
	fmt.Fprint(w, `{"id":"chatcmpl-x","object":"chat.completion","choices":[]}`)
}

// A line the gate shed is sent again and answered; a 503 the gate did not
// write is the line's answer, sent once.
func TestGateShedIsRetriedOtherFailuresAreNot(t *testing.T) {
	s := &shedServer{sheds: 1, plain: map[string]bool{"c1": true}, hits: map[string]int{}}
	srv := httptest.NewServer(s)
	defer srv.Close()
	dir := t.TempDir()
	res, err := Run(context.Background(), Options{Input: writeInput(t, dir, 3, func(int) string { return "m" }),
		BaseURL: srv.URL, Dir: filepath.Join(dir, "work"), Concurrency: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed != 2 || res.Failed != 1 {
		t.Fatalf("two shed lines are answered on their retry, the plain 503 fails: %+v", res)
	}
	if s.hits["c0"] != 2 || s.hits["c2"] != 2 || s.hits["c1"] != 1 {
		t.Fatalf("shed lines are sent twice, the plain 503 once: %v", s.hits)
	}
	if e := readResults(t, res.ErrorPath)["c1"]; e.Response == nil || e.Response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("the plain 503 is the line's answer: %+v", e)
	}
}

// A leader that sheds for good does not hold a line forever: when the line's
// budget cannot cover the next wait, the shed is its answer.
func TestGateShedStopsAtTheLineBudget(t *testing.T) {
	s := &shedServer{sheds: -1, hits: map[string]int{}}
	srv := httptest.NewServer(s)
	defer srv.Close()
	dir := t.TempDir()
	start := time.Now()
	res, err := Run(context.Background(), Options{Input: writeInput(t, dir, 1, func(int) string { return "m" }),
		BaseURL: srv.URL, Dir: filepath.Join(dir, "work"), Concurrency: 1, RequestTimeout: 1500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the budget bounds the retries; took %s", took)
	}
	if res.Failed != 1 || s.hits["c0"] != 2 {
		t.Fatalf("one wait fits in 1.5 s, the second shed is the answer: %+v hits %v", res, s.hits)
	}
	if e := readResults(t, res.ErrorPath)["c0"]; e.Response == nil || e.Response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("the last shed is the line's answer: %+v", e)
	}
}
