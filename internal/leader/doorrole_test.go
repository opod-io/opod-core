package leader

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/config"
)

// fakeEndpoint is an endpoint's leader as far as a door can tell: it accepts
// exactly one key, echoes what reached it, and streams when asked.
type fakeEndpoint struct {
	name string
	key  string
	// release, when set, holds a stream between its first and second chunk
	// until the test closes it.
	release chan struct{}
}

func (f *fakeEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.key {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key","type":"authentication_error"}}`)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"object":"list","data":[{"id":%q}]}`, f.name)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			_, _ = io.WriteString(w, "data: {\"chunk\":1}\n\n")
			fl.Flush()
			if f.release != nil {
				<-f.release
			}
			_, _ = io.WriteString(w, "data: {\"chunk\":2}\n\ndata: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"endpoint": f.name, "body": string(body), "path": r.URL.Path})
	default:
		http.NotFound(w, r)
	}
}

// newTestDoor writes a routes file and returns a door serving it.
func newTestDoor(t *testing.T, routes DoorRoutes) (*CellDoor, *httptest.Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "routes.json")
	writeRoutes(t, path, routes)
	cfg := config.Default()
	cfg.MaxBodyBytes = 4096
	d, err := NewCellDoor(cfg, nil, path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.watchRoutes(ctx)
	ts := httptest.NewServer(d.Handler())
	t.Cleanup(ts.Close)
	return d, ts, path
}

func writeRoutes(t *testing.T, path string, routes DoorRoutes) {
	t.Helper()
	raw, err := json.Marshal(routes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func doorPost(t *testing.T, base, key, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func twoEndpoints(t *testing.T) (a, b *fakeEndpoint, ua, ub string) {
	t.Helper()
	a = &fakeEndpoint{name: "llama-3.1-8b", key: "key-a"}
	b = &fakeEndpoint{name: "qwen-2.5-7b", key: "key-b"}
	sa, sb := httptest.NewServer(a), httptest.NewServer(b)
	t.Cleanup(sa.Close)
	t.Cleanup(sb.Close)
	return a, b, sa.URL, sb.URL
}

// The door sends each request to the endpoint its model names, with the body
// as the client sent it; an alias that is not the endpoint's model id is
// rewritten to that id and nothing else in the body moves.
func TestDoorDispatchesByAlias(t *testing.T) {
	_, _, ua, ub := twoEndpoints(t)
	_, ts, _ := newTestDoor(t, DoorRoutes{Revision: "r1", Routes: []DoorRoute{
		{Alias: "llama-3.1-8b", Upstream: ua},
		{Alias: "chat-b", Upstream: ub + "/v1", Model: "qwen-2.5-7b"},
	}})

	sent := `{"model":"llama-3.1-8b","messages":[{"role":"user","content":"hi"}],"seed":12345678901234567}`
	code, out := doorPost(t, ts.URL, "key-a", sent)
	if code != http.StatusOK || out["endpoint"] != "llama-3.1-8b" {
		t.Fatalf("alias llama-3.1-8b: %d %v, want endpoint A", code, out)
	}
	if out["body"] != sent {
		t.Fatalf("the body changed on its way through the door:\n got %s\nwant %s", out["body"], sent)
	}

	code, out = doorPost(t, ts.URL, "key-b", `{"model":"chat-b","messages":[],"seed":12345678901234567}`)
	if code != http.StatusOK || out["endpoint"] != "qwen-2.5-7b" {
		t.Fatalf("alias chat-b: %d %v, want endpoint B", code, out)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out["body"].(string)), &got); err != nil {
		t.Fatal(err)
	}
	if string(got["model"]) != `"qwen-2.5-7b"` || string(got["seed"]) != "12345678901234567" || string(got["messages"]) != "[]" {
		t.Fatalf("the alias was not rewritten to the endpoint's model alone: %s", out["body"])
	}
}

// An unknown model, no model and "auto" are OpenAI's 404 model_not_found:
// a door looks names up and chooses nothing.
func TestDoorUnknownModelIs404ModelNotFound(t *testing.T) {
	_, _, ua, _ := twoEndpoints(t)
	_, ts, _ := newTestDoor(t, DoorRoutes{Routes: []DoorRoute{{Alias: "a", Upstream: ua}}})
	for _, body := range []string{`{"model":"gpt-4o","messages":[]}`, `{"messages":[]}`, `{"model":"auto","messages":[]}`} {
		code, out := doorPost(t, ts.URL, "key-a", body)
		e, _ := out["error"].(map[string]any)
		if code != http.StatusNotFound || e["code"] != "model_not_found" || e["type"] != "invalid_request_error" || e["param"] != "model" {
			t.Fatalf("%s: %d %v, want 404 with OpenAI's model_not_found error", body, code, out)
		}
	}
}

// The door holds no credential: a key for endpoint A sent with endpoint B's
// model reaches B, and B's leader refuses it.
func TestDoorForwardsTheCallersKeyAndTheEndpointRefusesIt(t *testing.T) {
	_, _, ua, ub := twoEndpoints(t)
	_, ts, _ := newTestDoor(t, DoorRoutes{Routes: []DoorRoute{{Alias: "a", Upstream: ua}, {Alias: "b", Upstream: ub}}})
	code, out := doorPost(t, ts.URL, "key-a", `{"model":"b","messages":[]}`)
	e, _ := out["error"].(map[string]any)
	if code != http.StatusUnauthorized || e["type"] != "authentication_error" {
		t.Fatalf("key A with model b: %d %v, want B's own 401", code, out)
	}
	if code, _ := doorPost(t, ts.URL, "", `{"model":"a","messages":[]}`); code != http.StatusUnauthorized {
		t.Fatalf("no key: %d, want the endpoint's 401 (the door adds no credential)", code)
	}
}

// GET /v1/models lists only the aliases whose endpoint accepts the caller's
// key, and the per-key cache holds the key's hash, never the key.
func TestDoorModelsAreFilteredByKey(t *testing.T) {
	_, _, ua, ub := twoEndpoints(t)
	d, ts, _ := newTestDoor(t, DoorRoutes{Routes: []DoorRoute{{Alias: "a", Upstream: ua}, {Alias: "b", Upstream: ub}}})
	list := func(key string) []string {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Object string `json:"object"`
			Data   []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Object != "list" {
			t.Fatalf("not an OpenAI model list: %v %+v", err, out)
		}
		ids := []string{}
		for _, m := range out.Data {
			ids = append(ids, m.ID)
		}
		return ids
	}
	if got := list("key-a"); strings.Join(got, ",") != "a" {
		t.Fatalf("key A sees %v, want [a]", got)
	}
	if got := list("key-b"); strings.Join(got, ",") != "b" {
		t.Fatalf("key B sees %v, want [b]", got)
	}
	if got := list("nobody"); len(got) != 0 {
		t.Fatalf("an unknown key sees %v, want nothing", got)
	}
	d.cmu.Lock()
	defer d.cmu.Unlock()
	for k := range d.models {
		if strings.Contains(k, "key-") || len(k) != 64 {
			t.Fatalf("the models cache is keyed by %q, not a SHA-256", k)
		}
	}
}

// A stream reaches the client chunk by chunk: the first event arrives while
// the endpoint is still holding the second, and the bytes are the endpoint's.
func TestDoorStreamsUntouched(t *testing.T) {
	a := &fakeEndpoint{name: "m", key: "k", release: make(chan struct{})}
	sa := httptest.NewServer(a)
	defer sa.Close()
	_, ts, _ := newTestDoor(t, DoorRoutes{Routes: []DoorRoute{{Alias: "m", Upstream: sa.URL}}})

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type %q, want the endpoint's text/event-stream", ct)
	}
	rd := bufio.NewReader(resp.Body)
	first := make(chan string, 1)
	go func() {
		line, _ := rd.ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if line != "data: {\"chunk\":1}\n" {
			t.Fatalf("first line %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first chunk did not arrive while the endpoint held the second: the door buffers streams")
	}
	close(a.release)
	rest, _ := io.ReadAll(rd)
	if want := "\ndata: {\"chunk\":2}\n\ndata: [DONE]\n\n"; string(rest) != want {
		t.Fatalf("the rest of the stream changed:\n got %q\nwant %q", rest, want)
	}
}

// Body caps are the leader's per route (ADR-077 §3): over the cap is 413
// before anything is forwarded.
func TestDoorBodyCapsAreTheLeaders(t *testing.T) {
	_, _, ua, _ := twoEndpoints(t)
	_, ts, _ := newTestDoor(t, DoorRoutes{Routes: []DoorRoute{{Alias: "a", Upstream: ua}}})
	code, out := doorPost(t, ts.URL, "key-a", `{"model":"a","pad":"`+strings.Repeat("x", 4096)+`"}`)
	e, _ := out["error"].(map[string]any)
	if code != http.StatusRequestEntityTooLarge || e["type"] != "request_too_large" {
		t.Fatalf("over the chat cap: %d %v, want 413 request_too_large", code, out)
	}
}

// A routes file that does not parse, or repeats an alias, keeps the last good
// routes; a good one replaces them whole.
func TestDoorRoutesFileKeepsLastGood(t *testing.T) {
	_, _, ua, ub := twoEndpoints(t)
	d, _, path := newTestDoor(t, DoorRoutes{Revision: "r1", Routes: []DoorRoute{{Alias: "a", Upstream: ua}}})
	if d.route("a") == nil {
		t.Fatal("the first file was not applied")
	}
	for _, bad := range []string{`{not json`, `{"routes":[{"alias":"x","upstream":"` + ub + `"},{"alias":"x","upstream":"` + ua + `"}]}`, `{"routes":[{"alias":"x","upstream":"ftp://h"}]}`} {
		if err := d.apply([]byte(bad)); err == nil {
			t.Fatalf("%s was accepted", bad)
		}
		if d.route("a") == nil || d.route("x") != nil {
			t.Fatalf("a refused file %s changed the routes", bad)
		}
	}
	writeRoutes(t, path, DoorRoutes{Revision: "r2", Routes: []DoorRoute{{Alias: "b", Upstream: ub}}})
	if err := d.apply(mustRead(t, path)); err != nil {
		t.Fatal(err)
	}
	if d.route("a") != nil || d.route("b") == nil || d.revision != "r2" {
		t.Fatalf("the good file did not replace the routes whole: %v", d.routes)
	}
}

// An https upstream is trusted through exactly the route's CA.
func TestDoorTrustsTheRoutesCA(t *testing.T) {
	a := &fakeEndpoint{name: "m", key: "k"}
	sa := httptest.NewTLSServer(a)
	defer sa.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: sa.Certificate().Raw}))
	_, ts, _ := newTestDoor(t, DoorRoutes{Routes: []DoorRoute{
		{Alias: "pinned", Upstream: sa.URL, CA: ca},
		{Alias: "unpinned", Upstream: sa.URL},
	}})
	if code, out := doorPost(t, ts.URL, "k", `{"model":"pinned","messages":[]}`); code != http.StatusOK {
		t.Fatalf("with the endpoint's certificate: %d %v", code, out)
	}
	code, out := doorPost(t, ts.URL, "k", `{"model":"unpinned","messages":[]}`)
	e, _ := out["error"].(map[string]any)
	if code != http.StatusBadGateway || e["type"] != "upstream_unavailable" {
		t.Fatalf("without it: %d %v, want 502 (the system roots do not know a self-signed endpoint)", code, out)
	}
}

// /readyz waits for a routes file; /gatewayz says the door's own state.
func TestDoorReadyAndGatewayz(t *testing.T) {
	cfg := config.Default()
	d, err := NewCellDoor(cfg, nil, filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(d.Handler())
	defer ts.Close()
	get := func(path string) (int, map[string]any) {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := get("/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with no routes file: %d, want 503", code)
	}
	if err := d.apply([]byte(`{"revision":"r9","routes":[{"alias":"a","upstream":"http://a:8080"}]}`)); err != nil {
		t.Fatal(err)
	}
	if code, _ := get("/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz after a routes file: %d", code)
	}
	_, gz := get("/gatewayz")
	if gz["role"] != "door" || gz["routes_revision"] != "r9" || gz["routes"] != float64(1) {
		t.Fatalf("/gatewayz: %v", gz)
	}
	if _, err := NewCellDoor(cfg, nil, " "); err == nil {
		t.Fatal("a door with no routes path started")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(b)
}
