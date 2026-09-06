package callbacks

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/opod-io/opod/internal/metrics"
)

// Langfuse ships events to Langfuse's /api/public/ingestion endpoint.
// Authenticates with Basic auth (public_key:secret_key).
//
// Only the "usage" event is meaningful — the Langfuse trace shape
// expects a generation/observation per request. Audit + fallback
// events are ignored by Subscribes (those land in the webhook or
// audit log instead).
type Langfuse struct {
	id        string
	host      string
	authBasic string
	queue     chan Event
	log       *slog.Logger
	stop      chan struct{}
	once      sync.Once
	wg        sync.WaitGroup
	client    *http.Client
	started   bool // true only when the worker goroutine is running
}

// LangfuseConfig captures the YAML row.
type LangfuseConfig struct {
	ID        string // logical name; "langfuse" if blank
	Host      string // default https://cloud.langfuse.com
	PublicKey string // pk-... — required
	SecretKey string // sk-... — required
	QueueSz   int    // events to buffer; 0 = 100
}

// NewLangfuse returns a started Langfuse sink. Panics aren't a
// concern: if PublicKey/SecretKey are blank the sink reports
// Subscribes=false for every event, so Send is never called.
func NewLangfuse(cfg LangfuseConfig, log *slog.Logger) *Langfuse {
	if cfg.ID == "" {
		cfg.ID = "langfuse"
	}
	host := cfg.Host
	if host == "" {
		host = "https://cloud.langfuse.com"
	}
	host = strings.TrimRight(host, "/")
	queueSz := cfg.QueueSz
	if queueSz <= 0 {
		queueSz = 100
	}
	auth := base64.StdEncoding.EncodeToString([]byte(cfg.PublicKey + ":" + cfg.SecretKey))
	l := &Langfuse{
		id:        cfg.ID,
		host:      host,
		authBasic: "Basic " + auth,
		queue:     make(chan Event, queueSz),
		log:       log,
		stop:      make(chan struct{}),
		client:    &http.Client{Timeout: 10 * time.Second},
	}
	// Only run a worker when BOTH credentials are present — otherwise the
	// sink has no consumer. Subscribes is gated on the same `started` flag
	// so a half-configured sink (e.g. only the public key set) reports
	// Subscribes=false instead of enqueueing events that nothing drains.
	if cfg.PublicKey != "" && cfg.SecretKey != "" {
		l.started = true
		l.wg.Add(1)
		go l.run()
	}
	return l
}

func (l *Langfuse) Name() string { return l.id }

func (l *Langfuse) Subscribes(kind string) bool {
	// Gate on `started`: with no running worker there is nothing to drain
	// the queue, so Send would silently fill it and drop every event. A
	// half-set credential pair (only one of public/secret) lands here.
	if l == nil || !l.started {
		return false
	}
	// We only translate usage events into Langfuse "generation"
	// observations. Audit / fallback noise would clutter the trace
	// view; route those through the webhook sink instead.
	return strings.ToLower(kind) == "usage"
}

func (l *Langfuse) Send(_ context.Context, e Event) {
	select {
	case l.queue <- e:
		metrics.SetCallbackQueueDepth(l.id, len(l.queue))
	default:
		metrics.ObserveCallback(l.id, "dropped")
	}
}

func (l *Langfuse) Close(ctx context.Context) error {
	l.once.Do(func() { close(l.stop) })
	done := make(chan struct{})
	go func() { l.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Langfuse) run() {
	defer l.wg.Done()
	for {
		select {
		case <-l.stop:
			n := len(l.queue)
			for i := 0; i < n; i++ {
				select {
				case e := <-l.queue:
					l.deliver(e)
				default:
					return
				}
			}
			return
		case e := <-l.queue:
			metrics.SetCallbackQueueDepth(l.id, len(l.queue))
			l.deliver(e)
		}
	}
}

// deliver translates a usage event into a Langfuse "generation"
// observation. The full ingestion API supports trace/span/event/score
// shapes; we ship the minimum that surfaces in the dashboard.
func (l *Langfuse) deliver(e Event) {
	p := e.Payload
	now := e.Timestamp.UTC().Format(time.RFC3339Nano)
	id, _ := p["request_id"].(string)
	if id == "" {
		id = "opod-" + now
	}
	model, _ := p["model"].(string)
	userID, _ := p["user_id"].(string)
	prompt, _ := p["prompt_tokens"].(float64)
	completion, _ := p["completion_tokens"].(float64)
	costUSD, _ := p["cost_usd"].(float64)

	batch := map[string]any{
		"batch": []map[string]any{
			{
				"id":        id,
				"timestamp": now,
				"type":      "generation-create",
				"body": map[string]any{
					"id":        id,
					"name":      "chat",
					"startTime": now,
					"endTime":   now,
					"model":     model,
					"userId":    userID,
					"input":     nil, // we don't ship prompt content by default — privacy
					"output":    nil,
					"usage": map[string]any{
						"input":     prompt,
						"output":    completion,
						"total":     prompt + completion,
						"unit":      "TOKENS",
						"totalCost": costUSD,
					},
					"metadata": map[string]any{
						"protocol": p["protocol"],
						"outcome":  p["outcome"],
					},
				},
			},
		},
	}
	body, _ := json.Marshal(batch)
	req, err := http.NewRequest(http.MethodPost, l.host+"/api/public/ingestion", bytes.NewReader(body))
	if err != nil {
		metrics.ObserveCallback(l.id, "failed")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", l.authBasic)
	req.Header.Set("User-Agent", "opod/0.2 (langfuse)")
	resp, err := l.client.Do(req)
	if err != nil {
		if l.log != nil {
			l.log.Warn("langfuse send error", "err", err)
		}
		metrics.ObserveCallback(l.id, "failed")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		metrics.ObserveCallback(l.id, "ok")
		return
	}
	if l.log != nil {
		l.log.Warn("langfuse rejected", "status", resp.StatusCode)
	}
	metrics.ObserveCallback(l.id, "failed")
}

var _ Sink = (*Langfuse)(nil)
