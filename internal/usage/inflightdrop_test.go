package usage

import (
	"testing"
	"time"
)

// PLAN T32.63: a record the cap drops while it rides the batch being sent is
// not lost when that batch lands, so it must not be counted as dropped — the
// control plane then counted it both delivered and missing (40 such records in
// the CP's release check, 2026-11-08). A batch that fails loses them, and
// counts them then.
func TestARecordDroppedWhileItsBatchIsInFlightIsCountedOnlyIfTheBatchFails(t *testing.T) {
	fill := func() (*Queue, int64) {
		q := NewQueue(1, 0)
		rec := Record{TS: time.Unix(1, 0), Model: "m", APIKeyID: "k"}
		one := len(marshal(1, rec)) + itemOverhead
		q.cap = 10 * one // room for ten
		for i := 0; i < 10; i++ {
			q.Add(rec)
		}
		recs, last := q.peek(4 * one) // a batch of the oldest records
		if len(recs) == 0 {
			t.Fatal("empty batch")
		}
		for i := 0; i < 6; i++ { // six more: the six oldest leave, the batch's among them
			q.Add(rec)
		}
		return q, last
	}
	inBatch := func(q *Queue) int64 { return q.heldDrops }

	q, last := fill()
	held := inBatch(q)
	if held == 0 || q.Stats().Dropped+held != 6 {
		t.Fatalf("six dropped: %d counted, %d held for the batch", q.Stats().Dropped, held)
	}
	q.ack(last) // the batch landed
	if got := q.Stats().Dropped; got != 6-held {
		t.Fatalf("after the batch landed: %d dropped, want %d (its own records were delivered)", got, 6-held)
	}

	q, _ = fill()
	held = inBatch(q)
	q.release() // the batch failed
	if got := q.Stats().Dropped; got != 6 {
		t.Fatalf("after the batch failed: %d dropped, want all 6", got)
	}
}
