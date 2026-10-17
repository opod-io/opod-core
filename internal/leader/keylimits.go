package leader

// Per-key limits (ADR-092 §8, PLAN T19.17): requests and tokens per minute, a
// daily token quota and a model allowlist, set by the manager on each key of
// the auth snapshot. ADR-077 removed the first version because its daily quota
// ran a SUM over the usage table before every request; this one touches memory
// only on the request path:
//
//   - RPM and TPM are token buckets per key. Every replica that serves keyed
//     traffic — this leader and each gateway replica — enforces its share of
//     the limit, limit ÷ doors, the divisor the spend snapshot already carries
//     and /gatewayz states (never below 1).
//   - The daily quota is a counter per key and UTC day. The leader counts every
//     token — its own responses and the rows its doors push — and starts from
//     the larger of one store sum at snapshot load and the manager's
//     `spentToday`, which survives a replaced pod. A door adds its own tokens to
//     the leader's count it last polled.
//   - The allowlist is a set on the key.
//
// A refusal is 429 with Retry-After naming the limit, or 403 for a model the key
// may not use, and it is a usage row like any other answer.

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opod-io/opod-sdk/adminapi"

	"github.com/opod-io/opod/internal/api"
	"github.com/opod-io/opod/internal/auth"
)

// keyLimit is what the snapshot set on one key.
type keyLimit struct {
	rpm, tpm int
	quota    int64
	allowed  map[string]bool // nil = every model
	spent    int64           // the manager's count for day
	day      string
}

func (k keyLimit) limited() bool {
	return k.rpm > 0 || k.tpm > 0 || k.quota > 0 || k.allowed != nil
}

// keyMeter is one key's running state on this replica.
type keyMeter struct {
	reqLevel, tokLevel float64 // bucket levels, refilled per second
	at                 time.Time
	day                string
	used               int64 // tokens on day counted here
	remote             int64 // the leader's count on day, as a door last polled it
}

// keyLimiter holds every key's limits and meters.
type keyLimiter struct {
	limits atomic.Pointer[map[string]keyLimit]
	// doors is the divisor of a per-minute limit: each replica takes
	// limit ÷ doors. A value below 1 counts as 1.
	doors func() int
	now   func() time.Time

	mu     sync.Mutex
	meters map[string]*keyMeter
}

func newKeyLimiter(doors func() int) *keyLimiter {
	return &keyLimiter{doors: doors, now: time.Now, meters: map[string]*keyMeter{}}
}

func utcDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// set swaps the limits in whole (each auth snapshot).
func (l *keyLimiter) set(m map[string]keyLimit) { l.limits.Store(&m) }

func (l *keyLimiter) all() map[string]keyLimit {
	if p := l.limits.Load(); p != nil {
		return *p
	}
	return nil
}

func (l *keyLimiter) limitOf(keyID string) (keyLimit, bool) {
	k, ok := l.all()[keyID]
	return k, ok && k.limited()
}

// share is this replica's fraction of a per-minute limit.
func (l *keyLimiter) share() float64 {
	d := 1
	if l.doors != nil {
		d = max(l.doors(), 1)
	}
	return 1 / float64(d)
}

// meter is the key's meter, refilled to now and rolled to today. Held under mu.
func (l *keyLimiter) meter(keyID string, k keyLimit, now time.Time) *keyMeter {
	m := l.meters[keyID]
	share := l.share()
	rpmCap, tpmCap := float64(k.rpm)*share, float64(k.tpm)*share
	if m == nil {
		m = &keyMeter{reqLevel: rpmCap, tokLevel: tpmCap, at: now, day: utcDay(now)}
		l.meters[keyID] = m
	}
	if dt := now.Sub(m.at).Seconds(); dt > 0 {
		m.reqLevel = math.Min(rpmCap, m.reqLevel+dt*rpmCap/60)
		m.tokLevel = math.Min(tpmCap, m.tokLevel+dt*tpmCap/60)
		m.at = now
	}
	if d := utcDay(now); d != m.day {
		m.day, m.used, m.remote = d, 0, 0
	}
	return m
}

// spentToday is the key's tokens today as this replica best knows them.
func (m *keyMeter) spentToday(k keyLimit) int64 {
	spent := m.used + m.remote
	if k.day == m.day && k.spent > spent {
		spent = k.spent
	}
	return spent
}

// limitRefusal is why a request may not proceed.
type limitRefusal struct {
	status     int
	code, msg  string
	retryAfter int // seconds; 0 = none
}

// admit decides one request of keyID for model. It takes one request from
// the RPM bucket; tokens are taken when the response says how many.
func (l *keyLimiter) admit(keyID, model string) *limitRefusal {
	k, ok := l.limitOf(keyID)
	if !ok {
		return nil
	}
	if k.allowed != nil && model != "" && !k.allowed[model] {
		return &limitRefusal{status: http.StatusForbidden, code: "model_not_allowed",
			msg: "this API key may not use model " + strconv.Quote(model)}
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.meter(keyID, k, now)
	if k.quota > 0 && m.spentToday(k) >= k.quota {
		next := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day()+1, 0, 0, 0, 0, time.UTC)
		return &limitRefusal{status: http.StatusTooManyRequests, code: "quota_daily",
			msg:        "this API key has used its daily token quota (" + strconv.FormatInt(k.quota, 10) + " tokens, UTC day)",
			retryAfter: int(math.Ceil(next.Sub(now).Seconds()))}
	}
	if k.tpm > 0 && m.tokLevel <= 0 {
		rate := float64(k.tpm) * l.share() / 60
		return &limitRefusal{status: http.StatusTooManyRequests, code: "rate_limit_tpm",
			msg:        "this API key is over its tokens-per-minute limit (" + strconv.Itoa(k.tpm) + ")",
			retryAfter: int(math.Ceil((1 - m.tokLevel) / rate))}
	}
	if k.rpm > 0 {
		if m.reqLevel < 1 {
			rate := float64(k.rpm) * l.share() / 60
			return &limitRefusal{status: http.StatusTooManyRequests, code: "rate_limit_rpm",
				msg:        "this API key is over its requests-per-minute limit (" + strconv.Itoa(k.rpm) + ")",
				retryAfter: int(math.Ceil((1 - m.reqLevel) / rate))}
		}
		m.reqLevel--
	}
	return nil
}

// record counts tokens a response used, for the key's TPM bucket and its day.
func (l *keyLimiter) record(keyID string, tokens int64, at time.Time) {
	if keyID == "" || tokens <= 0 {
		return
	}
	k, ok := l.limitOf(keyID)
	if !ok {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.meter(keyID, k, l.now())
	if utcDay(at) == m.day {
		m.used += tokens
	}
	m.tokLevel -= float64(tokens)
}

// seed starts a key's day from a count read once (the store, at snapshot load),
// never lowering what is already counted.
func (l *keyLimiter) seed(keyID string, tokens int64) {
	k, ok := l.limitOf(keyID)
	if !ok {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if m := l.meter(keyID, k, l.now()); tokens > m.used {
		m.used = tokens
	}
}

// todayByKey is every limited key's tokens today, for the spend snapshot.
func (l *keyLimiter) todayByKey() (string, map[string]int64) {
	now := l.now()
	out := map[string]int64{}
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, k := range l.all() {
		if k.quota <= 0 {
			continue
		}
		out[id] = l.meter(id, k, now).spentToday(k)
	}
	return utcDay(now), out
}

// setRemote takes the leader's per-key counts on a door: this door's own
// count restarts from them, since the leader's number includes every row the
// door has pushed.
func (l *keyLimiter) setRemote(day string, counts map[string]int64) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, n := range counts {
		k, ok := l.all()[id]
		if !ok {
			continue
		}
		m := l.meter(id, k, now)
		if m.day == day {
			m.remote, m.used = n, 0
		}
	}
}

// allowedModel says whether the key may use model (for /v1/models).
func (l *keyLimiter) allowedModel(keyID, model string) bool {
	k, ok := l.limitOf(keyID)
	return !ok || k.allowed == nil || k.allowed[model]
}

// keyLimitsMiddleware enforces the calling key's limits before the request
// reaches admission or an engine. A request with no key, or a key with no
// limit, passes untouched; the body is read once and kept for the handler.
func (s *Server) keyLimitsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := auth.KeyFrom(r.Context())
		if k == nil || r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := s.keyLimits.limitOf(k.ID); !ok {
			next.ServeHTTP(w, r)
			return
		}
		body, r2, err := api.RequestBody(r)
		if err != nil {
			next.ServeHTTP(w, r2)
			return
		}
		model := peekModel(body)
		if ref := s.keyLimits.admit(k.ID, model); ref != nil {
			if ref.retryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(ref.retryAfter))
			}
			s.openaiH.RecordRefusal(r2.Context(), protocolOf(r2.URL.Path), model, ref.code)
			writeJSONErrorCode(w, ref.status, ref.code, ref.msg)
			return
		}
		next.ServeHTTP(w, r2)
	})
}

func protocolOf(path string) string {
	switch {
	case strings.HasSuffix(path, "/embeddings"):
		return "embeddings"
	case strings.HasSuffix(path, "/rerank"):
		return "rerank"
	}
	return "openai"
}

// doorCount is the divisor of this endpoint's per-minute limits — the same
// number the spend snapshot divides a key's rate by: the leader counts the
// doors that push to it (never below 1), a door reads it from the snapshot it
// polls.
func (s *Server) doorCount() int {
	if s.isGateway() {
		if s.front == nil || s.front.spend == nil {
			return 1
		}
		_, _, doors, _ := s.front.spend.Age()
		return doors
	}
	if s.gateways == nil {
		return 1
	}
	d, _ := s.gateways.doors(time.Now())
	return d
}

// keyLimitOf reads one snapshot key's limits.
func keyLimitOf(sk adminapi.SnapshotKey) keyLimit {
	kl := keyLimit{rpm: sk.RPMLimit, tpm: sk.TPMLimit, quota: sk.QuotaDailyTokens, spent: sk.SpentToday, day: sk.Day}
	if len(sk.AllowedModels) > 0 {
		kl.allowed = map[string]bool{}
		for _, m := range sk.AllowedModels {
			if m = strings.TrimSpace(m); m != "" {
				kl.allowed[m] = true
			}
		}
	}
	return kl
}

// seedKeyQuotas starts each quota'd key's day from the store once, at snapshot
// load — off the request path. The store is a bounded ring, so it may count
// low; the manager's spentToday covers a replaced pod.
func (s *Server) seedKeyQuotas(ctx context.Context, limits map[string]keyLimit) {
	since := time.Now().UTC().Truncate(24 * time.Hour)
	for id, k := range limits {
		if k.quota <= 0 {
			continue
		}
		if n, err := s.store.Usage().SumTokensSince(ctx, id, since); err == nil {
			s.keyLimits.seed(id, n)
		}
	}
}
