package leader

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// scriptedEndpoint is an endpoint's leader that answers chat with a set status
// and body, /loadz with a set load, and counts what reached it.
type scriptedEndpoint struct {
	name    string
	status  int
	body    string
	load    string // /loadz body; "" = 200 with one idle worker
	hits    atomic.Int64
	lastReq atomic.Value // the body of the last chat request
}

func (s *scriptedEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/loadz":
		load := s.load
		if load == "" {
			load = `{"workers":1}`
		}
		_, _ = io.WriteString(w, load)
	case "/v1/models":
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"object":"list","data":[{"id":%q}]}`, s.name)
	default:
		s.hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		s.lastReq.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		if s.status != 0 && s.status != http.StatusOK {
			w.WriteHeader(s.status)
			_, _ = io.WriteString(w, s.body)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"endpoint": s.name})
	}
}

func scripted(t *testing.T, eps ...*scriptedEndpoint) []string {
	t.Helper()
	urls := make([]string, len(eps))
	for i, e := range eps {
		s := httptest.NewServer(e)
		t.Cleanup(s.Close)
		urls[i] = s.URL
	}
	return urls
}

func autoRoutes(pick string, hops int, eps []*scriptedEndpoint, urls []string) DoorRoutes {
	r := DoorRoute{Alias: "auto", Pick: pick, MaxHops: hops}
	for i, e := range eps {
		// The model id is the alias; the second candidate can call tools.
		r.Candidates = append(r.Candidates, DoorCandidate{Alias: e.name, Upstream: urls[i], Tools: i == 1})
	}
	return DoorRoutes{Revision: "a1", Routes: []DoorRoute{r}}
}

func doorPostRaw(t *testing.T, base, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// The fallback classes, one by one: each answer from the first candidate
// either moves the request to the second, or is the client's answer.
func TestDoorAutoFallsBackByErrorClass(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		falls  bool
	}{
		{"rate limited", http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`, true},
		{"no capacity", http.StatusServiceUnavailable, `{"error":{"message":"no worker"}}`, true},
		{"5xx before the first token", http.StatusBadGateway, `{"error":{"message":"engine gone"}}`, true},
		{"internal error", http.StatusInternalServerError, `{}`, true},
		{"context length (vLLM)", http.StatusBadRequest, `{"error":{"message":"This model's maximum context length is 4096 tokens"}}`, true},
		{"context length (llama.cpp)", http.StatusBadRequest, `{"error":{"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error"}}`, true},
		{"a bad request stays", http.StatusBadRequest, `{"error":{"message":"messages is required"}}`, false},
		{"a refused key stays", http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`, false},
		{"a content refusal stays", http.StatusBadRequest, `{"error":{"message":"content policy violation"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &scriptedEndpoint{name: "small", status: tc.status, body: tc.body}
			b := &scriptedEndpoint{name: "big"}
			eps := []*scriptedEndpoint{a, b}
			_, ts, _ := newTestDoor(t, autoRoutes("", 0, eps, scripted(t, a, b)))
			resp := doorPostRaw(t, ts.URL, `{"model":"auto","messages":[]}`)
			got, _ := io.ReadAll(resp.Body)
			if tc.falls {
				if resp.StatusCode != http.StatusOK || resp.Header.Get(doorEndpointHeader) != "big" || b.hits.Load() != 1 {
					t.Fatalf("%d %q from %q (b hits %d), want 200 from big", resp.StatusCode, got, resp.Header.Get(doorEndpointHeader), b.hits.Load())
				}
				if req, _ := b.lastReq.Load().(string); !strings.Contains(req, `"model":"big"`) {
					t.Fatalf("the fallback did not carry the body with big's model: %s", req)
				}
				return
			}
			if resp.StatusCode != tc.status || string(got) != tc.body || b.hits.Load() != 0 {
				t.Fatalf("%d %q (b hits %d), want small's own %d %q", resp.StatusCode, got, b.hits.Load(), tc.status, tc.body)
			}
			if resp.Header.Get(doorEndpointHeader) != "small" {
				t.Fatalf("X-Opod-Endpoint = %q, want small", resp.Header.Get(doorEndpointHeader))
			}
		})
	}
}

// An endpoint that does not answer at all is a fallback too.
func TestDoorAutoFallsBackFromAnUnreachableEndpoint(t *testing.T) {
	b := &scriptedEndpoint{name: "big"}
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	_, ts, _ := newTestDoor(t, DoorRoutes{Routes: []DoorRoute{{Alias: "auto", Candidates: []DoorCandidate{
		{Alias: "small", Upstream: deadURL}, {Alias: "big", Upstream: scripted(t, b)[0]},
	}}}})
	resp := doorPostRaw(t, ts.URL, `{"model":"auto"}`)
	if resp.StatusCode != http.StatusOK || resp.Header.Get(doorEndpointHeader) != "big" {
		t.Fatalf("%d from %q, want 200 from big", resp.StatusCode, resp.Header.Get(doorEndpointHeader))
	}
}

// One hop by default: with three failing candidates the client gets the
// second one's answer, and the third is never asked. MaxHops 2 reaches it.
func TestDoorAutoHopsAreBounded(t *testing.T) {
	for _, tc := range []struct {
		hops      int
		wantFrom  string
		wantCode  int
		thirdHits int64
	}{{0, "b", http.StatusServiceUnavailable, 0}, {2, "c", http.StatusOK, 1}} {
		a := &scriptedEndpoint{name: "a", status: http.StatusServiceUnavailable}
		b := &scriptedEndpoint{name: "b", status: http.StatusServiceUnavailable, body: `{"error":{"message":"b busy"}}`}
		c := &scriptedEndpoint{name: "c"}
		eps := []*scriptedEndpoint{a, b, c}
		_, ts, _ := newTestDoor(t, autoRoutes("first", tc.hops, eps, scripted(t, a, b, c)))
		resp := doorPostRaw(t, ts.URL, `{"model":"auto"}`)
		if resp.StatusCode != tc.wantCode || resp.Header.Get(doorEndpointHeader) != tc.wantFrom || c.hits.Load() != tc.thirdHits {
			t.Fatalf("maxHops %d: %d from %q, c hits %d; want %d from %s, c hits %d",
				tc.hops, resp.StatusCode, resp.Header.Get(doorEndpointHeader), c.hits.Load(), tc.wantCode, tc.wantFrom, tc.thirdHits)
		}
	}
}

// A request that carries tools skips a candidate that cannot call them; with
// none left it is a 400 that says why.
func TestDoorAutoToolsSkipCandidates(t *testing.T) {
	a := &scriptedEndpoint{name: "small"}
	b := &scriptedEndpoint{name: "big"} // candidate 1: tools
	eps := []*scriptedEndpoint{a, b}
	_, ts, _ := newTestDoor(t, autoRoutes("", 0, eps, scripted(t, a, b)))
	resp := doorPostRaw(t, ts.URL, `{"model":"auto","tools":[{"type":"function","function":{"name":"f"}}]}`)
	if resp.StatusCode != http.StatusOK || resp.Header.Get(doorEndpointHeader) != "big" || a.hits.Load() != 0 {
		t.Fatalf("%d from %q (small hits %d), want big only", resp.StatusCode, resp.Header.Get(doorEndpointHeader), a.hits.Load())
	}
	if resp := doorPostRaw(t, ts.URL, `{"model":"auto","tools":[]}`); resp.Header.Get(doorEndpointHeader) != "small" {
		t.Fatalf("an empty tools list is no tools: served by %q", resp.Header.Get(doorEndpointHeader))
	}
	// No candidate calls tools.
	routes := autoRoutes("", 0, eps, scripted(t, a, b))
	routes.Revision = "a2"
	routes.Routes[0].Candidates[1].Tools = false
	d, _, _ := newTestDoor(t, routes)
	rec := httptest.NewRecorder()
	d.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","tools":[{}]}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "can call them") {
		t.Fatalf("no tool-calling candidate: %d %s", rec.Code, rec.Body)
	}
}

// least-loaded starts with the candidate whose leader reports the least work
// per serving worker; one with no serving worker goes last.
func TestDoorAutoLeastLoaded(t *testing.T) {
	a := &scriptedEndpoint{name: "a", load: `{"workers":0}`}
	b := &scriptedEndpoint{name: "b", load: `{"workers":2,"in_flight":8,"queue_depth":2}`} // 5 each
	c := &scriptedEndpoint{name: "c", load: `{"workers":1,"in_flight":1}`}                 // 1
	eps := []*scriptedEndpoint{a, b, c}
	d, ts, _ := newTestDoor(t, autoRoutes("least-loaded", 0, eps, scripted(t, a, b, c)))
	resp := doorPostRaw(t, ts.URL, `{"model":"auto"}`)
	if resp.Header.Get(doorEndpointHeader) != "c" {
		t.Fatalf("served by %q, want c (the least work per worker)", resp.Header.Get(doorEndpointHeader))
	}
	order := d.byLoad(t.Context(), d.auto("auto").cands)
	if order[0].alias != "c" || order[1].alias != "b" || order[2].alias != "a" {
		t.Fatalf("order %s %s %s, want c b a", order[0].alias, order[1].alias, order[2].alias)
	}
}

// An auto alias is listed when any candidate accepts the key, and a route
// file that gets one wrong is refused whole.
func TestDoorAutoModelsAndValidation(t *testing.T) {
	a := &scriptedEndpoint{name: "a"}
	b := &scriptedEndpoint{name: "b"}
	urls := scripted(t, a, b)
	d, ts, _ := newTestDoor(t, DoorRoutes{Revision: "v1", Routes: []DoorRoute{
		{Alias: "a", Upstream: urls[0]},
		{Alias: "auto", Candidates: []DoorCandidate{{Alias: "a", Upstream: urls[0]}, {Alias: "b", Upstream: urls[1]}}},
	}})
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Data []struct{ ID string } `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	_ = resp.Body.Close()
	if len(list.Data) != 2 || list.Data[0].ID != "a" || list.Data[1].ID != "auto" {
		t.Fatalf("models %+v, want a and auto", list.Data)
	}
	for name, bad := range map[string]DoorRoute{
		"a pick nobody knows":     {Alias: "auto", Pick: "cheapest", Candidates: []DoorCandidate{{Alias: "a", Upstream: urls[0]}}},
		"a candidate twice":       {Alias: "auto", Candidates: []DoorCandidate{{Alias: "a", Upstream: urls[0]}, {Alias: "a", Upstream: urls[1]}}},
		"a candidate with no URL": {Alias: "auto", Candidates: []DoorCandidate{{Alias: "a"}}},
		"negative hops":           {Alias: "auto", MaxHops: -1, Candidates: []DoorCandidate{{Alias: "a", Upstream: urls[0]}}},
	} {
		if err := d.apply(mustJSON(t, DoorRoutes{Routes: []DoorRoute{bad}})); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := d.apply(mustJSON(t, DoorRoutes{Routes: []DoorRoute{{Alias: "auto", Upstream: urls[0]},
		{Alias: "auto", Candidates: []DoorCandidate{{Alias: "a", Upstream: urls[0]}}}}})); err == nil {
		t.Error("an auto alias that repeats a name route was accepted")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
