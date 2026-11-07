// Package usage holds the leader's usage records between the request that made
// one and the receiver that takes it (feature usage_push, T32.34).
//
// The leader keeps no usage table. Each finished request becomes one OTLP log
// record (names in pkg/adminapi/usagelog.go), encoded once on Add and
// held in a queue capped by BYTES, so the cap is exact rather than a guess at
// the size of a Go struct. An exporter sends the oldest records to the URL in
// OPOD_USAGE_OTLP_ENDPOINT and frees them only when the receiver answers 2xx.
// While the receiver is away the queue grows to its cap, then drops its oldest
// record per new one and counts it: a long outage loses the oldest facts, never
// the newest, and never silently.
//
// Delivery is at least once. The dedupe key is (boot, seq): the leader's start
// in unix seconds and a sequence that starts at 1 on every boot, the same pair
// the control plane already keyed the pulled rows by.
package usage

import (
	"sync"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/opod-io/opod/internal/metrics"
	"github.com/opod-io/opod/pkg/adminapi"
)

// DefaultQueueMB is the queue's size when OPOD_USAGE_QUEUE_MB is unset: about
// 400 000 records of a typical size, an hour of a busy endpoint.
const DefaultQueueMB = 64

// Record is one request the leader (or one of its front doors) served.
type Record struct {
	TS               time.Time
	RequestID        string // the X-Opod-Request-Id the caller saw; "" = none was minted
	APIKeyID         string
	UserID           string
	Model            string
	Protocol         string
	PromptTokens     int
	CompletionTokens int
	LatencyMS        int
	// TTFTMS is the time to the FIRST byte the client could use. 0 on a
	// non-streamed answer, where LatencyMS is the only honest number.
	TTFTMS  int
	Outcome string
	NodeID  string // the worker that served it; "" = answered locally / not dispatched
	Gateway string // the front door that served it; "" = the leader itself
	// T32.47. CachedTokens: prompt tokens the engine served from its prefix
	// cache (0 = none reported). Class: the admission class that ranked the
	// request ("" = no classes in the policy). Operation: what it did — chat,
	// embeddings or rerank.
	CachedTokens int
	Class        string
	Operation    string
}

// itemOverhead is what one queued record costs beside its encoding: its slot
// in the queue (a sequence and a slice header) and the allocator's rounding.
// The cap counts it, so OPOD_USAGE_QUEUE_MB bounds the memory the queue really
// holds, not only its payload (measured: TestTheDefaultQueue…).
const itemOverhead = 40

type item struct {
	seq int64
	b   []byte // an encoded logspb.LogRecord
}

// Queue is the bounded, in-memory holding area. Safe for concurrent use.
type Queue struct {
	boot int64
	cap  int

	mu       sync.Mutex
	items    []item
	bytes    int
	seq      int64
	dropped  int64
	refused  int64
	lastUsed map[string]time.Time
	notify   chan struct{}
}

// NewQueue returns a queue that holds at most capBytes of encoded records.
// boot is the leader's start (unix seconds), half of every record's key.
func NewQueue(boot int64, capBytes int) *Queue {
	if capBytes <= 0 {
		capBytes = DefaultQueueMB << 20
	}
	return &Queue{boot: boot, cap: capBytes, lastUsed: map[string]time.Time{}, notify: make(chan struct{}, 1)}
}

// Boot is the leader start the queue's sequence belongs to.
func (q *Queue) Boot() int64 { return q.boot }

// Add encodes r and queues it. It never blocks and never fails: recording
// usage must not be able to fail a request that already succeeded. A record
// bigger than the whole queue is refused and counted; otherwise the oldest
// records make room.
func (q *Queue) Add(r Record) {
	if r.TS.IsZero() {
		r.TS = time.Now()
	}
	// Encoding stays under the lock (a few microseconds): sequence order is
	// queue order, which is what ack relies on to free only what was sent.
	q.mu.Lock()
	defer q.mu.Unlock()
	if r.Model != "" && r.TS.After(q.lastUsed[r.Model]) {
		q.lastUsed[r.Model] = r.TS
	}
	q.seq++
	b := marshal(q.seq, r)
	if len(b)+itemOverhead > q.cap {
		q.refused++
		metrics.AddUsageDropped("refused", 1)
		return
	}
	q.items = append(q.items, item{seq: q.seq, b: b})
	q.bytes += len(b) + itemOverhead
	for q.bytes > q.cap {
		q.bytes -= len(q.items[0].b) + itemOverhead
		q.items[0] = item{}
		q.items = q.items[1:]
		q.dropped++
		metrics.AddUsageDropped("full", 1)
	}
	metrics.SetUsageQueue(q.bytes, len(q.items))
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// LastUsed returns the newest record time per model since this process
// started: the lifecycle manager's least-recently-used order. A model with no
// request since the start is absent and counts as the least recently used.
func (q *Queue) LastUsed() map[string]time.Time {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make(map[string]time.Time, len(q.lastUsed))
	for k, v := range q.lastUsed {
		out[k] = v
	}
	return out
}

// Records decodes what the queue holds, oldest first. It copies everything,
// so it is for a test or a one-off look, never the request path.
func (q *Queue) Records() []Record {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Record, 0, len(q.items))
	for _, it := range q.items {
		if _, r, err := unmarshal(it.b); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// Stats is what the queue holds and what it lost.
type Stats struct {
	Records int   `json:"records"`
	Bytes   int   `json:"bytes"`
	Cap     int   `json:"cap_bytes"`
	Seq     int64 `json:"seq"`
	Dropped int64 `json:"dropped"` // oldest records dropped to make room
	Refused int64 `json:"refused"` // records bigger than the whole queue
}

func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Stats{Records: len(q.items), Bytes: q.bytes, Cap: q.cap, Seq: q.seq, Dropped: q.dropped, Refused: q.refused}
}

// peek returns the oldest records, in order, up to maxBytes of encoding (at
// least one record when any is queued), and the last sequence among them.
func (q *Queue) peek(maxBytes int) ([][]byte, int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out [][]byte
	var last int64
	n := 0
	for _, it := range q.items {
		if len(out) > 0 && n+len(it.b) > maxBytes {
			break
		}
		out = append(out, it.b)
		n += len(it.b)
		last = it.seq
	}
	return out, last
}

// ack frees every record up to and including seq. Records the cap dropped
// while the batch was in flight are simply no longer there.
func (q *Queue) ack(seq int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	i := 0
	for i < len(q.items) && q.items[i].seq <= seq {
		q.bytes -= len(q.items[i].b) + itemOverhead
		q.items[i] = item{}
		i++
	}
	q.items = q.items[i:]
	metrics.SetUsageQueue(q.bytes, len(q.items))
}

// The compact encoding: protobuf wire format with our own field numbers. Only
// the queue reads it, so it may change with the binary; the wire to a receiver
// is OTLP (toLog).
const (
	fSeq protowire.Number = iota + 1
	fTS
	fRequestID
	fAPIKeyID
	fUserID
	fModel
	fProtocol
	fPrompt
	fCompletion
	fLatency
	fTTFT
	fOutcome
	fNodeID
	fGateway
	fCached
	fClass
	fOperation
)

func marshal(seq int64, r Record) []byte {
	b := make([]byte, 0, 128)
	num := func(n protowire.Number, v int64) {
		b = protowire.AppendTag(b, n, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(v))
	}
	str := func(n protowire.Number, v string) {
		if v != "" {
			b = protowire.AppendTag(b, n, protowire.BytesType)
			b = protowire.AppendString(b, v)
		}
	}
	num(fSeq, seq)
	num(fTS, r.TS.UnixNano())
	str(fRequestID, r.RequestID)
	str(fAPIKeyID, r.APIKeyID)
	str(fUserID, r.UserID)
	str(fModel, r.Model)
	str(fProtocol, r.Protocol)
	num(fPrompt, int64(r.PromptTokens))
	num(fCompletion, int64(r.CompletionTokens))
	num(fLatency, int64(r.LatencyMS))
	num(fTTFT, int64(r.TTFTMS))
	str(fOutcome, r.Outcome)
	str(fNodeID, r.NodeID)
	str(fGateway, r.Gateway)
	num(fCached, int64(r.CachedTokens))
	str(fClass, r.Class)
	str(fOperation, r.Operation)
	return b
}

func unmarshal(b []byte) (int64, Record, error) {
	var seq int64
	var r Record
	for len(b) > 0 {
		n, typ, l := protowire.ConsumeTag(b)
		if l < 0 {
			return 0, r, protowire.ParseError(l)
		}
		b = b[l:]
		switch typ {
		case protowire.VarintType:
			v, l := protowire.ConsumeVarint(b)
			if l < 0 {
				return 0, r, protowire.ParseError(l)
			}
			b = b[l:]
			switch n {
			case fSeq:
				seq = int64(v)
			case fTS:
				r.TS = time.Unix(0, int64(v))
			case fPrompt:
				r.PromptTokens = int(v)
			case fCompletion:
				r.CompletionTokens = int(v)
			case fLatency:
				r.LatencyMS = int(v)
			case fTTFT:
				r.TTFTMS = int(v)
			case fCached:
				r.CachedTokens = int(v)
			}
		case protowire.BytesType:
			v, l := protowire.ConsumeString(b)
			if l < 0 {
				return 0, r, protowire.ParseError(l)
			}
			b = b[l:]
			switch n {
			case fRequestID:
				r.RequestID = v
			case fAPIKeyID:
				r.APIKeyID = v
			case fUserID:
				r.UserID = v
			case fModel:
				r.Model = v
			case fProtocol:
				r.Protocol = v
			case fOutcome:
				r.Outcome = v
			case fNodeID:
				r.NodeID = v
			case fGateway:
				r.Gateway = v
			case fClass:
				r.Class = v
			case fOperation:
				r.Operation = v
			}
		default:
			l := protowire.ConsumeFieldValue(n, typ, b)
			if l < 0 {
				return 0, r, protowire.ParseError(l)
			}
			b = b[l:]
		}
	}
	return seq, r, nil
}

// toLog is the record as OTLP sends it.
func toLog(seq int64, r Record) *logspb.LogRecord {
	attrs := []*commonpb.KeyValue{
		intAttr(adminapi.UsageAttrSeq, seq),
		strAttr(adminapi.UsageAttrModel, r.Model),
		intAttr(adminapi.UsageAttrInputTokens, int64(r.PromptTokens)),
		intAttr(adminapi.UsageAttrOutputTokens, int64(r.CompletionTokens)),
		strAttr(adminapi.UsageAttrProtocol, r.Protocol),
		strAttr(adminapi.UsageAttrOutcome, r.Outcome),
		intAttr(adminapi.UsageAttrLatencyMS, int64(r.LatencyMS)),
		intAttr(adminapi.UsageAttrTTFTMS, int64(r.TTFTMS)),
		intAttr(adminapi.UsageAttrCachedTokens, int64(r.CachedTokens)),
	}
	// Empty strings are left off: on a receiver an absent attribute and an
	// empty one read the same.
	for _, kv := range [][2]string{
		{adminapi.UsageAttrRequestID, r.RequestID},
		{adminapi.UsageAttrAPIKeyID, r.APIKeyID},
		{adminapi.UsageAttrUserID, r.UserID},
		{adminapi.UsageAttrNodeID, r.NodeID},
		{adminapi.UsageAttrGateway, r.Gateway},
		{adminapi.UsageAttrClass, r.Class},
		{adminapi.UsageAttrOperation, r.Operation},
	} {
		if kv[1] != "" {
			attrs = append(attrs, strAttr(kv[0], kv[1]))
		}
	}
	return &logspb.LogRecord{
		TimeUnixNano: uint64(r.TS.UnixNano()),
		EventName:    adminapi.UsageEventName,
		Attributes:   attrs,
	}
}

func strAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func intAttr(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}
