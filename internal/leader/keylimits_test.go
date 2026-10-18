package leader

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod-sdk/adminapi"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

func limiterAt(t time.Time, fronts int, limits map[string]keyLimit) (*keyLimiter, *time.Time) {
	now := t
	l := newKeyLimiter(func() int { return fronts })
	l.now = func() time.Time { return now }
	l.set(limits)
	return l, &now
}

// T19.17: requests per minute is a bucket that refills over the minute; a
// refusal is 429 with how long to wait.
func TestRPMIsABucketThatRefills(t *testing.T) {
	l, now := limiterAt(time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), 0, map[string]keyLimit{"k": {rpm: 2}})
	for i := range 2 {
		if ref := l.admit("k", "m"); ref != nil {
			t.Fatalf("request %d fits a 2/min limit: %+v", i+1, ref)
		}
	}
	ref := l.admit("k", "m")
	if ref == nil || ref.status != http.StatusTooManyRequests || ref.code != "rate_limit_rpm" || ref.retryAfter != 30 {
		t.Fatalf("the third waits 30 s for one request to refill: %+v", ref)
	}
	*now = now.Add(30 * time.Second)
	if l.admit("k", "m") != nil {
		t.Fatal("30 s later one request has refilled")
	}
	if l.admit("other", "m") != nil {
		t.Fatal("a key with no limit is not metered")
	}
}

// Each replica takes limit ÷ fronts of a per-minute limit.
func TestAPerMinuteLimitIsSharedWithTheDoors(t *testing.T) {
	l, _ := limiterAt(time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), 2, map[string]keyLimit{"k": {rpm: 4}})
	for i := range 2 {
		if ref := l.admit("k", "m"); ref != nil {
			t.Fatalf("request %d: half of 4 is 2: %+v", i+1, ref)
		}
	}
	if l.admit("k", "m") == nil {
		t.Fatal("the third is over this replica's share")
	}
}

// Tokens per minute are taken when a response says how many; the next request
// is refused while the bucket is empty.
func TestTPMIsChargedByTheResponse(t *testing.T) {
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	l, now := limiterAt(at, 0, map[string]keyLimit{"k": {tpm: 600}})
	if l.admit("k", "m") != nil {
		t.Fatal("a full bucket admits")
	}
	l.record("k", 900, at)
	if ref := l.admit("k", "m"); ref == nil || ref.code != "rate_limit_tpm" || ref.retryAfter != 31 {
		t.Fatalf("300 tokens in debt at 10/s: about 31 s: %+v", ref)
	}
	*now = now.Add(31 * time.Second)
	if l.admit("k", "m") != nil {
		t.Fatal("refilled")
	}
}

// The daily quota counts every token of the UTC day — a store seed, the
// manager's count and the responses — and waits until midnight; the next day
// starts at zero, and the manager's count for another day is ignored.
func TestTheDailyQuotaCountsTheDayAndWaitsForMidnight(t *testing.T) {
	at := time.Date(2026, 10, 17, 22, 0, 0, 0, time.UTC)
	l, now := limiterAt(at, 0, map[string]keyLimit{"k": {quota: 1000, spent: 300, day: "2026-10-17"}})
	// The manager's 300 and this leader's count are the same tokens seen twice,
	// so the larger counts, never the sum (a replaced pod may under-count).
	l.seed("k", 200)
	if ref := l.admit("k", "m"); ref != nil {
		t.Fatalf("max(200, 300) = 300 of 1000: %+v", ref)
	}
	l.record("k", 600, at) // 800 counted here
	if ref := l.admit("k", "m"); ref != nil {
		t.Fatalf("800 of 1000: %+v", ref)
	}
	l.record("k", 200, at)
	ref := l.admit("k", "m")
	if ref == nil || ref.code != "quota_daily" || ref.retryAfter != 7200 {
		t.Fatalf("1000 of 1000: refused until midnight, 2 h: %+v", ref)
	}
	*now = time.Date(2026, 10, 18, 0, 0, 1, 0, time.UTC)
	if ref := l.admit("k", "m"); ref != nil {
		t.Fatalf("a new UTC day; the manager's count was for yesterday: %+v", ref)
	}
}

// A door's count restarts from the leader's, which includes the door's pushed
// rows; the leader publishes every quota'd key's day.
func TestADoorTakesTheLeadersCount(t *testing.T) {
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	door, _ := limiterAt(at, 0, map[string]keyLimit{"k": {quota: 1000}})
	door.record("k", 400, at)
	door.setRemote("2026-10-17", map[string]int64{"k": 1000})
	if ref := door.admit("k", "m"); ref == nil || ref.code != "quota_daily" {
		t.Fatalf("the leader counts 1000: refused: %+v", ref)
	}
	leader, _ := limiterAt(at, 0, map[string]keyLimit{"k": {quota: 1000}, "free": {rpm: 5}})
	leader.record("k", 250, at)
	day, keys := leader.todayByKey()
	if day != "2026-10-17" || keys["k"] != 250 || len(keys) != 1 {
		t.Fatalf("only quota'd keys, with their day: %s %v", day, keys)
	}
}

// A model off the key's allowlist is 403, and hidden from /v1/models.
func TestTheAllowlistIsAForbiddenAndAFilter(t *testing.T) {
	l, _ := limiterAt(time.Now(), 0, map[string]keyLimit{"k": keyLimitOf(adminKey("qwen", " llama "))})
	if ref := l.admit("k", "gpt-oss"); ref == nil || ref.status != http.StatusForbidden || ref.code != "model_not_allowed" {
		t.Fatalf("off the list: %+v", ref)
	}
	if l.admit("k", "llama") != nil || !l.allowedModel("k", "qwen") || l.allowedModel("k", "gpt-oss") || !l.allowedModel("nolimit", "x") {
		t.Fatal("on the list, and the filter agrees")
	}
}

// failingEngine answers a chat at once with an error: the request got past
// every limit, which is all the test asks (the shared stub's nil stream would
// block the handler forever).
type failingEngine struct{ stubLeaderEngine }

func (failingEngine) Chat(context.Context, engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	return nil, errors.New("no engine in this test")
}

func adminKey(models ...string) adminapi.SnapshotKey {
	return adminapi.SnapshotKey{ID: "k", AllowedModels: models}
}

// Through the leader's own /v1 stack: the snapshot's limits refuse before any
// engine is asked, the refusal is a usage row, and /v1/models hides what the key
// may not use.
func TestKeyLimitsRefuseThroughTheLeader(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &failingEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	plain := "limit-test-plaintext-key"
	snap := &AuthSnapshot{Revision: "r1", RequireKeys: true, Keys: []SnapshotKey{
		{ID: "a", Name: "team-a", Hash: auth.Hash(plain), RPMLimit: 1, AllowedModels: []string{"m"}},
	}}
	if err := srv.applyAuthSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	post := func(model string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+plain)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := post("other"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("a model off the list: %d", r.StatusCode)
	}
	if r := post("m"); r.StatusCode == http.StatusTooManyRequests || r.StatusCode == http.StatusForbidden {
		t.Fatalf("the first request is not refused by a limit: %d", r.StatusCode)
	}
	r := post("m")
	if r.StatusCode != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("the second in a minute is 429 with Retry-After: %d %q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	rows, err := st.Usage().Recent(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]int{}
	for _, row := range rows {
		outcomes[row.Outcome]++
	}
	if outcomes["model_not_allowed"] != 1 || outcomes["rate_limit_rpm"] != 1 {
		t.Fatalf("each refusal is a usage row: %v", outcomes)
	}
}
