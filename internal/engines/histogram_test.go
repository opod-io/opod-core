package engines

import (
	"math"
	"testing"
	"time"
)

// ttftText is a TTFT histogram as vLLM publishes it: buckets per model name,
// with the _sum and _count beside them and an unrelated metric around it.
func ttftText(name string, counts [5]int) string {
	les := []string{"0.01", "0.05", "0.1", "0.5", "+Inf"}
	s := "# HELP " + name + " ttft\n# TYPE " + name + " histogram\nvllm:num_requests_waiting{model_name=\"m\"} 0\n"
	for i, le := range les {
		s += name + "_bucket{le=\"" + le + "\",model_name=\"m\"} " + itoa(counts[i]) + "\n"
	}
	return s + name + "_sum{model_name=\"m\"} 9\n" + name + "_count{model_name=\"m\"} " + itoa(counts[4]) + "\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

func TestPromHistogramSumsSeriesAndSorts(t *testing.T) {
	text := ttftText("x", [5]int{1, 2, 3, 4, 5}) +
		"x_bucket{le=\"0.01\",model_name=\"n\"} 1\nx_bucket{le=\"+Inf\",model_name=\"n\"} 2\n"
	b := PromHistogram(text, "x")
	if len(b) != 5 || b[0].LE != 0.01 || b[0].Count != 2 || !math.IsInf(b[4].LE, 1) || b[4].Count != 7 {
		t.Fatalf("buckets %+v", b)
	}
	if PromHistogram("y 1\n", "x") != nil {
		t.Fatal("an absent histogram must read as nil")
	}
}

func TestHistogramQuantileInterpolates(t *testing.T) {
	cases := []struct {
		name string
		b    []HistBucket
		want float64
		ok   bool
	}{
		{"empty", nil, 0, false},
		{"nothing observed", []HistBucket{{0.1, 0}, {math.Inf(1), 0}}, 0, false},
		// 10 observations, all in (0.05, 0.1]: the 5th sits halfway in it.
		{"inside one bucket", []HistBucket{{0.05, 0}, {0.1, 10}, {math.Inf(1), 10}}, 0.075, true},
		// in the first bucket, interpolated from 0
		{"first bucket", []HistBucket{{0.1, 4}, {math.Inf(1), 4}}, 0.05, true},
		// the median lands in +Inf: the largest finite bound
		{"overflow", []HistBucket{{0.1, 1}, {math.Inf(1), 10}}, 0.1, true},
	}
	for _, c := range cases {
		got, ok := histogramQuantile(0.5, c.b)
		if ok != c.ok || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: got %v %v, want %v %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// The median covers about the last minute, not the engine's life: a slow
// first minute leaves the window, and a quiet window says nothing.
func TestHistogramWindowIsTheLastMinute(t *testing.T) {
	var w HistogramWindow
	t0 := time.Unix(1_800_000_000, 0)
	// first reading: 10 slow requests (0.1–0.5 s) since start — no window yet
	if v := w.TTFTP50Ms(ttftText("t", [5]int{0, 0, 0, 10, 10}), "t", t0); v != nil {
		t.Fatalf("the first reading has no window, got %d", *v)
	}
	// 30 s later: 10 fast requests (≤ 10 ms) since the first reading
	v := w.TTFTP50Ms(ttftText("t", [5]int{10, 10, 10, 20, 20}), "t", t0.Add(30*time.Second))
	if v == nil || *v != 5 {
		t.Fatalf("the window holds only the fast requests: want 5 ms, got %v", v)
	}
	// 90 s: nothing new since 30 s — the base is the 30 s reading, nothing observed
	if v := w.TTFTP50Ms(ttftText("t", [5]int{10, 10, 10, 20, 20}), "t", t0.Add(90*time.Second)); v != nil {
		t.Fatalf("a quiet minute must say nothing, got %d", *v)
	}
	// the engine restarted: counts went down — not said, and counting starts again
	if v := w.TTFTP50Ms(ttftText("t", [5]int{1, 1, 1, 1, 1}), "t", t0.Add(95*time.Second)); v != nil {
		t.Fatalf("a reset must say nothing, got %d", *v)
	}
	if v := w.TTFTP50Ms(ttftText("t", [5]int{1, 1, 1, 3, 3}), "t", t0.Add(100*time.Second)); v == nil || *v != 300 {
		t.Fatalf("after a reset the window counts from the reset: want 300 ms, got %v", v)
	}
	// the engine stopped publishing it
	if v := w.TTFTP50Ms("other 1\n", "t", t0.Add(105*time.Second)); v != nil {
		t.Fatalf("no histogram, no figure: got %d", *v)
	}
}
