package batch

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is an OpenAI-shaped chat endpoint that serves one model, counts
// every request by custom id (carried in the body's `user` field, which the
// lines below set to their custom_id) and can hold chosen ids until released.
type fakeServer struct {
	model string
	mu    sync.Mutex
	hits  map[string]int
	order []string
	hold  map[string]chan struct{}
	seen  chan string
}

func newFake(model string) *fakeServer {
	return &fakeServer{model: model, hits: map[string]int{}, hold: map[string]chan struct{}{}, seen: make(chan string, 1000)}
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
		w.Write([]byte(`{"object":"list","data":[]}`))
		return
	}
	var body struct {
		Model string `json:"model"`
		User  string `json:"user"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &body)
	f.mu.Lock()
	f.hits[body.User]++
	f.order = append(f.order, body.User)
	hold := f.hold[body.User]
	f.mu.Unlock()
	f.seen <- body.User
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("X-Request-Id", "req-"+body.User)
	if body.Model != f.model {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"error":{"message":"model %q not found","type":"invalid_request_error","code":"model_not_found"}}`, body.Model)
		return
	}
	fmt.Fprintf(w, `{"id":"chatcmpl-%s","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`, body.User, body.Model)
}

func (f *fakeServer) count(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[id]
}

// writeInput writes n chat lines for model, custom ids c0…c<n-1>.
func writeInput(t *testing.T, dir string, n int, modelFor func(i int) string) string {
	t.Helper()
	p := filepath.Join(dir, "in.jsonl")
	var sb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, `{"custom_id":"c%d","method":"POST","url":"/v1/chat/completions","body":{"model":%q,"user":"c%d","messages":[{"role":"user","content":"hi"}]}}`+"\n", i, modelFor(i), i)
	}
	if err := os.WriteFile(p, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readResults(t *testing.T, path string) map[string]OutputLine {
	t.Helper()
	out := map[string]OutputLine{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return out
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var o OutputLine
		if err := json.Unmarshal(sc.Bytes(), &o); err != nil {
			t.Fatalf("%s: a line that is not an output object: %q", path, sc.Text())
		}
		if _, dup := out[o.CustomID]; dup {
			t.Fatalf("%s: %s answered twice", path, o.CustomID)
		}
		out[o.CustomID] = o
	}
	return out
}

// L1: a line naming a model the server does not serve gets the server's 404 in
// the error file, in OpenAI's shape; the other lines land in the output file,
// and a mapped alias is rewritten to the served id before it is sent.
func TestUnknownModelLandsInErrorFile(t *testing.T) {
	fake := newFake("served-model")
	srv := httptest.NewServer(fake)
	defer srv.Close()
	dir := t.TempDir()
	in := writeInput(t, dir, 4, func(i int) string {
		switch i {
		case 1:
			return "nope"
		case 2:
			return "alias"
		}
		return "served-model"
	})
	res, err := Run(context.Background(), Options{Input: in, BaseURL: srv.URL + "/v1", Dir: filepath.Join(dir, "work"), Concurrency: 2,
		ModelMap: map[string]string{"alias": "served-model"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 4 || res.Completed != 3 || res.Failed != 1 || res.Cancelled {
		t.Fatalf("counts: %+v", res)
	}
	errs := readResults(t, res.ErrorPath)
	e, ok := errs["c1"]
	if !ok || len(errs) != 1 {
		t.Fatalf("the unknown model's line is the one error: %+v", errs)
	}
	if e.Response == nil || e.Response.StatusCode != http.StatusNotFound || !strings.Contains(string(e.Response.Body), "model_not_found") || e.Error != nil {
		t.Fatalf("the error line carries the server's 404 as its response: %+v", e)
	}
	if e.ID != "batch_req_1" || e.Response.RequestID != "req-c1" {
		t.Fatalf("id and request id: %+v", e)
	}
	outs := readResults(t, res.OutputPath)
	if len(outs) != 3 || outs["c2"].Response == nil || outs["c2"].Response.StatusCode != 200 {
		t.Fatalf("the alias line was rewritten and answered: %+v", outs)
	}
}

// A line that is not a valid request is answered in the error file without
// being sent.
func TestInvalidLineIsNotSent(t *testing.T) {
	fake := newFake("m")
	srv := httptest.NewServer(fake)
	defer srv.Close()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.jsonl")
	os.WriteFile(in, []byte(`{"custom_id":"a","method":"GET","url":"/v1/chat/completions","body":{"model":"m","user":"a"}}
not json

{"custom_id":"b","method":"POST","url":"/v1/chat/completions","body":{"model":"m","user":"b"}}
`), 0o600)
	res, err := Run(context.Background(), Options{Input: in, BaseURL: srv.URL, Dir: filepath.Join(dir, "w")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 3 || res.Completed != 1 || res.Failed != 2 {
		t.Fatalf("counts (a blank line is not a line): %+v", res)
	}
	if fake.count("a") != 0 {
		t.Fatal("a GET line was sent")
	}
	errs := readResults(t, res.ErrorPath)
	if errs["a"].Error == nil || errs["a"].Error.Code != CodeInvalidLine {
		t.Fatalf("the GET line's reason: %+v", errs)
	}
}

// L1: cancel stops at the next line. With one request at a time, line c1 is
// held in flight while the context is cancelled: c1 finishes inside the drain,
// and no later line is ever sent.
func TestCancelStopsAtNextLine(t *testing.T) {
	fake := newFake("m")
	release := make(chan struct{})
	fake.hold["c1"] = release
	srv := httptest.NewServer(fake)
	defer srv.Close()
	dir := t.TempDir()
	in := writeInput(t, dir, 6, func(int) string { return "m" })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() {
		res, err := Run(ctx, Options{Input: in, BaseURL: srv.URL, Dir: filepath.Join(dir, "w"), Concurrency: 1, Drain: 5 * time.Second})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	waitSeen(t, fake, "c1")
	cancel()
	time.Sleep(100 * time.Millisecond) // the dispatcher sees the cancel while c1 is still in flight
	close(release)
	res := <-done
	if !res.Cancelled || res.Completed != 2 || res.Failed != 0 || res.Total != 6 {
		t.Fatalf("result: %+v", res)
	}
	for i := 2; i < 6; i++ {
		if n := fake.count(fmt.Sprintf("c%d", i)); n != 0 {
			t.Fatalf("c%d was sent after the cancel", i)
		}
	}
	// A request still in flight when the drain runs out is aborted and named.
	fake2 := newFake("m")
	fake2.hold["c0"] = make(chan struct{})
	srv2 := httptest.NewServer(fake2)
	defer srv2.Close()
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan Result, 1)
	go func() {
		res, _ := Run(ctx2, Options{Input: in, BaseURL: srv2.URL, Dir: filepath.Join(dir, "w2"), Concurrency: 1, Drain: 50 * time.Millisecond})
		done2 <- res
	}()
	waitSeen(t, fake2, "c0")
	cancel2()
	res2 := <-done2
	errs := readResults(t, res2.ErrorPath)
	if !res2.Cancelled || errs["c0"].Error == nil || errs["c0"].Error.Code != CodeCancelled || fake2.count("c1") != 0 {
		t.Fatalf("aborted in flight: %+v %+v", res2, errs)
	}
}

func waitSeen(t *testing.T, f *fakeServer, id string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case got := <-f.seen:
			if got == id {
				return
			}
		case <-deadline:
			t.Fatalf("the server never saw %s", id)
		}
	}
}

// L1: resume after a KILL with no line sent twice. A child process runs the
// batch with one request at a time and is SIGKILLed while c3 is held in flight
// — no cleanup of any kind runs. A partial line is then appended to the output
// file, as a kill mid-write would leave it. The second run, on the same
// directory, sends every line the first did not, never sends c0–c3 again,
// writes c3 to the error file as interrupted, and repairs the partial line.
func TestResumeAfterKillSendsNoLineTwice(t *testing.T) {
	if os.Getenv("OPOD_BATCH_HELPER") == "1" {
		t.Skip("helper process")
	}
	fake := newFake("m")
	fake.hold["c3"] = make(chan struct{}) // never released: c3 is in flight at the kill
	srv := httptest.NewServer(fake)
	defer srv.Close()
	dir := t.TempDir()
	in := writeInput(t, dir, 10, func(int) string { return "m" })
	work := filepath.Join(dir, "w")

	cmd := exec.Command(os.Args[0], "-test.run=^TestBatchHelperProcess$")
	cmd.Env = append(os.Environ(), "OPOD_BATCH_HELPER=1", "OPOD_BATCH_URL="+srv.URL, "OPOD_BATCH_IN="+in, "OPOD_BATCH_DIR="+work)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitSeen(t, fake, "c3")
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	for i := 0; i < 4; i++ {
		if fake.count(fmt.Sprintf("c%d", i)) != 1 {
			t.Fatalf("first run: c%d sent %d times", i, fake.count(fmt.Sprintf("c%d", i)))
		}
	}
	if fake.count("c4") != 0 {
		t.Fatal("first run: c4 was sent while c3 was held with one request at a time")
	}
	// A kill in the middle of a write: a partial line at the end of the output.
	f, _ := os.OpenFile(filepath.Join(work, OutputFile), os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"id":"batch_req_4","custom_id":"c4","resp`)
	f.Close()

	fake.mu.Lock()
	delete(fake.hold, "c3")
	fake.mu.Unlock()
	res, err := Run(context.Background(), Options{Input: in, BaseURL: srv.URL, Dir: work, Concurrency: 3})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if n := fake.count(fmt.Sprintf("c%d", i)); n != 1 {
			t.Fatalf("across both runs c%d was sent %d times, want exactly 1", i, n)
		}
	}
	if res.Total != 10 || res.Completed != 9 || res.Failed != 1 || res.Interrupted != 1 || res.Cancelled {
		t.Fatalf("counts: %+v", res)
	}
	errs := readResults(t, res.ErrorPath)
	if e := errs["c3"]; e.Error == nil || e.Error.Code != CodeInterrupted || e.ID != "batch_req_3" {
		t.Fatalf("c3 is written as interrupted: %+v", errs)
	}
	outs := readResults(t, res.OutputPath)
	if len(outs) != 9 {
		t.Fatalf("nine answers, each once: %d", len(outs))
	}
	// A third run on a finished directory sends nothing and reads the same counts.
	res3, err := Run(context.Background(), Options{Input: in, BaseURL: srv.URL, Dir: work})
	if err != nil || res3.Completed != 9 || res3.Failed != 1 {
		t.Fatalf("a finished directory: %v %+v", err, res3)
	}
	for i := 0; i < 10; i++ {
		if fake.count(fmt.Sprintf("c%d", i)) != 1 {
			t.Fatalf("a third run re-sent c%d", i)
		}
	}
}

// TestBatchHelperProcess is the child the kill test runs and kills.
func TestBatchHelperProcess(t *testing.T) {
	if os.Getenv("OPOD_BATCH_HELPER") != "1" {
		t.Skip("run by TestResumeAfterKillSendsNoLineTwice")
	}
	_, _ = Run(context.Background(), Options{Input: os.Getenv("OPOD_BATCH_IN"), BaseURL: os.Getenv("OPOD_BATCH_URL"),
		Dir: os.Getenv("OPOD_BATCH_DIR"), Concurrency: 1})
	os.Exit(0)
}

// The input may be a files content URL read with the key, or a presigned URL
// read with NO key (a store refuses a request that carries two credentials);
// either is staged once. The results are PUT to presigned URLs, with no key,
// their length stated (a presigned PUT takes no chunked body) — and a run that
// is interrupted writes what it has, then the resume writes again and its
// files replace the first ones.
func TestInputURLAndPutResults(t *testing.T) {
	fake := newFake("m")
	fake.hold["c1"] = make(chan struct{}) // held: the first run is interrupted on it
	src := writeInput(t, t.TempDir(), 4, func(i int) string {
		if i == 3 {
			return "x"
		}
		return "m"
	})
	raw, _ := os.ReadFile(src)
	var mu sync.Mutex
	objects := map[string]string{}
	puts := 0
	mux := http.NewServeMux()
	mux.Handle("/v1/chat/completions", fake)
	mux.HandleFunc("GET /v1/files/file-in/content", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			return
		}
		w.Write(raw)
	})
	// The store: a presigned URL's query is its credential; a second one is refused.
	store := func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("X-Amz-Signature") != "sig" || r.Header.Get("Authorization") != "" {
			w.WriteHeader(400)
			fmt.Fprint(w, "<Error><Code>InvalidArgument</Code><Message>Only one auth mechanism allowed</Message></Error>")
			return false
		}
		return true
	}
	mux.HandleFunc("GET /bucket/in.jsonl", func(w http.ResponseWriter, r *http.Request) {
		if store(w, r) {
			w.Write(raw)
		}
	})
	mux.HandleFunc("PUT /bucket/{obj}", func(w http.ResponseWriter, r *http.Request) {
		if !store(w, r) {
			return
		}
		if r.ContentLength <= 0 || len(r.TransferEncoding) > 0 {
			w.WriteHeader(411) // what S3 answers a chunked PUT
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		objects[r.PathValue("obj")] = string(b)
		puts++
		mu.Unlock()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	work := t.TempDir()
	in := srv.URL + "/bucket/in.jsonl?X-Amz-Signature=sig"
	out, errs := srv.URL+"/bucket/output?X-Amz-Signature=sig", srv.URL+"/bucket/errors?X-Amz-Signature=sig"
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() {
		res, err := Run(ctx, Options{Input: in, InputPresigned: true, Key: "k", BaseURL: srv.URL + "/v1", Dir: work, Concurrency: 1,
			Drain: 50 * time.Millisecond, StopIsCancel: func(context.Context) bool { return false }})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	waitSeen(t, fake, "c1")
	stop()
	res := <-done
	if !res.Resumable {
		t.Fatalf("the first run was interrupted: %+v", res)
	}
	if w, err := PutResults(context.Background(), nil, out, errs, res); err != nil || !w.Output || !w.Errors {
		t.Fatalf("the interrupted run writes what it has: %+v %v", w, err)
	}
	if got := strings.Count(objects["output"], "\n"); got != 1 {
		t.Fatalf("after the interruption the output object holds c0 only: %q", objects["output"])
	}
	fake.mu.Lock()
	delete(fake.hold, "c1")
	fake.mu.Unlock()
	res2, err := Run(context.Background(), Options{Input: in, InputPresigned: true, Key: "k", BaseURL: srv.URL + "/v1", Dir: work})
	if err != nil || res2.Resumable {
		t.Fatalf("resumed: %v %+v", err, res2)
	}
	if w, err := PutResults(context.Background(), nil, out, errs, res2); err != nil || !w.Output || !w.Errors {
		t.Fatalf("the resumed run writes again: %+v %v", w, err)
	}
	// c0 and c2 answered; c1 was in flight at the interruption (never resent),
	// c3 names a model the server does not serve.
	if o, e := strings.Count(objects["output"], "\n"), strings.Count(objects["errors"], "\n"); o != 2 || e != 2 || puts != 4 {
		t.Fatalf("the objects are replaced by the resume's files: output %d lines, errors %d lines, %d PUTs", o, e, puts)
	}
	for i := 0; i < 4; i++ {
		if n := fake.count(fmt.Sprintf("c%d", i)); n != 1 {
			t.Fatalf("c%d was sent %d times", i, n)
		}
	}

	// The keyed form (an OpenAI files content URL) still sends the key.
	if r, err := Run(context.Background(), Options{Input: srv.URL + "/v1/files/file-in/content", Key: "k", BaseURL: srv.URL + "/v1", Dir: t.TempDir()}); err != nil || r.Total != 4 {
		t.Fatalf("a keyed input URL: %v %+v", err, r)
	}
	// A keyed presigned read is refused by the store — the runner must not send it.
	if _, err := Run(context.Background(), Options{Input: in, Key: "k", BaseURL: srv.URL + "/v1", Dir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("a presigned URL read WITH the key: %v", err)
	}
	// An empty result file is not written, as OpenAI leaves the id null.
	empty := Result{OutputPath: filepath.Join(t.TempDir(), "output.jsonl"), ErrorPath: filepath.Join(t.TempDir(), "errors.jsonl")}
	os.WriteFile(empty.OutputPath, nil, 0o600)
	if w, err := PutResults(context.Background(), nil, out, errs, empty); err != nil || w.Output || w.Errors {
		t.Fatalf("nothing to write: %+v %v", w, err)
	}
}
