package usage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/opod-io/opod/internal/metrics"
	"github.com/opod-io/opod/pkg/adminapi"
)

// maxBatchBytes bounds one push. OTLP receivers commonly cap a request near
// 4 MiB; a megabyte keeps a slow link's retry cheap.
const maxBatchBytes = 1 << 20

// Exporter pushes the queue's oldest records to an OTLP/HTTP logs URL.
//
// It is our own small client, not the OpenTelemetry SDK's batch processor:
// that one frees a record when it hands it to the exporter, drops silently when
// its queue is full and caps by count, and this queue's promise is the
// opposite — freed only on a 2xx, capped by bytes, every loss counted.
type Exporter struct {
	q       *Queue
	url     string
	headers map[string]string
	http    *http.Client
	// Backoff after a failed push doubles from minBackoff to maxBackoff.
	minBackoff, maxBackoff time.Duration

	mu      sync.Mutex
	lastErr string
	lastOK  time.Time
}

// NewExporter returns an exporter for url (an OTLP/HTTP logs endpoint, e.g.
// http://collector:4318/v1/logs). headers is the OTEL_EXPORTER_OTLP_HEADERS
// form, "k=v,k=v".
func NewExporter(q *Queue, url, headers string) (*Exporter, error) {
	h, err := ParseHeaders(headers)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("usage export: %q is not an http(s) URL", url)
	}
	return &Exporter{q: q, url: url, headers: h, http: &http.Client{Timeout: 15 * time.Second},
		minBackoff: time.Second, maxBackoff: 30 * time.Second}, nil
}

// ParseHeaders reads "k=v,k=v". Values may contain '=' (a base64 token).
func ParseHeaders(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("usage export headers: %q is not k=v", part)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// Status is the exporter's last outcome, for /loadz-style reporting.
type Status struct {
	URL     string    `json:"url"`
	LastOK  time.Time `json:"last_ok,omitzero"`
	LastErr string    `json:"last_error,omitempty"`
}

func (e *Exporter) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	return Status{URL: e.url, LastOK: e.lastOK, LastErr: e.lastErr}
}

// Run pushes until ctx ends. Every wait is under ctx, so a stopping leader
// stops pushing at once (standard 6d); what is still queued is lost and was
// counted as queued.
func (e *Exporter) Run(ctx context.Context) {
	backoff := e.minBackoff
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		sent, err := e.Flush(ctx)
		if err != nil {
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, e.maxBackoff)
			continue
		}
		backoff = e.minBackoff
		if sent > 0 {
			continue // more may be waiting: drain before waiting
		}
		select {
		case <-ctx.Done():
			return
		case <-e.q.notify:
		case <-tick.C:
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Flush sends one batch of the oldest records and frees it on a 2xx. It
// returns how many records left the queue. A failure frees nothing.
//
// Per OTLP, a 400 and a partial success's rejected records are permanent: the
// receiver will never take them, so they are freed and counted as rejected
// rather than retried for ever. A 401 or 403 is retried — a receiver that does
// not know this leader's credential yet (it is still starting) must not cost
// the rows.
func (e *Exporter) Flush(ctx context.Context) (int, error) {
	recs, last := e.q.peek(maxBatchBytes)
	if len(recs) == 0 {
		return 0, nil
	}
	body, err := e.request(recs)
	if err != nil {
		return 0, e.fail(fmt.Errorf("usage export: encode: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return 0, e.fail(fmt.Errorf("usage export: %w", err))
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return 0, e.fail(fmt.Errorf("usage export to %s: %w", e.url, err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode/100 == 2:
		rejected := 0
		var out collogspb.ExportLogsServiceResponse
		if proto.Unmarshal(raw, &out) == nil && out.GetPartialSuccess() != nil {
			rejected = int(out.GetPartialSuccess().GetRejectedLogRecords())
		}
		e.q.ack(last)
		if rejected > 0 {
			metrics.AddUsageDropped("rejected", rejected)
		}
		metrics.AddUsageSent(len(recs) - rejected)
		e.mu.Lock()
		e.lastOK, e.lastErr = time.Now(), ""
		e.mu.Unlock()
		return len(recs), nil
	case resp.StatusCode == http.StatusBadRequest:
		e.q.ack(last)
		metrics.AddUsageDropped("rejected", len(recs))
		return len(recs), e.fail(fmt.Errorf("usage export to %s: 400, %d records rejected: %s", e.url, len(recs), strings.TrimSpace(string(raw))))
	default:
		return 0, e.fail(fmt.Errorf("usage export to %s: %s: %s", e.url, resp.Status, strings.TrimSpace(string(raw))))
	}
}

func (e *Exporter) fail(err error) error {
	metrics.AddUsageExportError()
	e.mu.Lock()
	e.lastErr = err.Error()
	e.mu.Unlock()
	return err
}

// request turns a batch of queued records into one ExportLogsServiceRequest.
func (e *Exporter) request(recs [][]byte) ([]byte, error) {
	lrs := make([]*logspb.LogRecord, 0, len(recs))
	for _, b := range recs {
		seq, r, err := unmarshal(b)
		if err != nil {
			return nil, err
		}
		lrs = append(lrs, toLog(seq, r))
	}
	return proto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			strAttr(adminapi.UsageAttrServiceName, "opod-leader"),
			intAttr(adminapi.UsageAttrLeaderBoot, e.q.Boot()),
		}},
		ScopeLogs: []*logspb.ScopeLogs{{
			Scope:      &commonpb.InstrumentationScope{Name: "github.com/opod-io/opod/internal/usage"},
			LogRecords: lrs,
		}},
	}}})
}
