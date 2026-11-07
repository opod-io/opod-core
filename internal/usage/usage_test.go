package usage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/opod-io/opod-core/pkg/adminapi"
)

func rec(model string, i int) Record {
	return Record{TS: time.Unix(1_700_000_000+int64(i), 0), RequestID: "0123456789abcdef", APIKeyID: "key_4f2a9c",
		Model: model, Protocol: "chat", PromptTokens: 812, CompletionTokens: 213, LatencyMS: 1840, TTFTMS: 95,
		Outcome: "ok", NodeID: "n_opod-ep-chat-small-w-0"}
}

func seqs(t *testing.T, q *Queue) []int64 {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]int64, len(q.items))
	for i, it := range q.items {
		out[i] = it.seq
	}
	return out
}

func oneSize(t *testing.T) int {
	t.Helper()
	return len(marshal(1, rec("m", 1))) + itemOverhead
}

// Q1: order is kept and the OLDEST leave at the byte cap; the drop count is
// exactly the records that left.
func TestQueueDropsTheOldestAtTheByteCapAndCountsThem(t *testing.T) {
	size := oneSize(t)
	q := NewQueue(1, size*3+size/2) // room for three
	for i := 1; i <= 5; i++ {
		q.Add(rec("m", i))
	}
	if got := seqs(t, q); len(got) != 3 || got[0] != 3 || got[2] != 5 {
		t.Fatalf("queue = %v, want [3 4 5]", got)
	}
	st := q.Stats()
	if st.Dropped != 2 || st.Records != 3 || st.Bytes > st.Cap {
		t.Fatalf("stats = %+v, want 2 dropped, 3 held, within the cap", st)
	}
}

// Q2: a record bigger than the whole queue is refused and counted; it never
// empties the queue to make room it can not have.
func TestARecordBiggerThanTheQueueIsRefusedNotWedged(t *testing.T) {
	size := oneSize(t)
	q := NewQueue(1, size*2)
	q.Add(rec("m", 1))
	big := rec("m", 2)
	big.UserID = strings.Repeat("u", size*3)
	q.Add(big)
	q.Add(rec("m", 3))
	if got := seqs(t, q); len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("queue = %v, want [1 3]", got)
	}
	if st := q.Stats(); st.Refused != 1 || st.Dropped != 0 {
		t.Fatalf("stats = %+v, want 1 refused, 0 dropped", st)
	}
}

type receiver struct {
	mu      sync.Mutex
	status  []int // one per call; the last repeats
	reject  int64
	got     [][]*logspb.LogRecord
	headers http.Header
	calls   atomic.Int32
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	n := int(r.calls.Add(1)) - 1
	body, _ := io.ReadAll(req.Body)
	var in collogspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(body, &in); err != nil {
		w.WriteHeader(http.StatusTeapot)
		return
	}
	r.mu.Lock()
	r.headers = req.Header.Clone()
	code := r.status[min(n, len(r.status)-1)]
	if code/100 == 2 {
		r.got = append(r.got, in.ResourceLogs[0].ScopeLogs[0].LogRecords)
	}
	r.mu.Unlock()
	w.WriteHeader(code)
	if code/100 == 2 && r.reject > 0 {
		b, _ := proto.Marshal(&collogspb.ExportLogsServiceResponse{PartialSuccess: &collogspb.ExportLogsPartialSuccess{RejectedLogRecords: r.reject}})
		_, _ = w.Write(b)
	}
}

func newExporter(t *testing.T, q *Queue, rc *receiver) *Exporter {
	t.Helper()
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	e, err := NewExporter(q, srv.URL+"/v1/logs", "Authorization=Bearer tok=en,X-Opod-Endpoint=ep_1")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// Q3: a 2xx frees exactly the batch sent; a 5xx, a 429, a 401 and a reset free
// nothing; a 400 and a partial success's rejected records are permanent and
// counted (OTLP: never retry them).
func TestOnlyA2xxFreesTheBatch(t *testing.T) {
	for _, code := range []int{500, 503, 429, 401, 403} {
		q := NewQueue(7, 0)
		q.Add(rec("m", 1))
		q.Add(rec("m", 2))
		e := newExporter(t, q, &receiver{status: []int{code}})
		if _, err := e.Flush(context.Background()); err == nil {
			t.Fatalf("%d: no error", code)
		}
		if got := q.Stats().Records; got != 2 {
			t.Fatalf("%d: %d records left, want 2 (nothing freed)", code, got)
		}
	}

	q := NewQueue(7, 0)
	for i := 1; i <= 3; i++ {
		q.Add(rec("m", i))
	}
	rc := &receiver{status: []int{200}}
	e := newExporter(t, q, rc)
	n, err := e.Flush(context.Background())
	if err != nil || n != 3 || q.Stats().Records != 0 {
		t.Fatalf("2xx: n=%d err=%v left=%d, want 3 freed", n, err, q.Stats().Records)
	}
	if rc.headers.Get("Authorization") != "Bearer tok=en" || rc.headers.Get("X-Opod-Endpoint") != "ep_1" {
		t.Fatalf("headers not sent as configured: %v", rc.headers)
	}

	// A record added while a batch is in flight is not freed by its ack.
	q = NewQueue(7, 0)
	q.Add(rec("m", 1))
	_, last := q.peek(maxBatchBytes)
	q.Add(rec("m", 2))
	q.ack(last)
	if got := seqs(t, q); len(got) != 1 || got[0] != 2 {
		t.Fatalf("after ack(%d) queue = %v, want [2]", last, got)
	}

	q = NewQueue(7, 0)
	q.Add(rec("m", 1))
	if _, err := newExporter(t, q, &receiver{status: []int{400}}).Flush(context.Background()); err == nil || q.Stats().Records != 0 {
		t.Fatalf("400: err=%v left=%d, want an error and the batch freed", err, q.Stats().Records)
	}

	q = NewQueue(7, 0)
	q.Add(rec("m", 1))
	q.Add(rec("m", 2))
	if n, err := newExporter(t, q, &receiver{status: []int{200}, reject: 1}).Flush(context.Background()); err != nil || n != 2 || q.Stats().Records != 0 {
		t.Fatalf("partial success: n=%d err=%v left=%d", n, err, q.Stats().Records)
	}

	// A receiver that hangs up mid-answer frees nothing.
	q = NewQueue(7, 0)
	q.Add(rec("m", 1))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		c, _, _ := hj.Hijack()
		_ = c.Close()
	}))
	defer srv.Close()
	e, _ = NewExporter(q, srv.URL, "")
	if _, err := e.Flush(context.Background()); err == nil || q.Stats().Records != 1 {
		t.Fatalf("reset: err=%v left=%d, want an error and nothing freed", err, q.Stats().Records)
	}
}

// Q3 (batching): a batch never passes maxBatchBytes, and Run drains a backlog
// in several pushes, in order.
func TestRunDrainsABacklogInOrderedBatches(t *testing.T) {
	q := NewQueue(7, 0)
	n := (maxBatchBytes/len(marshal(1, rec("m", 1))))*2 + 10
	for i := 1; i <= n; i++ {
		q.Add(rec("m", i))
	}
	rc := &receiver{status: []int{200}}
	e := newExporter(t, q, rc)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go e.Run(ctx)
	for q.Stats().Records > 0 && ctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.got) < 3 {
		t.Fatalf("%d pushes, want ≥ 3 for %d records", len(rc.got), n)
	}
	var want int64 = 1
	for _, batch := range rc.got {
		for _, lr := range batch {
			if s := attrInt(lr, adminapi.UsageAttrSeq); s != want {
				t.Fatalf("seq %d, want %d (order broken)", s, want)
			}
			want++
		}
	}
}

// Q4: the backoff is bounded and every wait ends with the parent context.
func TestBackoffIsBoundedAndHonoursTheParentContext(t *testing.T) {
	q := NewQueue(7, 0)
	q.Add(rec("m", 1))
	rc := &receiver{status: []int{503}}
	e := newExporter(t, q, rc)
	e.minBackoff, e.maxBackoff = 5*time.Millisecond, 20*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	e.Run(ctx)
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("Run outlived its context by %v", d-300*time.Millisecond)
	}
	// 300 ms at a 20 ms cap is ≥ 10 tries; an unbounded doubling would make ~6.
	if c := rc.calls.Load(); c < 10 {
		t.Fatalf("%d tries in 300 ms, want ≥ 10 (backoff not capped)", c)
	}
	if q.Stats().Records != 1 {
		t.Fatal("a failing receiver freed the record")
	}
	if _, err := e.Flush(context.Background()); err == nil || !strings.Contains(e.Status().LastErr, "503") {
		t.Fatalf("status = %+v, want the 503", e.Status())
	}
}

func attrInt(lr *logspb.LogRecord, k string) int64 {
	for _, a := range lr.Attributes {
		if a.Key == k {
			return a.Value.GetIntValue()
		}
	}
	return -1
}

func attrStr(lr *logspb.LogRecord, k string) (string, bool) {
	for _, a := range lr.Attributes {
		if a.Key == k {
			return a.Value.GetStringValue(), true
		}
	}
	return "", false
}

// Q5: every field survives the trip, zero values included (TTFT 0 = not
// streamed), and the boot rides on the resource.
func TestEveryFieldRoundTrips(t *testing.T) {
	q := NewQueue(1_761_955_200, 0)
	r := rec("qwen", 1)
	r.UserID, r.Gateway = "u_1", "door-1"
	r.CachedTokens, r.Class, r.Operation = 600, "batch", "chat"
	q.Add(r)
	z := Record{TS: time.Unix(5, 0), Model: "qwen", Outcome: "error"}
	q.Add(z)
	recs, _ := q.peek(maxBatchBytes)
	e := &Exporter{q: q}
	body, err := e.request(recs)
	if err != nil {
		t.Fatal(err)
	}
	var in collogspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(body, &in); err != nil {
		t.Fatal(err)
	}
	var boot int64
	for _, a := range in.ResourceLogs[0].Resource.Attributes {
		if a.Key == adminapi.UsageAttrLeaderBoot {
			boot = a.Value.GetIntValue()
		}
	}
	if boot != 1_761_955_200 {
		t.Fatalf("boot = %d", boot)
	}
	lrs := in.ResourceLogs[0].ScopeLogs[0].LogRecords
	a := lrs[0]
	if a.EventName != adminapi.UsageEventName || a.TimeUnixNano != uint64(r.TS.UnixNano()) {
		t.Fatalf("event %q ts %d", a.EventName, a.TimeUnixNano)
	}
	for k, want := range map[string]string{adminapi.UsageAttrModel: "qwen", adminapi.UsageAttrRequestID: r.RequestID,
		adminapi.UsageAttrAPIKeyID: r.APIKeyID, adminapi.UsageAttrUserID: "u_1", adminapi.UsageAttrProtocol: "chat",
		adminapi.UsageAttrOutcome: "ok", adminapi.UsageAttrNodeID: r.NodeID, adminapi.UsageAttrGateway: "door-1",
		adminapi.UsageAttrClass: "batch", adminapi.UsageAttrOperation: "chat"} {
		if got, _ := attrStr(a, k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
	for k, want := range map[string]int64{adminapi.UsageAttrSeq: 1, adminapi.UsageAttrInputTokens: 812,
		adminapi.UsageAttrOutputTokens: 213, adminapi.UsageAttrLatencyMS: 1840, adminapi.UsageAttrTTFTMS: 95,
		adminapi.UsageAttrCachedTokens: 600} {
		if got := attrInt(a, k); got != want {
			t.Fatalf("%s = %d, want %d", k, got, want)
		}
	}
	b := lrs[1]
	for _, k := range []string{adminapi.UsageAttrTTFTMS, adminapi.UsageAttrInputTokens, adminapi.UsageAttrOutputTokens, adminapi.UsageAttrLatencyMS, adminapi.UsageAttrCachedTokens} {
		if got := attrInt(b, k); got != 0 {
			t.Fatalf("zero record: %s = %d, want 0 present", k, got)
		}
	}
	for _, k := range []string{adminapi.UsageAttrAPIKeyID, adminapi.UsageAttrClass, adminapi.UsageAttrOperation} {
		if _, ok := attrStr(b, k); ok {
			t.Fatalf("an empty %s was sent", k)
		}
	}
}

// Q7 (the reader it replaces): the last-used map gives the LRU order the
// table's MAX(ts) GROUP BY model gave for the same sequence.
func TestLastUsedGivesTheTablesLRUOrder(t *testing.T) {
	q := NewQueue(1, 0)
	seq := []struct {
		model string
		ts    int64
	}{{"a", 10}, {"b", 20}, {"a", 30}, {"c", 25}, {"b", 15}} // b's late arrival is older than its newest
	want := map[string]int64{}
	for _, s := range seq {
		q.Add(Record{TS: time.Unix(s.ts, 0), Model: s.model})
		if s.ts > want[s.model] {
			want[s.model] = s.ts
		}
	}
	got := q.LastUsed()
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for m, ts := range want {
		if got[m].Unix() != ts {
			t.Fatalf("%s = %d, want %d", m, got[m].Unix(), ts)
		}
	}
}

// Q8: the default queue holds ~400 k realistic records, and the heap it costs
// stays near its cap (the cap counts each record's encoding and its slot).
func TestTheDefaultQueueHoldsAboutFourHundredThousandRecords(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates ~100 MB")
	}
	size := oneSize(t)
	limit := DefaultQueueMB << 20
	n := limit / size
	if n < 400_000 {
		t.Fatalf("a realistic record is %d B, so %d MiB holds only %d", size, DefaultQueueMB, n)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	q := NewQueue(1, limit)
	for i := range n + 1000 {
		q.Add(rec("qwen2.5-7b-instruct", i))
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	st := q.Stats()
	if st.Bytes > limit || st.Dropped < 1000 {
		t.Fatalf("stats %+v: over the cap or nothing dropped", st)
	}
	heap := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if most := int64(limit) * 6 / 5; heap > most {
		t.Fatalf("heap %d MiB for a %d MiB queue (> 1.2×)", heap>>20, DefaultQueueMB)
	}
	t.Logf("record + slot %d B · %d records in %d MiB · heap %d MiB", size, st.Records, DefaultQueueMB, heap>>20)
	runtime.KeepAlive(q)
}

func TestHeadersParse(t *testing.T) {
	h, err := ParseHeaders(" Authorization=Bearer a=b= , X-Opod-Endpoint=ep_1,")
	if err != nil || h["Authorization"] != "Bearer a=b=" || h["X-Opod-Endpoint"] != "ep_1" {
		t.Fatalf("%v %v", h, err)
	}
	if _, err := ParseHeaders("novalue"); err == nil {
		t.Fatal("accepted a header with no =")
	}
	if _, err := NewExporter(NewQueue(1, 0), "collector:4318", ""); err == nil {
		t.Fatal("accepted a URL with no scheme")
	}
}
