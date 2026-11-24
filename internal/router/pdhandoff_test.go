package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/metrics"
	"github.com/opod-io/opod-core/internal/store"
)

// pdWorker is one half of a fake prefill/decode pair: it records every chat
// body it is sent, answers a prefill step with kv_transfer_params, streams a
// decode answer, and serves the sampling-defaults route.
type pdWorker struct {
	srv *httptest.Server

	mu     sync.Mutex
	bodies []map[string]any

	defaults   string // the /v1/sampling-defaults body; "" = 404 (an older worker)
	failWithKV bool   // answer 500 to a chat that carries kv_transfer_params
}

const prefillAnswer = `{"do_remote_prefill":true,"do_remote_decode":false,"remote_engine_id":"eng-p","remote_block_ids":[[7,8]],"remote_host":"10.0.0.9","remote_port":5600,"remote_request_id":"r-1","tp_size":1}`

func newPDWorker(t *testing.T, defaults string) *pdWorker {
	t.Helper()
	w := &pdWorker{defaults: defaults}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sampling-defaults" {
			w.mu.Lock()
			defaults := w.defaults
			w.mu.Unlock()
			if defaults == "" {
				http.NotFound(rw, r)
				return
			}
			_, _ = io.WriteString(rw, defaults)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.mu.Lock()
		w.bodies = append(w.bodies, body)
		w.mu.Unlock()
		if _, kv := body["kv_transfer_params"]; kv && w.failWithKV {
			http.Error(rw, "engine: KV load failed", http.StatusInternalServerError)
			return
		}
		if stream, _ := body["stream"].(bool); !stream {
			rw.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(rw, `{"choices":[{"message":{"content":"x"},"finish_reason":"length"}],"kv_transfer_params":`+prefillAnswer+`}`)
			return
		}
		rw.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(rw, delta("decoded")+"data: [DONE]\n\n")
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *pdWorker) chats() []map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]map[string]any(nil), w.bodies...)
}

func pdPair(t *testing.T, prefill, decode *pdWorker) *Router {
	t.Helper()
	r, _ := drainRouter(t,
		store.Node{ID: "wp", State: store.NodeStateReady, HardwareJSON: `{"Role":"prefill","Engine":"vllm"}`},
		store.Node{ID: "wd", State: store.NodeStateReady, HardwareJSON: `{"Role":"decode","Engine":"vllm"}`})
	r.remotes["wp"] = engines.MustNew("vllm", prefill.srv.URL, "")
	r.remotes["wd"] = engines.MustNew("vllm", decode.srv.URL, "")
	return r
}

func pdChat(t *testing.T, r *Router, extra map[string]json.RawMessage) string {
	t.Helper()
	stream, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m", Stream: true, Extra: extra,
		Messages: []engines.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	text, evErr := drainAnswer(t, stream)
	if evErr != nil {
		t.Fatal(evErr)
	}
	return text
}

// PLAN T16.9: a request picked onto a decode worker is prefilled on a prefill
// worker first, as one non-streamed call with max_tokens 1 and
// do_remote_decode, and the decode call carries the kv_transfer_params the
// prefill engine answered with — exactly, so its engine pulls those blocks.
func TestADecodeWorkerGetsThePromptsKVFromAPrefillWorker(t *testing.T) {
	p, d := newPDWorker(t, `{"known":true}`), newPDWorker(t, `{"known":true,"repetition_penalty":1}`)
	if got := pdChat(t, pdPair(t, p, d), nil); got != "decoded" {
		t.Fatalf("answer = %q, want the decode worker's", got)
	}
	pre, dec := p.chats(), d.chats()
	if len(pre) != 1 || len(dec) != 1 {
		t.Fatalf("want one prefill step and one decode call, got %d and %d", len(pre), len(dec))
	}
	if pre[0]["stream"] != false || pre[0]["max_tokens"] != float64(1) {
		t.Errorf("the prefill step must be one non-streamed token: %v", pre[0])
	}
	if kv, _ := pre[0]["kv_transfer_params"].(map[string]any); kv["do_remote_decode"] != true {
		t.Errorf("the prefill step must ask for a remote decode: %v", pre[0]["kv_transfer_params"])
	}
	got, _ := json.Marshal(dec[0]["kv_transfer_params"])
	var want any
	_ = json.Unmarshal([]byte(prefillAnswer), &want)
	wantB, _ := json.Marshal(want)
	if string(got) != string(wantB) {
		t.Errorf("the decode call must carry the prefill engine's params exactly:\n got %s\nwant %s", got, wantB)
	}
}

// The penalty rule (T16.9 design §6, vLLM #63693): a repetition penalty in
// force — the request's, else the model's default the decode worker reads —
// sends the request whole to its decode worker; so does a default nobody can
// read. The request's own 1.0 over a 1.1 default splits.
func TestAPenaltyInForceServesTheRequestWhole(t *testing.T) {
	for _, c := range []struct {
		name, defaults string
		extra          map[string]json.RawMessage
		split          bool
	}{
		{"the model's default penalty", `{"known":true,"repetition_penalty":1.1}`, nil, false},
		{"a default nobody can read", `{"known":false}`, nil, false},
		{"an older worker with no route", "", nil, false},
		{"the request's own penalty", `{"known":true,"repetition_penalty":1}`, map[string]json.RawMessage{"repetition_penalty": json.RawMessage("1.2")}, false},
		{"the request's own 1.0 over a 1.1 default", `{"known":true,"repetition_penalty":1.1}`, map[string]json.RawMessage{"repetition_penalty": json.RawMessage("1.0")}, true},
	} {
		p, d := newPDWorker(t, `{"known":true}`), newPDWorker(t, c.defaults)
		if got := pdChat(t, pdPair(t, p, d), c.extra); got != "decoded" {
			t.Fatalf("%s: answer = %q", c.name, got)
		}
		dec := d.chats()
		_, carried := dec[0]["kv_transfer_params"]
		if split := len(p.chats()) == 1 && carried; split != c.split {
			t.Errorf("%s: split = %v, want %v (prefill steps %d)", c.name, split, c.split, len(p.chats()))
		}
	}
}

// A decode step that fails with the KV (its engine fails fast on a lost
// transfer) is served whole on the same decode worker, before anything has
// reached the caller.
func TestADecodeStepThatFailsWithTheKVIsServedWhole(t *testing.T) {
	p, d := newPDWorker(t, `{"known":true}`), newPDWorker(t, `{"known":true,"repetition_penalty":1}`)
	d.failWithKV = true
	if got := pdChat(t, pdPair(t, p, d), nil); got != "decoded" {
		t.Fatalf("answer = %q, want the decode worker's whole answer", got)
	}
	dec := d.chats()
	if len(dec) != 2 {
		t.Fatalf("want the decode call with the KV, then the whole request: %d calls", len(dec))
	}
	if _, kv := dec[1]["kv_transfer_params"]; kv {
		t.Error("the retry must be the whole request, with no KV reference")
	}
	// The pair rests: the next request is not prefilled onto it at all, so a
	// pair whose engines disagree does not pay a wasted prefill per request.
	r := pdPair(t, p, d)
	r.pdFailing.failed("wp", "wd", time.Now())
	before := len(p.chats())
	if got := pdChat(t, r, nil); got != "decoded" {
		t.Fatalf("answer = %q", got)
	}
	if len(p.chats()) != before {
		t.Fatal("a failing pair must not be prefilled onto during its back-off")
	}
}

// "Cannot say" is not remembered for long: the snapshot a worker reads its
// defaults from arrives with a rollout's prefetch, without restarting it.
func TestAnUnknownDefaultIsAskedAgainSoon(t *testing.T) {
	p, d := newPDWorker(t, `{"known":true}`), newPDWorker(t, `{"known":false}`)
	r := pdPair(t, p, d)
	pdChat(t, r, nil)
	if len(p.chats()) != 0 {
		t.Fatal("an unknown default must not split")
	}
	d.mu.Lock()
	d.defaults = `{"known":true,"repetition_penalty":1}`
	d.mu.Unlock()
	pdChat(t, r, nil) // still inside the short window: the cached unknown holds
	if len(p.chats()) != 0 {
		t.Fatal("the unknown answer is kept for its short window")
	}
	r.sampling.mu.Lock()
	for k, e := range r.sampling.m {
		e.at = e.at.Add(-samplingUnknownTTL - time.Second)
		r.sampling.m[k] = e
	}
	r.sampling.mu.Unlock()
	pdChat(t, r, nil)
	if len(p.chats()) != 1 {
		t.Fatalf("once the window passes, the now-known default must split: %d prefill steps", len(p.chats()))
	}
}

// handoffObserved is how many handoffs opod_pd_handoff_seconds holds for one
// pair, and whether the pair has a series at all.
func handoffObserved(t *testing.T, prefill, decode string) (uint64, bool) {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != "opod_pd_handoff_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			l := map[string]string{}
			for _, p := range m.GetLabel() {
				l[p.GetName()] = p.GetValue()
			}
			if l["prefill"] == prefill && l["decode"] == decode {
				return m.GetHistogram().GetSampleCount(), true
			}
		}
	}
	return 0, false
}

// PLAN T28.13: a streamed split is timed on its pair, labelled by the two
// workers' node ids. A request that did not split, or whose decode step failed
// with the KV and was served whole, is not timed — its figure would not be a
// handoff's, so it is absent rather than a made-up one. Forgetting a worker
// drops its pairs, so the series stay bounded by the endpoint's workers.
func TestASplitIsTimedOnItsPair(t *testing.T) {
	metrics.ForgetNode("wp")
	metrics.ForgetNode("wd")
	t.Cleanup(func() { metrics.ForgetNode("wp"); metrics.ForgetNode("wd") })

	// Not split (a penalty in force): no series.
	p, d := newPDWorker(t, `{"known":true}`), newPDWorker(t, `{"known":true,"repetition_penalty":1.1}`)
	pdChat(t, pdPair(t, p, d), nil)
	if _, ok := handoffObserved(t, "wp", "wd"); ok {
		t.Fatal("a request served whole is not a handoff")
	}

	// Split and served: one observation.
	p, d = newPDWorker(t, `{"known":true}`), newPDWorker(t, `{"known":true,"repetition_penalty":1}`)
	pdChat(t, pdPair(t, p, d), nil)
	if n, ok := handoffObserved(t, "wp", "wd"); !ok || n != 1 {
		t.Fatalf("observed %d (series %v), want 1 handoff on wp→wd", n, ok)
	}

	// The decode step failed with the KV: served whole, not timed.
	p, d = newPDWorker(t, `{"known":true}`), newPDWorker(t, `{"known":true,"repetition_penalty":1}`)
	d.failWithKV = true
	pdChat(t, pdPair(t, p, d), nil)
	if n, _ := handoffObserved(t, "wp", "wd"); n != 1 {
		t.Fatalf("observed %d, want still 1: a retried request is not a handoff", n)
	}

	// A non-streamed request splits too, but its decode call's first event is
	// its whole answer: not a handoff time, so not timed.
	p, d = newPDWorker(t, `{"known":true}`), newPDWorker(t, `{"known":true,"repetition_penalty":1}`)
	stream, err := pdPair(t, p, d).Chat(context.Background(), engines.ChatRequest{Model: "m",
		Messages: []engines.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, evErr := drainAnswer(t, stream); evErr != nil {
		t.Fatal(evErr)
	}
	if _, kv := d.chats()[0]["kv_transfer_params"]; !kv {
		t.Fatal("the non-streamed request must still split, or this case proves nothing")
	}
	if n, _ := handoffObserved(t, "wp", "wd"); n != 1 {
		t.Fatalf("observed %d, want still 1: a non-streamed decode is not timed", n)
	}

	metrics.ForgetNode("wd")
	if _, ok := handoffObserved(t, "wp", "wd"); ok {
		t.Fatal("a forgotten decode worker keeps no handoff series")
	}
}
