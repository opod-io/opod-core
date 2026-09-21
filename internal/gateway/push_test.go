package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Recording usage must never fail a request that already succeeded, and a
// leader that will not take a batch must not stop the door serving. So a failed
// push LEAVES the rows queued and the retry is safe: the leader deduplicates by
// the id the door minted, which is minted once per request and not per attempt.
func TestAFailedPushKeepsTheRowsAndRetriesSafely(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []string
	var down bool
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if down {
			http.Error(w, "nope", http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Gateway string `json:"gateway"`
			Rows    []Row  `json:"rows"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Gateway != "gw-1" {
			t.Errorf("the door names itself so the leader can count its doors: %q", req.Gateway)
		}
		for _, row := range req.Rows {
			seen = append(seen, row.ID)
		}
		fmt.Fprint(w, `{"accepted":`+fmt.Sprint(len(req.Rows))+`,"duplicate":0}`)
	}))
	defer leader.Close()

	p := NewPusher(leader.URL, "tok", "gw-1")
	mu.Lock()
	down = true
	mu.Unlock()
	for i := range 3 {
		p.Add(Row{ID: fmt.Sprintf("r%d", i), TS: time.Now(), APIKeyID: "k1", PromptTokens: 1, Outcome: "ok"})
	}
	if err := p.Flush(ctx, 100); err == nil {
		t.Error("a refused push must report the failure")
	}
	if q, sent, _, lastErr := p.Stats(); q != 3 || sent != 0 || lastErr == "" {
		t.Fatalf("a refused push keeps its rows and says why: queued=%d sent=%d err=%q", q, sent, lastErr)
	}

	mu.Lock()
	down = false
	mu.Unlock()
	if err := p.Flush(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if q, sent, dropped, _ := p.Stats(); q != 0 || sent != 3 || dropped != 0 {
		t.Fatalf("once taken, the rows leave the queue: queued=%d sent=%d dropped=%d", q, sent, dropped)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || seen[0] != "r0" {
		t.Errorf("the ids the door minted are what the leader deduplicates on: %v", seen)
	}
}

// A batch is only partly drained, and rows added while a push was in flight
// must not be dropped with the ones that were sent.
func TestFlushRemovesOnlyWhatItSent(t *testing.T) {
	ctx := context.Background()
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"accepted":2,"duplicate":0}`)
	}))
	defer leader.Close()
	p := NewPusher(leader.URL, "tok", "gw-1")
	for i := range 5 {
		p.Add(Row{ID: fmt.Sprintf("r%d", i), APIKeyID: "k1", Outcome: "ok"})
	}
	if err := p.Flush(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if q, sent, _, _ := p.Stats(); q != 3 || sent != 2 {
		t.Fatalf("two sent, three left: queued=%d sent=%d", q, sent)
	}
}

// A long outage must not grow without limit, and what it loses must be the
// OLDEST rows — the newest are the ones a daily quota still needs to be right
// about — and the loss must be counted, not silent.
func TestTheQueueIsBoundedAndDropsTheOldestCounted(t *testing.T) {
	p := NewPusher("http://127.0.0.1:1", "tok", "gw-1")
	for i := range QueueLimit + 10 {
		p.Add(Row{ID: fmt.Sprintf("r%d", i), APIKeyID: "k1", Outcome: "ok"})
	}
	q, _, dropped, _ := p.Stats()
	if q != QueueLimit {
		t.Errorf("queued = %d, want the bound %d", q, QueueLimit)
	}
	if dropped != 10 {
		t.Errorf("dropped = %d, want 10 counted", dropped)
	}
	p.mu.Lock()
	first := p.queue[0].ID
	p.mu.Unlock()
	if first != "r10" {
		t.Errorf("the oldest go first: the queue now starts at %s, want r10", first)
	}
}
