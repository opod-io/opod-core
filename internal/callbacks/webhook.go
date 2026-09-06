package callbacks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/opod-io/opod/internal/httpsafe"
	"github.com/opod-io/opod/internal/metrics"
)

// Webhook is a generic JSON POST sink. Payload is the standard
// Event struct, signed with HMAC-SHA256 over the body when Secret is
// set; the signature lands in `X-Opod-Signature`.
//
// Retries: 4 attempts (the first immediate, then 250ms / 500ms / 1s
// backoff) for transient failures (5xx or transport error). Anything
// else (4xx, malformed URL) is dropped after the first attempt — no
// point burning the queue on a permanent receiver-side bug.
type Webhook struct {
	id     string
	url    string
	secret string
	events map[string]bool
	queue  chan Event
	log    *slog.Logger
	wg     sync.WaitGroup
	stop   chan struct{}
	once   sync.Once
	ctx    context.Context // worker lifetime — cancelled when stop fires
	cancel context.CancelFunc
	client *http.Client
}

// WebhookConfig captures the YAML row.
type WebhookConfig struct {
	ID                  string   // logical name; "webhook" if blank
	URL                 string   // required
	Secret              string   // optional — when set, payloads are HMAC-signed
	Events              []string // empty/nil = all kinds; otherwise filter
	QueueSz             int      // events to buffer; 0 = 100
	BlockPrivateTargets bool     // refuse to POST to private/loopback addresses
}

// NewWebhook returns a started Webhook sink. The worker goroutine is
// kicked off here so Send doesn't have to special-case "not started
// yet".
func NewWebhook(cfg WebhookConfig, log *slog.Logger) *Webhook {
	if cfg.ID == "" {
		cfg.ID = "webhook"
	}
	queueSz := cfg.QueueSz
	if queueSz <= 0 {
		queueSz = 100
	}
	w := &Webhook{
		id:     cfg.ID,
		url:    cfg.URL,
		secret: cfg.Secret,
		queue:  make(chan Event, queueSz),
		log:    log,
		stop:   make(chan struct{}),
		client: httpsafe.NewClient(10*time.Second, cfg.BlockPrivateTargets),
	}
	w.ctx, w.cancel = context.WithCancel(context.Background())
	if len(cfg.Events) > 0 {
		w.events = make(map[string]bool, len(cfg.Events))
		for _, e := range cfg.Events {
			w.events[strings.ToLower(e)] = true
		}
	}
	w.wg.Add(1)
	go w.run()
	return w
}

func (w *Webhook) Name() string { return w.id }

func (w *Webhook) Subscribes(kind string) bool {
	if w == nil || w.url == "" {
		return false
	}
	if w.events == nil {
		return true // "all kinds" when no filter configured
	}
	return w.events[strings.ToLower(kind)]
}

// Send buffers `e` on the sink's queue and returns immediately. A
// full queue means we drop the event and count it — the hot path is
// never blocked.
func (w *Webhook) Send(_ context.Context, e Event) {
	select {
	case w.queue <- e:
		metrics.SetCallbackQueueDepth(w.id, len(w.queue))
	default:
		metrics.ObserveCallback(w.id, "dropped")
	}
}

func (w *Webhook) Close(ctx context.Context) error {
	w.once.Do(func() {
		close(w.stop)
		// Cancel the worker ctx too so an in-flight delivery's retry
		// backoff aborts instead of holding up shutdown.
		w.cancel()
	})
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Webhook) run() {
	defer w.wg.Done()
	for {
		select {
		case <-w.stop:
			// Empty whatever's left so queued events are counted rather
			// than silently lost. The worker ctx is already cancelled at
			// this point, so each event gets at most one fast attempt and
			// in-flight backoff aborts. Bounded by the queue's current
			// length so we can't loop forever if a producer keeps writing.
			n := len(w.queue)
			for i := 0; i < n; i++ {
				select {
				case e := <-w.queue:
					w.deliver(w.ctx, e)
				default:
					return
				}
			}
			return
		case e := <-w.queue:
			metrics.SetCallbackQueueDepth(w.id, len(w.queue))
			w.deliver(w.ctx, e)
		}
	}
}

func (w *Webhook) deliver(ctx context.Context, e Event) {
	body := MustMarshal(e)
	backoffs := []time.Duration{0, 250 * time.Millisecond, 500 * time.Millisecond, time.Second}
	for attempt, wait := range backoffs {
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				metrics.ObserveCallback(w.id, "cancelled")
				return
			}
		}
		ok, retriable := w.postOnce(ctx, body)
		if ok {
			metrics.ObserveCallback(w.id, "ok")
			return
		}
		if !retriable {
			metrics.ObserveCallback(w.id, "failed")
			return
		}
		if attempt == len(backoffs)-1 {
			metrics.ObserveCallback(w.id, "exhausted")
			return
		}
	}
}

// postOnce sends one HTTP POST. Returns (success, retriable). 5xx and
// transport errors are retriable; 4xx is not.
func (w *Webhook) postOnce(ctx context.Context, body []byte) (ok, retriable bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return false, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "opod/0.2 (callbacks)")
	if w.secret != "" {
		req.Header.Set("X-Opod-Signature", w.sign(body))
	}
	resp, err := w.client.Do(req)
	if err != nil {
		if w.log != nil {
			w.log.Warn("webhook send error", "sink", w.id, "err", err)
		}
		return false, true
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, false
	}
	if resp.StatusCode >= 500 {
		return false, true
	}
	if w.log != nil {
		w.log.Warn("webhook receiver rejected", "sink", w.id, "status", resp.StatusCode)
	}
	return false, false
}

// sign returns the lowercase-hex HMAC-SHA256 of `body` using
// w.secret. Receivers verify by recomputing and constant-time
// comparing.
func (w *Webhook) sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(w.secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature is a helper for tests and receiver implementations.
// Constant-time compare so a side channel can't leak the secret.
func VerifySignature(secret string, body []byte, header string) bool {
	if secret == "" || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(header))
}

// Ensure interface compliance.
var _ Sink = (*Webhook)(nil)

// ensure fmt stays imported if we add a debug call.
var _ = fmt.Sprintf
