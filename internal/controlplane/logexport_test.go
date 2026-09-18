package controlplane

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
)

// otlpSink is a collector's /v1/logs: it decodes what the exporter posts.
type otlpSink struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*collogspb.ExportLogsServiceRequest
}

func newOTLPSink(t *testing.T) *otlpSink {
	t.Helper()
	k := &otlpSink{}
	k.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/logs" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req collogspb.ExportLogsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			t.Errorf("the payload is not an ExportLogsServiceRequest: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		k.mu.Lock()
		k.reqs = append(k.reqs, &req)
		k.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		out, _ := proto.Marshal(&collogspb.ExportLogsServiceResponse{})
		_, _ = w.Write(out)
	}))
	t.Cleanup(k.Close)
	return k
}

// records flattens what arrived into body → attributes, and the service name.
func (k *otlpSink) records() (byBody map[string]map[string]string, service string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	byBody = map[string]map[string]string{}
	for _, req := range k.reqs {
		for _, rl := range req.ResourceLogs {
			for _, a := range rl.GetResource().GetAttributes() {
				if a.Key == "service.name" {
					service = a.Value.GetStringValue()
				}
			}
			for _, sl := range rl.ScopeLogs {
				for _, rec := range sl.LogRecords {
					attrs := map[string]string{"severity": rec.SeverityText}
					for _, a := range rec.Attributes {
						attrs[a.Key] = a.Value.GetStringValue()
					}
					byBody[rec.Body.GetStringValue()] = attrs
				}
			}
		}
	}
	return byBody, service
}

func TestExportLogsTeesToTheCollectorAndKeepsTheLocalHandler(t *testing.T) {
	sink := newOTLPSink(t)
	var local bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&local, &slog.HandlerOptions{Level: slog.LevelInfo}))

	log, shutdown, err := ExportLogs(context.Background(), sink.URL, "v-test", base)
	if err != nil {
		t.Fatal(err)
	}
	log.With("node", "node-a").Warn("worker went quiet", "model", "m")
	log.Debug("below the configured level")
	if err := shutdown(context.Background()); err != nil { // shutdown flushes the batch
		t.Fatalf("shutdown: %v", err)
	}

	got, service := sink.records()
	rec, ok := got["worker went quiet"]
	if !ok {
		t.Fatalf("the record never reached the collector; got %v", got)
	}
	if rec["node"] != "node-a" || rec["model"] != "m" || !strings.EqualFold(rec["severity"], "WARN") {
		t.Errorf("exported record = %v, want node, model and WARN", rec)
	}
	if service != "opod" {
		t.Errorf("service.name = %q, want opod — the same resource the spans carry", service)
	}
	if _, leaked := got["below the configured level"]; leaked {
		t.Error("a debug record was exported although log_level is info: the local level must govern both")
	}
	if out := local.String(); !strings.Contains(out, "worker went quiet") || strings.Contains(out, "below the configured level") {
		t.Errorf("the local handler must keep working unchanged; it wrote:\n%s", out)
	}
}

func TestExportLogsOffIsTheSameLogger(t *testing.T) {
	base := slog.New(slog.NewTextHandler(io.Discard, nil))
	log, shutdown, err := ExportLogs(context.Background(), "", "v", base)
	if err != nil || log != base {
		t.Fatalf("no endpoint must change nothing: log=%p base=%p err=%v", log, base, err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A collector that is gone costs records, never time: emit does not block on
// the network, the queue is bounded, and shutdown gives up when told to.
func TestExportLogsNeverBlocksOnADeadCollector(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	_ = ln.Close() // nothing listens here any more

	var local bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&local, nil))
	log, shutdown, err := ExportLogs(context.Background(), dead, "v", base)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < 3*logExportQueue; i++ { // three queues' worth: the overflow must be dropped, not waited for
		log.Info("still serving", "i", i)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("emitting %d records with the collector down took %s — emit must not wait on the sink", 3*logExportQueue, took)
	}
	if n := strings.Count(local.String(), "still serving"); n != 3*logExportQueue {
		t.Errorf("the local handler saw %d of %d records", n, 3*logExportQueue)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = shutdown(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown outlived its context with the collector down")
	}
}
