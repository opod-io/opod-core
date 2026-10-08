// Package batch is a generic OpenAI batch client: it reads an input file in
// OpenAI's batch line format, sends every line to an OpenAI-compatible server
// at bounded concurrency, and writes the answers in OpenAI's batch output
// shape — one output file for the answers that succeeded, one error file for
// the rest. `opod batch run` is its command.
//
// It knows no store, no server and no control plane. What drives it gives it
// a base URL, a key, an input and a work directory; whoever that is decides
// where the result files go afterwards (--output-url / --error-url PUT them to
// URLs that carry their own authorization, such as an object store's
// presigned URLs).
//
// # The work directory is the progress record
//
// A run can be killed at any instant — a pod is evicted, a node reboots — and
// is resumed by running it again on the same directory. Three files there are
// the whole record:
//
//	sent.jsonl    one {"line":N} per input line, appended and fsynced BEFORE
//	              that line's request leaves this process
//	output.jsonl  one OpenAI output object per answered line (2xx)
//	errors.jsonl  one per line that failed: a non-2xx answer, a transport
//	              error, a malformed input line, or a line in flight at a kill
//
// Every result object's id is "batch_req_<line>", so a resume knows which lines
// are finished from the result files alone. A line in sent.jsonl with no result
// was IN FLIGHT when the previous run died: its request may or may not have
// reached the server, and nothing on this side can tell which.
//
// # At most once, on purpose
//
// Such a line is NOT sent again. It is written to the error file with code
// "interrupted" and the run moves on. That is at-most-once delivery: resending
// would be at-least-once, and an inference request that already ran on the
// server would run — and be billed, and be counted against the endpoint's
// capacity — a second time, with the first answer lost anyway. Losing the one
// answer per kill that was in flight, and saying so by name in the error file,
// is the cheaper and the honest failure. A caller who wants those lines retried
// collects their custom_ids from the error file and submits them again.
//
// The guarantee holds as long as the work directory survives the kill: a
// directory that is lost with its pod takes the record with it, and a run on an
// empty directory starts from line 0.
//
// # Cancel, and a stop that is not one
//
// Cancelling the context stops dispatch at the next line: no new line is sent.
// Requests already in flight get Options.Drain to finish; any still running
// after that are aborted and written to the error file. Lines never sent
// appear in neither file.
//
// A stopped context is not by itself a request to cancel. Kubernetes sends the
// same SIGTERM to a pod it evicts, drains off a node or that someone deletes by
// hand as to one whose batch was cancelled, and treating every SIGTERM as a
// cancel ends a half-sent batch as if it were finished. So a stop is judged
// once, while the in-flight lines drain, by Options.StopIsCancel:
//
//   - a CANCEL (StopIsCancel nil or true): the aborted lines are "cancelled",
//     Result.Cancelled is set, and the batch is over — what finished is the
//     result.
//   - an INTERRUPTION (StopIsCancel false): the aborted lines are
//     "interrupted", and when lines remain unsent Result.Resumable is set — the
//     run is to be started again on the same directory, where it continues with
//     the lines it never sent and sends none twice. `opod batch run` exits
//     ExitInterrupted then, so a Job restarts it.
package batch

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// File names inside the work directory.
const (
	InputFile  = "input.jsonl"
	SentFile   = "sent.jsonl"
	OutputFile = "output.jsonl"
	ErrorFile  = "errors.jsonl"
)

// maxLine bounds one input line. OpenAI caps a whole batch input at 200 MB;
// one request body near that size is not a chat request.
const maxLine = 64 << 20

// maxResponse bounds one answer read into memory.
const maxResponse = 64 << 20

// Options is one run.
type Options struct {
	// Input is the batch input: a local path, or an http(s) URL read with Key
	// (an OpenAI files content URL) — or, with InputPresigned, read with no
	// credential at all. A URL is downloaded once into the work directory, so a
	// resume reads the same bytes in the same order.
	Input string
	// InputPresigned marks an Input URL that carries its own authorization (a
	// presigned or signed URL): the Key is NOT sent with it, since a store
	// refuses a request that carries two credentials.
	InputPresigned bool
	// BaseURL is the OpenAI-compatible server, with or without its /v1: a line's
	// url "/v1/chat/completions" is joined to it without doubling the /v1.
	BaseURL string
	// Key is sent as `Authorization: Bearer` on every request to BaseURL, and
	// on the input download unless InputPresigned.
	Key string
	// Dir is the work directory: the progress record and the result files.
	Dir string
	// Concurrency bounds the requests in flight (default 8).
	Concurrency int
	// ModelMap rewrites a line's `model` when it is a key of the map. Lines
	// naming any other model are sent as written, and the server's answer —
	// a 404 for a model it does not serve — lands in the error file.
	ModelMap map[string]string
	// CA, when set, is the only certificate an https BaseURL is trusted by
	// (PEM). The input download is not held to it (FilesClient).
	CA []byte
	// RequestTimeout bounds one request (default 10 minutes).
	RequestTimeout time.Duration
	// ReadyTimeout, when > 0, is how long the run waits before its first line
	// for GET <base>/models to answer 2xx with Key: a key minted moments ago may
	// not have reached the server yet, and a server may be waking.
	ReadyTimeout time.Duration
	// Drain is how long in-flight requests may finish after a stop (default 20 s).
	Drain time.Duration
	// StopIsCancel judges a stop (see "Cancel, and a stop that is not one"):
	// called at most once, when the context ends before every line is answered,
	// with a context bounded by stopJudgeTimeout. nil = every stop is a cancel.
	StopIsCancel func(context.Context) bool
	// Progress, when set, is called with the counts at most every ProgressEvery.
	Progress      func(Counts)
	ProgressEvery time.Duration
	// Client overrides the HTTP client for BaseURL (tests).
	Client *http.Client
	// FilesClient reads an Input URL; nil = the system's trust roots. It is not
	// Client: an object store's certificate is not the server's CA.
	FilesClient *http.Client
	Log         *slog.Logger
}

// Counts are OpenAI's request_counts.
type Counts struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

// Result is how a run ended.
type Result struct {
	Counts
	// Cancelled is true when the context was cancelled before every line was
	// sent: the result files hold what finished.
	Cancelled bool
	// Interrupted counts the lines found in flight from a previous run and
	// written to the error file instead of being sent again.
	Interrupted int
	// Resumable is true when the run was stopped by something that was not a
	// cancel (Options.StopIsCancel said false) and lines remain unsent: the
	// batch is not finished, and a run on the same directory continues it.
	Resumable  bool
	OutputPath string
	ErrorPath  string
}

// Line is one input line in OpenAI's batch format.
type Line struct {
	CustomID string          `json:"custom_id"`
	Method   string          `json:"method"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

// OutputLine is one line of the output or error file, in OpenAI's shape.
type OutputLine struct {
	ID       string     `json:"id"`
	CustomID string     `json:"custom_id"`
	Response *Response  `json:"response"`
	Error    *LineError `json:"error"`
}

// Response is the server's answer to one line.
type Response struct {
	StatusCode int             `json:"status_code"`
	RequestID  string          `json:"request_id"`
	Body       json.RawMessage `json:"body"`
}

// LineError is a line that got no answer from the server.
type LineError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes this runner writes on its own (a server's answer keeps its own).
const (
	CodeInvalidLine = "invalid_request"
	CodeTransport   = "request_failed"
	CodeInterrupted = "interrupted"
	CodeCancelled   = "cancelled"
)

// ExitInterrupted is the exit status of `opod batch run` when Result.Resumable:
// EX_TEMPFAIL from sysexits(3), "try again later". It is non-zero on purpose —
// a Job counts a zero exit as the batch finished and never runs it again.
const ExitInterrupted = 75

// stopJudgeTimeout bounds Options.StopIsCancel: it runs inside the drain, and
// a judge that cannot answer must not eat the grace the upload needs.
const stopJudgeTimeout = 10 * time.Second

// reqID is the result id of input line n; lineOf reads it back.
func reqID(n int) string { return "batch_req_" + strconv.Itoa(n) }

func lineOf(id string) (int, bool) {
	s, ok := strings.CutPrefix(id, "batch_req_")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 0
}

// Run sends the batch, resuming from the work directory's record.
func Run(ctx context.Context, opt Options) (Result, error) {
	if opt.Dir == "" {
		return Result{}, errors.New("batch: a work directory is required")
	}
	if opt.BaseURL == "" {
		return Result{}, errors.New("batch: a base URL is required")
	}
	if opt.Concurrency <= 0 {
		opt.Concurrency = 8
	}
	if opt.RequestTimeout <= 0 {
		opt.RequestTimeout = 10 * time.Minute
	}
	if opt.Drain <= 0 {
		opt.Drain = 20 * time.Second
	}
	if opt.Log == nil {
		opt.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opt.Client == nil {
		c, err := newClient(opt.CA)
		if err != nil {
			return Result{}, err
		}
		opt.Client = c
	}
	if err := os.MkdirAll(opt.Dir, 0o750); err != nil {
		return Result{}, fmt.Errorf("batch: work directory: %w", err)
	}
	input, err := stageInput(ctx, opt)
	if err != nil {
		return Result{}, err
	}
	rec, err := openRecord(opt.Dir)
	if err != nil {
		return Result{}, err
	}
	defer rec.close()
	res := Result{OutputPath: rec.outPath, ErrorPath: rec.errPath}

	lines, err := readLines(input)
	if err != nil {
		return res, err
	}
	res.Total = len(lines)
	res.Completed, res.Failed = rec.okCount, rec.failCount

	// The previous run's in-flight lines: recorded as sent, never answered.
	// They are written as interrupted before anything new is sent (see the
	// package comment for why they are not sent again).
	for n := range rec.sent {
		if rec.finished[n] || n >= len(lines) {
			continue
		}
		if err := rec.result(n, OutputLine{ID: reqID(n), CustomID: lines[n].customID(), Error: &LineError{Code: CodeInterrupted,
			Message: "this line was in flight when a previous run stopped; it is not sent twice — submit it again if its answer is needed"}}, false); err != nil {
			return res, err
		}
		res.Failed++
		res.Interrupted++
	}

	if opt.ReadyTimeout > 0 {
		if err := waitReady(ctx, opt); err != nil {
			if ctx.Err() != nil {
				res.Cancelled = true
				if !judgeStop(opt) {
					res.Cancelled, res.Resumable = false, res.Completed+res.Failed < res.Total
				}
				return res, nil
			}
			return res, err
		}
	}

	var mu sync.Mutex // the counts; the record has its own lock
	report := func() {
		if opt.Progress == nil {
			return
		}
		mu.Lock()
		c := res.Counts
		mu.Unlock()
		opt.Progress(c)
	}
	stopTicker := make(chan struct{})
	if opt.Progress != nil && opt.ProgressEvery > 0 {
		go func() {
			t := time.NewTicker(opt.ProgressEvery)
			defer t.Stop()
			for {
				select {
				case <-stopTicker:
					return
				case <-t.C:
					report()
				}
			}
		}()
	}

	// Requests run under their own context: a stop ends DISPATCH at once, and
	// gives what is in flight Drain to finish before it is aborted too. The
	// stop is judged (cancel or interruption) inside that drain, before any
	// abort, so an aborted line is labelled by what the stop was.
	reqCtx, abort := context.WithCancel(context.WithoutCancel(ctx))
	defer abort()
	var cancelStop atomic.Bool // valid once judged is closed
	judged := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			drainEnd := time.After(opt.Drain)
			cancelStop.Store(judgeStop(opt))
			close(judged)
			select {
			case <-drainEnd:
				abort()
			case <-reqCtx.Done():
			}
		case <-reqCtx.Done():
		}
	}()

	work := make(chan int)
	var wg sync.WaitGroup
	var fatalOnce sync.Once
	var fatal error
	stopped := make(chan struct{}) // closed on the first record write that failed
	for w := 0; w < opt.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range work {
				out, ok := send(reqCtx, ctx, &cancelStop, opt, rec, n, lines[n])
				if err := rec.result(n, out, ok); err != nil {
					fatalOnce.Do(func() { fatal = err; close(stopped) })
					continue
				}
				mu.Lock()
				if ok {
					res.Completed++
				} else {
					res.Failed++
				}
				mu.Unlock()
			}
		}()
	}
dispatch:
	for n := range lines {
		if rec.sent[n] || rec.finished[n] {
			continue
		}
		// Checked before the select too: with a worker free and the context
		// done, select would pick either case at random, and a cancel must stop
		// at the NEXT line, not one or two later.
		if ctx.Err() != nil {
			res.Cancelled = true
			break
		}
		select {
		case <-ctx.Done():
			res.Cancelled = true
			break dispatch
		case <-stopped:
			break dispatch
		case work <- n:
		}
	}
	close(work)
	wg.Wait()
	close(stopTicker)
	if ctx.Err() != nil {
		<-judged
		switch {
		case !cancelStop.Load():
			// Not a cancel: nothing is over unless every line is answered.
			res.Cancelled = false
			res.Resumable = res.Completed+res.Failed < res.Total
		case !res.Cancelled:
			// Cancelled after the last line was handed out: every line was sent,
			// so the batch is complete, but whether any was cut short is in the
			// counts.
			res.Cancelled = rec.cancelledAny()
		}
	}
	report()
	return res, fatal
}

// judgeStop asks Options.StopIsCancel, bounded; no judge = a cancel.
func judgeStop(opt Options) bool {
	if opt.StopIsCancel == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), stopJudgeTimeout)
	defer cancel()
	return opt.StopIsCancel(ctx)
}

// customID tolerates a line that did not parse.
func (l parsedLine) customID() string { return l.line.CustomID }

// parsedLine is an input line and why it cannot be sent, if it cannot.
type parsedLine struct {
	line    Line
	invalid string
}

// readLines parses the whole input. Blank lines are skipped and not counted,
// like OpenAI's; a line that does not parse is kept — it is answered in the
// error file, in its place — so line numbers stay the input's.
func readLines(path string) ([]parsedLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("batch: input: %w", err)
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var out []parsedLine
	for {
		raw, err := r.ReadBytes('\n')
		if len(raw) > maxLine {
			return nil, fmt.Errorf("batch: input line %d is longer than %d bytes", len(out), maxLine)
		}
		if t := bytes.TrimSpace(raw); len(t) > 0 {
			out = append(out, parseLine(t))
		}
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("batch: input: %w", err)
		}
	}
}

func parseLine(raw []byte) parsedLine {
	var l Line
	if err := json.Unmarshal(raw, &l); err != nil {
		return parsedLine{invalid: "the line is not a JSON object: " + err.Error()}
	}
	p := parsedLine{line: l}
	switch {
	case l.CustomID == "":
		p.invalid = "custom_id is required"
	case l.Method != "" && !strings.EqualFold(l.Method, http.MethodPost):
		p.invalid = "method must be POST"
	case !strings.HasPrefix(l.URL, "/"):
		p.invalid = "url must be a path such as /v1/chat/completions"
	case len(bytes.TrimSpace(l.Body)) == 0 || bytes.TrimSpace(l.Body)[0] != '{':
		p.invalid = "body must be a JSON object"
	}
	return p
}

// send answers one line: the request when the line is valid, the reason when
// it is not. ok is true for a 2xx answer. dispatchCtx is the run's own
// context: a request aborted after it stopped is "cancelled" or "interrupted"
// (cancelStop, judged before any abort), not a transport failure.
func send(reqCtx, dispatchCtx context.Context, cancelStop *atomic.Bool, opt Options, rec *record, n int, p parsedLine) (OutputLine, bool) {
	out := OutputLine{ID: reqID(n), CustomID: p.line.CustomID}
	if p.invalid != "" {
		out.Error = &LineError{Code: CodeInvalidLine, Message: p.invalid}
		return out, false
	}
	body, err := rewriteModel(p.line.Body, opt.ModelMap)
	if err != nil {
		out.Error = &LineError{Code: CodeInvalidLine, Message: "body must be a JSON object: " + err.Error()}
		return out, false
	}
	// The record first: once this returns, a kill at any later instant leaves
	// the line marked as possibly sent, and it is never sent again.
	if err := rec.markSent(n); err != nil {
		out.Error = &LineError{Code: CodeTransport, Message: "the progress record could not be written, so the line was not sent: " + err.Error()}
		return out, false
	}
	ctx, cancel := context.WithTimeout(reqCtx, opt.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joinURL(opt.BaseURL, p.line.URL), bytes.NewReader(body))
	if err != nil {
		out.Error = &LineError{Code: CodeInvalidLine, Message: err.Error()}
		return out, false
	}
	req.Header.Set("Content-Type", "application/json")
	if opt.Key != "" {
		req.Header.Set("Authorization", "Bearer "+opt.Key)
	}
	resp, err := opt.Client.Do(req)
	if err != nil {
		switch {
		case dispatchCtx.Err() != nil && reqCtx.Err() != nil && cancelStop.Load():
			rec.noteCancelled()
			out.Error = &LineError{Code: CodeCancelled, Message: "the batch was cancelled while this line was in flight"}
		case dispatchCtx.Err() != nil && reqCtx.Err() != nil:
			// Aborted at the end of the drain of a stop that was not a cancel.
			// It may have reached the server, so a resume does not send it
			// again (at most once) — it is answered here, by name.
			out.Error = &LineError{Code: CodeInterrupted,
				Message: "this line was in flight when the run was stopped; it is not sent twice — submit it again if its answer is needed"}
		default:
			out.Error = &LineError{Code: CodeTransport, Message: err.Error()}
		}
		return out, false
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(raw) > maxResponse {
		if err == nil {
			err = fmt.Errorf("the answer is larger than %d bytes", maxResponse)
		}
		out.Error = &LineError{Code: CodeTransport, Message: "reading the answer: " + err.Error()}
		return out, false
	}
	if !json.Valid(raw) {
		// The output format carries the body as JSON; a non-JSON answer (an
		// HTML error page from a proxy) is kept as a string.
		raw, _ = json.Marshal(string(raw))
	}
	out.Response = &Response{StatusCode: resp.StatusCode, RequestID: resp.Header.Get("X-Request-Id"), Body: raw}
	return out, resp.StatusCode >= 200 && resp.StatusCode < 300
}

// rewriteModel sets the body's `model` to its mapped value, leaving every
// other field byte for byte as written.
func rewriteModel(body json.RawMessage, m map[string]string) ([]byte, error) {
	if len(m) == 0 {
		return body, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	var name string
	if raw, ok := obj["model"]; !ok || json.Unmarshal(raw, &name) != nil {
		return body, nil
	}
	to, ok := m[name]
	if !ok || to == name {
		return body, nil
	}
	v, _ := json.Marshal(to)
	obj["model"] = v
	return json.Marshal(obj)
}

// joinURL puts a line's path under the base URL without doubling /v1.
func joinURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(path, "/v1/") {
		path = strings.TrimPrefix(path, "/v1")
	}
	return base + path
}

// waitReady polls GET <base>/models until it answers 2xx with the key.
func waitReady(ctx context.Context, opt Options) error {
	deadline := time.Now().Add(opt.ReadyTimeout)
	last := ""
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(opt.BaseURL, "/v1/models"), nil)
		if err != nil {
			return err
		}
		if opt.Key != "" {
			req.Header.Set("Authorization", "Bearer "+opt.Key)
		}
		resp, err := opt.Client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			last = resp.Status
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("batch: %s did not accept the key within %s (last: %s)", opt.BaseURL, opt.ReadyTimeout, last)
		}
		opt.Log.Info("waiting for the server to accept the key", "last", last)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// stageInput is the local path of the input. A URL is downloaded into the
// work directory once (temp file + rename), so a resume never reads a file
// that changed underneath it.
func stageInput(ctx context.Context, opt Options) (string, error) {
	if opt.Input == "" {
		return "", errors.New("batch: an input is required")
	}
	u, err := url.Parse(opt.Input)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return opt.Input, nil
	}
	dst := filepath.Join(opt.Dir, InputFile)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opt.Input, nil)
	if err != nil {
		return "", err
	}
	if opt.Key != "" && !opt.InputPresigned {
		req.Header.Set("Authorization", "Bearer "+opt.Key)
	}
	client := opt.FilesClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("batch: download the input: %w", redact(err, opt.Input))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("batch: download the input: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	tmp, err := os.CreateTemp(opt.Dir, ".input-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return "", fmt.Errorf("batch: download the input: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", err
	}
	return dst, nil
}

func newClient(ca []byte) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 64
	if len(ca) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("batch: the CA file holds no PEM certificate")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: tr}, nil
}
