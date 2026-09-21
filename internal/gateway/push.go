package gateway

// The usage half (ADR-063): a door records a request locally and pushes it to
// the leader, which is the single writer the store was built for.
//
// The contract is at-least-once with a row id the DOOR mints, because only the
// door knows that two pushes are the same request. Fail-static: a leader that
// will not take a batch does not stop the door serving — the batch stays
// queued, bounded, and the oldest rows are the ones dropped if a long outage
// overruns the queue, because the newest are the ones a quota still needs.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// QueueLimit bounds what one door holds while the leader is away. At ~200
// bytes a row this is a few megabytes, and a door that has been cut off for
// long enough to fill it has a bigger problem than its usage rows.
const QueueLimit = 20_000

// Row is one usage fact with the id the door minted for it.
type Row struct {
	ID               string    `json:"id"`
	TS               time.Time `json:"ts"`
	APIKeyID         string    `json:"api_key_id"`
	UserID           string    `json:"user_id,omitempty"`
	Model            string    `json:"model"`
	Protocol         string    `json:"protocol,omitempty"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	LatencyMS        int       `json:"latency_ms"`
	TTFTMS           int       `json:"ttft_ms,omitempty"`
	Outcome          string    `json:"outcome"`
	NodeID           string    `json:"node_id,omitempty"`
}

// RowFrom builds a push row from a recorded usage fact. id must be unique for
// this request across retries — mint it once, when the request is recorded,
// never per attempt.
func RowFrom(id string, u store.Usage) Row {
	return Row{ID: id, TS: u.TS, APIKeyID: u.APIKeyID, UserID: u.UserID, Model: u.Model, Protocol: u.Protocol,
		PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens,
		LatencyMS: u.LatencyMS, TTFTMS: u.TTFTMS, Outcome: u.Outcome, NodeID: u.NodeID}
}

// Pusher queues rows and sends them to the leader in batches.
type Pusher struct {
	leaderURL string
	token     string
	gateway   string
	http      *http.Client

	mu      sync.Mutex
	queue   []Row
	dropped int64
	lastErr string
	sent    int64
}

func NewPusher(leaderURL, token, gatewayID string) *Pusher {
	return &Pusher{leaderURL: leaderURL, token: token, gateway: gatewayID,
		http: &http.Client{Timeout: 15 * time.Second}}
}

// Add queues a row. It never blocks and never fails: recording usage must not
// be able to fail a request that already succeeded.
func (p *Pusher) Add(r Row) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue = append(p.queue, r)
	if len(p.queue) > QueueLimit {
		// Drop the OLDEST: the newest rows are the ones a daily quota still
		// needs to be right about, and the loss is counted rather than silent.
		over := len(p.queue) - QueueLimit
		p.queue = p.queue[over:]
		p.dropped += int64(over)
	}
}

// Stats is what the door reports about its own backlog — the only symptom a
// queue that is not draining otherwise has.
func (p *Pusher) Stats() (queued int, sent, dropped int64, lastErr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue), p.sent, p.dropped, p.lastErr
}

// Flush sends up to batch rows. Rows are removed only once the leader has
// taken them: a failed push leaves them queued, and the leader deduplicates a
// row it has already recorded, so a retry after an unseen response is safe.
func (p *Pusher) Flush(ctx context.Context, batch int) error {
	p.mu.Lock()
	if len(p.queue) == 0 {
		p.mu.Unlock()
		return nil
	}
	n := min(batch, len(p.queue))
	send := make([]Row, n)
	copy(send, p.queue[:n])
	p.mu.Unlock()

	body, err := json.Marshal(struct {
		Gateway string `json:"gateway"`
		Rows    []Row  `json:"rows"`
	}{Gateway: p.gateway, Rows: send})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.leaderURL+"/admin/v1/usage/push", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := p.http.Do(req)
	if err != nil {
		p.note(err.Error())
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e := fmt.Sprintf("leader answered %s", resp.Status)
		p.note(e)
		return fmt.Errorf("%s", e)
	}
	// Taken. Drop exactly the rows that were sent — more may have been queued
	// while the request was in flight.
	p.mu.Lock()
	p.queue = p.queue[n:]
	p.sent += int64(n)
	p.lastErr = ""
	p.mu.Unlock()
	return nil
}

func (p *Pusher) note(err string) {
	p.mu.Lock()
	p.lastErr = err
	p.mu.Unlock()
}

// Run flushes every interval until ctx is done, then makes one last attempt so
// a clean shutdown does not strand the rows it already has.
func (p *Pusher) Run(ctx context.Context, every time.Duration, batch int) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			last, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_ = p.Flush(last, batch)
			return
		case <-t.C:
			_ = p.Flush(ctx, batch)
		}
	}
}
