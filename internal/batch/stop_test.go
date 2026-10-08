package batch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The batch object's status is what tells a cancel from an interruption: still
// running means the stop was not asked for; any end, or a key that no longer
// opens the batch, means it was; an answer that cannot be read is never taken
// for a cancel.
func TestCancelRequestedReadsTheBatchObject(t *testing.T) {
	var code atomic.Int32
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.WriteHeader(int(code.Load()))
		w.Write([]byte(body.Load().(string)))
	}))
	defer srv.Close()
	for _, c := range []struct {
		code int
		body string
		want bool
	}{
		{200, `{"id":"batch_1","status":"in_progress"}`, false},
		{200, `{"status":"validating"}`, false},
		{200, `{"status":"cancelling"}`, true},
		{200, `{"status":"finalizing"}`, true},
		{200, `{"status":"cancelled"}`, true},
		{200, `{"status":"expired"}`, true},
		{200, `{"status":"failed"}`, true},
		{401, `{"error":{}}`, true},
		{404, `{"error":{}}`, true},
		{500, `{"status":"cancelling"}`, false},
		{503, ``, false},
		{200, `not json`, false},
		{200, `{}`, false},
	} {
		code.Store(int32(c.code))
		body.Store(c.body)
		if got := CancelRequested(context.Background(), nil, srv.URL+"/v1/batches/batch_1", "k"); got != c.want {
			t.Errorf("%d %s: cancel=%v, want %v", c.code, c.body, got, c.want)
		}
	}
	if CancelRequested(context.Background(), nil, "", "k") {
		t.Error("no status URL is a cancel")
	}
	srv.Close()
	if CancelRequested(context.Background(), nil, srv.URL+"/v1/batches/batch_1", "k") {
		t.Error("an unreachable batch object is a cancel")
	}
}

// A stop that is not a cancel ends nothing: dispatch stops at the next line, the
// line in flight finishes inside the drain, the run says it is resumable (and
// the command's exit status says so), and a second run on the same directory
// sends every remaining line exactly once.
func TestAnInterruptionResumesAndSendsNoLineTwice(t *testing.T) {
	fake := newFake("m")
	release := make(chan struct{})
	fake.hold["c2"] = release
	srv := httptest.NewServer(fake)
	defer srv.Close()
	dir := t.TempDir()
	in := writeInput(t, dir, 10, func(int) string { return "m" })
	work := filepath.Join(dir, "w")
	ctx, stop := context.WithCancel(context.Background())
	var judged atomic.Int32
	done := make(chan Result, 1)
	go func() {
		res, err := Run(ctx, Options{Input: in, BaseURL: srv.URL, Dir: work, Concurrency: 1, Drain: 5 * time.Second,
			StopIsCancel: func(context.Context) bool { judged.Add(1); return false }})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	waitSeen(t, fake, "c2")
	stop()
	time.Sleep(100 * time.Millisecond)
	close(release)
	res := <-done
	if !res.Resumable || res.Cancelled || res.Completed != 3 || res.Failed != 0 || res.Total != 10 || judged.Load() != 1 {
		t.Fatalf("an interruption: %+v (judged %d times)", res, judged.Load())
	}
	if ExitCode(res) != ExitInterrupted {
		t.Fatalf("exit status %d, want %d", ExitCode(res), ExitInterrupted)
	}
	res2, err := Run(context.Background(), Options{Input: in, BaseURL: srv.URL, Dir: work, Concurrency: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Resumable || res2.Cancelled || res2.Completed != 10 || res2.Failed != 0 || ExitCode(res2) != 0 {
		t.Fatalf("the resumed run: %+v", res2)
	}
	for i := 0; i < 10; i++ {
		if n := fake.count(fmt.Sprintf("c%d", i)); n != 1 {
			t.Fatalf("c%d was sent %d times, want exactly 1", i, n)
		}
	}
}

// A line still in flight when an interruption's drain runs out is aborted and
// written as "interrupted" — it may have reached the server — and the resume
// does not send it again.
func TestAnInterruptionPastTheDrainNamesTheLineAndNeverResendsIt(t *testing.T) {
	fake := newFake("m")
	fake.hold["c1"] = make(chan struct{}) // never released
	srv := httptest.NewServer(fake)
	defer srv.Close()
	dir := t.TempDir()
	in := writeInput(t, dir, 4, func(int) string { return "m" })
	work := filepath.Join(dir, "w")
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() {
		res, _ := Run(ctx, Options{Input: in, BaseURL: srv.URL, Dir: work, Concurrency: 1, Drain: 50 * time.Millisecond,
			StopIsCancel: func(context.Context) bool { return false }})
		done <- res
	}()
	waitSeen(t, fake, "c1")
	stop()
	res := <-done
	errs := readResults(t, res.ErrorPath)
	if !res.Resumable || res.Cancelled || errs["c1"].Error == nil || errs["c1"].Error.Code != CodeInterrupted {
		t.Fatalf("aborted by an interruption: %+v %+v", res, errs)
	}
	fake.mu.Lock()
	delete(fake.hold, "c1")
	fake.mu.Unlock()
	res2, err := Run(context.Background(), Options{Input: in, BaseURL: srv.URL, Dir: work})
	if err != nil || res2.Completed != 3 || res2.Failed != 1 || res2.Resumable {
		t.Fatalf("resumed: %v %+v", err, res2)
	}
	for i := 0; i < 4; i++ {
		if n := fake.count(fmt.Sprintf("c%d", i)); n != 1 {
			t.Fatalf("c%d was sent %d times", i, n)
		}
	}
}

// The process, end to end, with a real SIGTERM — what Kubernetes sends a pod it
// deletes. While the batch object says in_progress, the SIGTERM is an
// interruption: the process exits ExitInterrupted, and a restart on the same
// directory sends the remaining lines exactly once. When the batch object says
// cancelling, the same SIGTERM is a cancel: exit 0, cancelled, nothing resent.
func TestSigtermIsACancelOnlyWhenTheBatchSaysSo(t *testing.T) {
	if os.Getenv("OPOD_BATCH_SIG_HELPER") == "1" {
		t.Skip("helper process")
	}
	for _, c := range []struct {
		status   string
		wantExit int
	}{{"in_progress", ExitInterrupted}, {"cancelling", 0}} {
		t.Run(c.status, func(t *testing.T) {
			fake := newFake("m")
			release := make(chan struct{})
			fake.hold["c3"] = release
			mux := http.NewServeMux()
			mux.Handle("/v1/chat/completions", fake)
			mux.HandleFunc("GET /v1/batches/batch_1", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"id":"batch_1","object":"batch","status":%q}`, c.status)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			dir := t.TempDir()
			in := writeInput(t, dir, 10, func(int) string { return "m" })
			work := filepath.Join(dir, "w")

			cmd := exec.Command(os.Args[0], "-test.run=^TestBatchSignalHelperProcess$")
			var stdout bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Env = append(os.Environ(), "OPOD_BATCH_SIG_HELPER=1", "OPOD_BATCH_URL="+srv.URL+"/v1",
				"OPOD_BATCH_IN="+in, "OPOD_BATCH_DIR="+work, "OPOD_BATCH_STATUS="+srv.URL+"/v1/batches/batch_1")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waitSeen(t, fake, "c3")
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			time.Sleep(200 * time.Millisecond) // the stop is seen while c3 is still in flight
			close(release)                     // c3 finishes inside the drain
			err := cmd.Wait()
			exit := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				exit = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			var res Result
			if jerr := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res); jerr != nil {
				t.Fatalf("the helper's result %q: %v", stdout.String(), jerr)
			}
			if exit != c.wantExit {
				t.Fatalf("exit status %d, want %d (%+v)", exit, c.wantExit, res)
			}
			if res.Completed != 4 || res.Cancelled == res.Resumable || res.Resumable != (c.status == "in_progress") {
				t.Fatalf("the stopped run: %+v", res)
			}
			for i := 4; i < 10; i++ {
				if n := fake.count(fmt.Sprintf("c%d", i)); n != 0 {
					t.Fatalf("c%d was sent after the SIGTERM", i)
				}
			}
			if c.status != "in_progress" {
				return
			}
			// The restart a Job makes after the non-zero exit.
			res2, err := Run(context.Background(), Options{Input: in, BaseURL: srv.URL + "/v1", Dir: work, Concurrency: 3})
			if err != nil || res2.Completed != 10 || res2.Failed != 0 || ExitCode(res2) != 0 {
				t.Fatalf("the restart: %v %+v", err, res2)
			}
			for i := 0; i < 10; i++ {
				if n := fake.count(fmt.Sprintf("c%d", i)); n != 1 {
					t.Fatalf("across both runs c%d was sent %d times, want exactly 1", i, n)
				}
			}
		})
	}
}

// TestBatchSignalHelperProcess is the child the SIGTERM test signals: it runs
// the batch the way `opod batch run` does (Signals + CancelRequested + Run) and
// exits with ExitCode, printing its Result.
func TestBatchSignalHelperProcess(t *testing.T) {
	if os.Getenv("OPOD_BATCH_SIG_HELPER") != "1" {
		t.Skip("run by TestSigtermIsACancelOnlyWhenTheBatchSaysSo")
	}
	status := os.Getenv("OPOD_BATCH_STATUS")
	ctx, judge, stop := Signals(context.Background(), func(c context.Context) bool {
		return CancelRequested(c, nil, status, "")
	})
	res, err := Run(ctx, Options{Input: os.Getenv("OPOD_BATCH_IN"), BaseURL: os.Getenv("OPOD_BATCH_URL"),
		Dir: os.Getenv("OPOD_BATCH_DIR"), Concurrency: 1, Drain: 5 * time.Second, StopIsCancel: judge})
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(res)
	os.Exit(ExitCode(res))
}
