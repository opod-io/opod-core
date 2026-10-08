package batch

// Telling a cancel from an interruption (see the package comment).
//
// The runner is stopped the same way in both cases — SIGTERM — so the signal
// cannot carry the difference. What can is the batch object the runner's
// caller keeps: OpenAI's batch API serves GET /v1/batches/{id}, and its status
// says whether the batch still wants a runner ("in_progress") or was asked to
// stop ("cancelling", or any end). That is OpenAI's own surface on the same
// files base the runner already reads its input from and uploads to, so the
// runner still knows no control plane: it asks one OpenAI question, once, at
// the moment it is stopped. A marker file in the work directory would need the
// caller to reach the runner's volume; a polled flag would cost a request every
// few seconds for an answer needed once.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
)

// CancelRequested reports whether the OpenAI batch object at statusURL (GET
// …/v1/batches/{id}, read with key) says the batch was asked to stop.
//
//   - status "validating" or "in_progress": no — the batch still wants this
//     runner, so the stop was an interruption.
//   - any other status ("cancelling", "finalizing", "cancelled", "expired",
//     "completed", "failed"): yes — the batch's owner ended it.
//   - 401, 403 or 404: yes — the key no longer opens the batch (its keeper
//     revokes the key when the batch ends) or the batch is gone; a restart
//     could do nothing but fail.
//   - anything else, or no answer: no. A cancel that cannot be read is never
//     assumed, because assuming one ends a half-sent batch as finished; the
//     interruption it becomes is resumed and costs at most a restart.
func CancelRequested(ctx context.Context, client *http.Client, statusURL, key string) bool {
	if statusURL == "" {
		return false
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return false
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false
	}
	var obj struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&obj); err != nil || obj.Status == "" {
		return false
	}
	return obj.Status != "validating" && obj.Status != "in_progress"
}

// Signals is the context `opod batch run` runs under: it ends on SIGINT or
// SIGTERM, and the returned judge (Options.StopIsCancel) says which stop that
// was. SIGINT is a person at a terminal pressing Ctrl-C — an explicit cancel,
// and nothing in Kubernetes sends it. SIGTERM, and a parent context that
// ended, are judged by asked (CancelRequested against the batch object); with
// no asked they are interruptions. stop releases the signals.
func Signals(parent context.Context, asked func(context.Context) bool) (ctx context.Context, judge func(context.Context) bool, stop func()) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	var byHand atomic.Bool
	go func() {
		select {
		case s := <-ch:
			byHand.Store(s == os.Interrupt)
			cancel()
		case <-ctx.Done():
		}
	}()
	judge = func(c context.Context) bool {
		if byHand.Load() {
			return true
		}
		return asked != nil && asked(c)
	}
	return ctx, judge, func() { signal.Stop(ch); cancel() }
}

// ExitCode is the process exit status for a run that returned no error: 0 when
// the batch is over (completed, or cancelled on request), ExitInterrupted when
// it was stopped with lines unsent and must be run again on its directory.
func ExitCode(res Result) int {
	if res.Resumable {
		return ExitInterrupted
	}
	return 0
}
