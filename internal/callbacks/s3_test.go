package callbacks

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeS3 records every PutObject so a test can assert on object keys
// and body contents without a live bucket.
type fakeS3 struct {
	mu   sync.Mutex
	puts []putRecord
}

type putRecord struct {
	bucket string
	key    string
	body   string
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(in.Body)
	f.puts = append(f.puts, putRecord{
		bucket: derefStr(in.Bucket),
		key:    derefStr(in.Key),
		body:   string(b),
	})
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) records() []putRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]putRecord, len(f.puts))
	copy(out, f.puts)
	return out
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// newTestS3 builds a sink with the worker stopped-ready and a fake
// client injected, so no AWS config load happens.
func newTestS3(cfg S3Config, fake s3PutObjectAPI) *S3 {
	if cfg.Bucket == "" {
		cfg.Bucket = "test-bucket"
	}
	s := NewS3(cfg, nil)
	s.clientMu.Lock()
	s.client = fake
	s.clientMu.Unlock()
	return s
}

func TestS3_FlushOnBatchSize(t *testing.T) {
	fake := &fakeS3{}
	s := newTestS3(S3Config{BatchSize: 3, FlushSeconds: 3600, Prefix: "logs"}, fake)
	defer s.Close(context.Background())

	for i := 0; i < 3; i++ {
		s.Send(context.Background(), Event{Kind: "usage", Payload: map[string]any{"i": i}})
	}

	waitFor(t, func() bool { return len(fake.records()) == 1 })

	rec := fake.records()[0]
	if rec.bucket != "test-bucket" {
		t.Fatalf("bucket = %q", rec.bucket)
	}
	// prefix normalized with a trailing slash, date-partitioned, .jsonl
	if !strings.HasPrefix(rec.key, "logs/") || !strings.HasSuffix(rec.key, ".jsonl") {
		t.Fatalf("unexpected key %q", rec.key)
	}
	lines := strings.Split(strings.TrimRight(rec.body, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 NDJSON lines, got %d: %q", len(lines), rec.body)
	}
	for _, ln := range lines {
		if !strings.Contains(ln, `"kind":"usage"`) {
			t.Fatalf("line missing kind: %q", ln)
		}
	}
}

func TestS3_FlushOnClose(t *testing.T) {
	fake := &fakeS3{}
	s := newTestS3(S3Config{BatchSize: 100, FlushSeconds: 3600}, fake)

	s.Send(context.Background(), Event{Kind: "audit", Payload: map[string]any{"a": 1}})
	s.Send(context.Background(), Event{Kind: "audit", Payload: map[string]any{"a": 2}})

	// Below batch size and well under the flush interval — only Close
	// should trigger the upload.
	if got := len(fake.records()); got != 0 {
		t.Fatalf("expected no flush before close, got %d", got)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	recs := fake.records()
	if len(recs) != 1 {
		t.Fatalf("expected 1 object after close, got %d", len(recs))
	}
	if n := strings.Count(recs[0].body, "\n"); n != 2 {
		t.Fatalf("expected 2 buffered events flushed, got %d lines", n)
	}
}

func TestS3_SubscribesFilter(t *testing.T) {
	s := NewS3(S3Config{Bucket: "b", Events: []string{"usage"}}, nil)
	defer s.Close(context.Background())
	if !s.Subscribes("usage") {
		t.Fatal("should subscribe to usage")
	}
	if s.Subscribes("audit") {
		t.Fatal("should not subscribe to audit when filtered")
	}

	all := NewS3(S3Config{Bucket: "b"}, nil)
	defer all.Close(context.Background())
	if !all.Subscribes("audit") || !all.Subscribes("usage") {
		t.Fatal("unfiltered sink should subscribe to all kinds")
	}

	inert := NewS3(S3Config{}, nil) // no bucket
	defer inert.Close(context.Background())
	if inert.Subscribes("usage") {
		t.Fatal("bucket-less sink must be inert")
	}
}

func TestS3_ObjectKeyUniqueAndPartitioned(t *testing.T) {
	s := NewS3(S3Config{Bucket: "b", Prefix: "p/"}, nil)
	defer s.Close(context.Background())
	now := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)
	k1 := s.objectKey(now)
	k2 := s.objectKey(now)
	if k1 == k2 {
		t.Fatalf("keys must be unique, got %q twice", k1)
	}
	if !strings.HasPrefix(k1, "p/2026/03/07/") {
		t.Fatalf("key not date-partitioned: %q", k1)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
