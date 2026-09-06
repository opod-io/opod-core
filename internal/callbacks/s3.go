package callbacks

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/opod-io/opod/internal/metrics"
)

// S3 ships events to an S3 (or S3-compatible: MinIO, R2, GCS-interop)
// bucket as newline-delimited JSON. Events are buffered and flushed in
// batches — one object per flush — so a high request rate doesn't turn
// into one PutObject per request. A flush fires when the batch fills
// (BatchSize), the flush interval elapses (FlushSeconds), or the sink
// is closed.
//
// Object keys are date-partitioned for easy lifecycle rules and Athena
// / cheap querying:
//
//	<prefix>YYYY/MM/DD/opod-<unixNano>-<seq>.jsonl
//
// The AWS client is built lazily on the first flush (inside the worker
// goroutine, never the hot path or startup) so a misconfigured default
// credential chain can't stall server boot. Credentials come from the
// explicit AccessKeyID/SecretAccessKey when set, otherwise the standard
// AWS chain (env, shared config, IMDS) — the same chain Bedrock egress
// uses.
type S3 struct {
	id            string
	bucket        string
	region        string
	prefix        string
	endpoint      string
	accessKey     string
	secretKey     string
	batchSize     int
	flushInterval time.Duration
	events        map[string]bool
	queue         chan Event
	log           *slog.Logger
	wg            sync.WaitGroup
	stop          chan struct{}
	once          sync.Once
	seq           atomic.Uint64

	clientMu sync.Mutex
	client   s3PutObjectAPI // lazily built; injectable in tests
}

// s3PutObjectAPI is the slice of the S3 client the sink needs. Narrow
// interface so tests can inject a fake without a live bucket.
type s3PutObjectAPI interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// S3Config captures the YAML row.
type S3Config struct {
	ID              string   // logical name; "s3" if blank
	Bucket          string   // required
	Region          string   // default "us-east-1"
	Prefix          string   // key prefix; "/" appended if missing
	Endpoint        string   // S3-compatible base endpoint; empty = AWS
	AccessKeyID     string   // optional — else default AWS chain
	SecretAccessKey string   // optional — else default AWS chain
	Events          []string // empty/nil = all kinds
	BatchSize       int      // events per object; 0 = 100
	FlushSeconds    int      // max batch age before flush; 0 = 30
	QueueSz         int      // events to buffer; 0 = 1000
}

const (
	s3DefaultBatchSize    = 100
	s3DefaultFlushSeconds = 30
	s3DefaultQueueSz      = 1000
	s3UploadTimeout       = 30 * time.Second
)

// NewS3 returns a started S3 sink. Like the other sinks the worker is
// kicked off here; if Bucket is blank the sink reports Subscribes=false
// for every kind so Send is never called.
func NewS3(cfg S3Config, log *slog.Logger) *S3 {
	if cfg.ID == "" {
		cfg.ID = "s3"
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1" // SDK requires one; most S3-compat ignore it
	}
	prefix := cfg.Prefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = s3DefaultBatchSize
	}
	flushSecs := cfg.FlushSeconds
	if flushSecs <= 0 {
		flushSecs = s3DefaultFlushSeconds
	}
	queueSz := cfg.QueueSz
	if queueSz <= 0 {
		queueSz = s3DefaultQueueSz
	}
	s := &S3{
		id:            cfg.ID,
		bucket:        cfg.Bucket,
		region:        region,
		prefix:        prefix,
		endpoint:      cfg.Endpoint,
		accessKey:     cfg.AccessKeyID,
		secretKey:     cfg.SecretAccessKey,
		batchSize:     batchSize,
		flushInterval: time.Duration(flushSecs) * time.Second,
		queue:         make(chan Event, queueSz),
		log:           log,
		stop:          make(chan struct{}),
	}
	if len(cfg.Events) > 0 {
		s.events = make(map[string]bool, len(cfg.Events))
		for _, e := range cfg.Events {
			s.events[strings.ToLower(e)] = true
		}
	}
	s.wg.Add(1)
	go s.run()
	return s
}

func (s *S3) Name() string { return s.id }

func (s *S3) Subscribes(kind string) bool {
	if s == nil || s.bucket == "" {
		return false
	}
	if s.events == nil {
		return true // "all kinds" when no filter configured
	}
	return s.events[strings.ToLower(kind)]
}

// Send buffers e on the queue and returns immediately. A full queue
// drops the event and counts it — the hot path is never blocked.
func (s *S3) Send(_ context.Context, e Event) {
	select {
	case s.queue <- e:
		metrics.SetCallbackQueueDepth(s.id, len(s.queue))
	default:
		metrics.ObserveCallback(s.id, "dropped")
	}
}

func (s *S3) Close(ctx context.Context) error {
	s.once.Do(func() { close(s.stop) })
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *S3) run() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()

	batch := make([]Event, 0, s.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.upload(batch)
		batch = batch[:0]
	}

	for {
		select {
		case <-s.stop:
			// Drain whatever's queued into the batch, then flush once so
			// buffered events aren't lost on shutdown. Bounded by the
			// queue's current length so a producer racing Close can't
			// spin us forever.
			n := len(s.queue)
			for i := 0; i < n; i++ {
				select {
				case e := <-s.queue:
					batch = append(batch, e)
				default:
					i = n
				}
			}
			flush()
			return
		case e := <-s.queue:
			metrics.SetCallbackQueueDepth(s.id, len(s.queue))
			batch = append(batch, e)
			if len(batch) >= s.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// upload writes one batch as an NDJSON object. The SDK applies its own
// transient-error retries, so a single PutObject is enough; on failure
// the batch is dropped (and counted) rather than retried here, matching
// the "never block, drop-and-count" contract of the other sinks.
func (s *S3) upload(batch []Event) {
	ctx, cancel := context.WithTimeout(context.Background(), s3UploadTimeout)
	defer cancel()

	client, err := s.ensureClient(ctx)
	if err != nil {
		if s.log != nil {
			s.log.Warn("s3 callback: client init failed", "sink", s.id, "err", err)
		}
		metrics.ObserveCallback(s.id, "failed")
		return
	}

	var buf bytes.Buffer
	for _, e := range batch {
		buf.Write(MustMarshal(e))
		buf.WriteByte('\n')
	}

	key := s.objectKey(time.Now().UTC())
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(buf.Bytes()),
		ContentType: aws.String("application/x-ndjson"),
	})
	if err != nil {
		if s.log != nil {
			s.log.Warn("s3 callback: put failed", "sink", s.id, "bucket", s.bucket, "key", key, "err", err)
		}
		metrics.ObserveCallback(s.id, "failed")
		return
	}
	metrics.ObserveCallback(s.id, "ok")
}

// objectKey builds a date-partitioned, collision-free key. The atomic
// sequence disambiguates two flushes that land in the same nanosecond.
func (s *S3) objectKey(now time.Time) string {
	n := s.seq.Add(1)
	return fmt.Sprintf("%s%04d/%02d/%02d/opod-%d-%d.jsonl",
		s.prefix, now.Year(), now.Month(), now.Day(), now.UnixNano(), n)
}

func (s *S3) ensureClient(ctx context.Context) (s3PutObjectAPI, error) {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	if s.client != nil {
		return s.client, nil
	}
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(s.region)}
	if s.accessKey != "" && s.secretKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(s.accessKey, s.secretKey, "")))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	s.client = s3.NewFromConfig(cfg, func(o *s3.Options) {
		if s.endpoint != "" {
			// S3-compatible stores (MinIO, R2, GCS) need an explicit
			// endpoint and almost always path-style addressing.
			o.BaseEndpoint = aws.String(s.endpoint)
			o.UsePathStyle = true
		}
	})
	return s.client, nil
}

// Ensure interface compliance.
var _ Sink = (*S3)(nil)
