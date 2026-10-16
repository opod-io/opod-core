//go:build perfgate

package perfgate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// ttftP95Margin is what the leader and worker together may add to the
	// time to first token at p95, over the engine called directly on the same
	// runner. Measured on a laptop the leader adds 3–4 ms here and the slowed
	// arm 25–27 ms; the margin sits between them, and moves only with runner
	// numbers that justify it.
	ttftP95Margin = 10 * time.Millisecond

	// slowedBy is the regression the gate must catch on every run.
	slowedBy = 20 * time.Millisecond

	rounds   = 4  // arms alternate order each round, so drift lands on all of them
	perRound = 50 // requests per arm per round
	warmup   = 10 // per arm, discarded: the first request through the router dials the worker
)

// arm is one way of reaching the stub engine.
type arm struct {
	name    string
	url     string
	client  *http.Client
	samples []sample
	wall    time.Duration
}

func newArm(name, url string, conc int) *arm {
	tr := &http.Transport{MaxIdleConnsPerHost: conc * 2, DisableCompression: true}
	return &arm{name: name, url: url, client: &http.Client{Transport: tr, Timeout: 30 * time.Second}}
}

// run sends n streamed requests, conc at a time.
func (a *arm) run(t *testing.T, n, conc int, keep bool) {
	t.Helper()
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		got  []sample
		errs []error
	)
	jobs := make(chan struct{}, n)
	for range n {
		jobs <- struct{}{}
	}
	close(jobs)
	began := time.Now()
	for range conc {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				s, err := stream(context.Background(), a.client, a.url)
				mu.Lock()
				if err != nil {
					errs = append(errs, err)
				} else {
					got = append(got, s)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("arm %s: %d of %d requests failed, first: %v", a.name, len(errs), n, errs[0])
	}
	if keep {
		a.samples = append(a.samples, got...)
		a.wall += time.Since(began)
	}
}

func (a *arm) ttft(q float64) time.Duration {
	d := make([]time.Duration, len(a.samples))
	for i, s := range a.samples {
		d[i] = s.ttft
	}
	return quantile(d, q)
}

func (a *arm) tokensPerSec() float64 {
	n := 0
	for _, s := range a.samples {
		n += s.tokens
	}
	return float64(n) / a.wall.Seconds()
}

// quantile is the nearest-rank quantile.
func quantile(d []time.Duration, q float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	i := int(q*float64(len(s))+0.999999) - 1
	return s[max(0, min(i, len(s)-1))]
}

// verdict is the gate's judgement of one arm against the direct one.
type verdict struct {
	Arm             string  `json:"arm"`
	Concurrency     int     `json:"concurrency"`
	Requests        int     `json:"requests"`
	DirectTTFTP50ms float64 `json:"direct_ttft_p50_ms"`
	DirectTTFTP95ms float64 `json:"direct_ttft_p95_ms"`
	TTFTP50ms       float64 `json:"ttft_p50_ms"`
	TTFTP95ms       float64 `json:"ttft_p95_ms"`
	AddedP50ms      float64 `json:"added_ttft_p50_ms"`
	AddedP95ms      float64 `json:"added_ttft_p95_ms"`
	DirectTokPerS   float64 `json:"direct_tokens_per_s"`
	TokPerS         float64 `json:"tokens_per_s"`
	ThroughputLoss  float64 `json:"throughput_loss_pct"` // reported, not gated: runner spread not measured yet
	MarginP95ms     float64 `json:"margin_ttft_p95_ms"`
	Pass            bool    `json:"pass"`
}

func judge(direct, other *arm, conc int) verdict {
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	added := other.ttft(0.95) - direct.ttft(0.95)
	dt, ot := direct.tokensPerSec(), other.tokensPerSec()
	return verdict{
		Arm:             other.name,
		Concurrency:     conc,
		Requests:        len(other.samples),
		DirectTTFTP50ms: ms(direct.ttft(0.50)),
		DirectTTFTP95ms: ms(direct.ttft(0.95)),
		TTFTP50ms:       ms(other.ttft(0.50)),
		TTFTP95ms:       ms(other.ttft(0.95)),
		AddedP50ms:      ms(other.ttft(0.50) - direct.ttft(0.50)),
		AddedP95ms:      ms(added),
		DirectTokPerS:   dt,
		TokPerS:         ot,
		ThroughputLoss:  (dt - ot) / dt * 100,
		MarginP95ms:     ms(ttftP95Margin),
		Pass:            added <= ttftP95Margin,
	}
}

// slowed fronts target with a proxy that holds every request for d before
// passing it on: a leader that got d slower, built without touching the leader.
func slowed(t *testing.T, target string, d time.Duration) string {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.FlushInterval = -1 // stream every chunk as it arrives
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(d)
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestLeaderCost(t *testing.T) {
	bin := opodBinary(t)
	engine := httptest.NewServer(stubEngine{prefill: 20 * time.Millisecond, tokens: 16, gap: 5 * time.Millisecond})
	t.Cleanup(engine.Close)
	c := startCluster(t, bin, engine.URL)
	slowURL := slowed(t, c.leaderURL, slowedBy)

	var all []verdict
	for _, conc := range []int{1, 4} {
		arms := []*arm{
			newArm("direct", engine.URL, conc),
			newArm("leader", c.leaderURL, conc),
			newArm("leader+20ms", slowURL, conc),
		}
		for _, a := range arms {
			a.run(t, warmup, conc, false)
		}
		for r := range rounds {
			for i := range arms {
				a := arms[(i+r)%len(arms)]
				a.run(t, perRound, conc, true)
			}
		}
		direct, leader, slow := arms[0], arms[1], arms[2]
		lv, sv := judge(direct, leader, conc), judge(direct, slow, conc)
		all = append(all, lv, sv)
		if !lv.Pass {
			t.Errorf("at %d stream(s) the leader adds %.1f ms to TTFT p95 (direct %.1f ms, through the leader %.1f ms); the margin is %.0f ms",
				conc, lv.AddedP95ms, lv.DirectTTFTP95ms, lv.TTFTP95ms, lv.MarginP95ms)
		}
		if sv.Pass {
			t.Errorf("at %d stream(s) a leader %s slower passed the gate (added %.1f ms at p95): the gate cannot see a regression of that size",
				conc, slowedBy, sv.AddedP95ms)
		}
	}
	report(t, all)
}

// report logs the table, appends it to the GitHub job summary when there is
// one, and writes the JSON to $OPOD_PERFGATE_OUT for anything that keeps history.
func report(t *testing.T, vs []verdict) {
	var b strings.Builder
	b.WriteString("| arm | streams | requests | TTFT p50 | TTFT p95 | added p50 | added p95 (margin) | tokens/s | throughput loss | gate |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for _, v := range vs {
		gate := "pass"
		if !v.Pass {
			gate = "FAIL"
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %.1f ms | %.1f ms | %+.1f ms | %+.1f ms (%.0f) | %.0f | %.1f %% | %s |\n",
			v.Arm, v.Concurrency, v.Requests, v.TTFTP50ms, v.TTFTP95ms, v.AddedP50ms, v.AddedP95ms, v.MarginP95ms,
			v.TokPerS, v.ThroughputLoss, gate)
	}
	t.Logf("leader cost against the engine called directly (the leader+20ms arm must FAIL):\n%s", b.String())
	if p := os.Getenv("GITHUB_STEP_SUMMARY"); p != "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0); err == nil {
			fmt.Fprintf(f, "### Leader cost\n\nThe `leader+20ms` arm is a deliberately slowed leader and must fail.\n\n%s\n", b.String())
			f.Close()
		}
	}
	if p := os.Getenv("OPOD_PERFGATE_OUT"); p != "" {
		raw, _ := json.MarshalIndent(map[string]any{"at": time.Now().UTC().Format(time.RFC3339), "verdicts": vs}, "", "  ")
		if err := os.WriteFile(p, raw, 0o644); err != nil {
			t.Errorf("write %s: %v", p, err)
		}
	}
}
