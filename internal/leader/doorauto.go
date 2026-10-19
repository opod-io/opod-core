package leader

// Auto routes at the cell door (ADR-092 §8): `model: "auto"`, or any name the
// owner defines, resolves to an ORDERED list of endpoints.
//
//   - The door picks a first try by the route's pick rule — "first" (the
//     owner's order as written) or "least-loaded" (each candidate's own
//     /loadz: queued plus in-flight requests per serving worker; a candidate
//     with no serving worker, or that does not answer, goes last). The order
//     is the owner's; nothing in it is a price (ADR-003).
//   - It falls back down the list, at most MaxHops times (default 1), when the
//     answer is one another endpoint may not give: 429, any 5xx (a status line
//     comes before the first token, so a stream that already started is never
//     retried), a context-length refusal, or no answer at all. Every other
//     answer — 2xx, a 401 for a key that endpoint does not hold, a 400 for a
//     bad request — is the client's, from the endpoint that gave it.
//   - A request that carries `tools` skips a candidate whose model cannot
//     call them.
//   - X-Opod-Endpoint names the endpoint that served. That endpoint's leader
//     records the usage, as for any request: a door has none of its own.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/opod-io/opod-sdk/adminapi"

	"github.com/opod-io/opod/internal/router"
)

const (
	// doorEndpointHeader names, on every answer through the door, the alias
	// of the endpoint that served it.
	doorEndpointHeader = "X-Opod-Endpoint"

	doorPickFirst       = "first"
	doorPickLeastLoaded = "least-loaded"

	// doorLoadTTL is how long one candidate's /loadz is reused for a
	// least-loaded pick: a probe per candidate every two seconds at most,
	// never one per request.
	doorLoadTTL = 2 * time.Second
	// doorLoadProbe bounds one /loadz call; a candidate that does not answer
	// in time is ordered last, not waited for.
	doorLoadProbe = time.Second
	// doorErrorPeek is how much of a 400 the door reads to tell a
	// context-length refusal from any other.
	doorErrorPeek = 64 << 10
)

// errDoorNextCandidate is how an attempt's ModifyResponse says "this answer is
// one the next candidate may do better on".
var errDoorNextCandidate = errors.New("door: the next candidate may take this request")

// doorAuto is one auto route made usable.
type doorAuto struct {
	alias string
	cands []*doorUpstream
	pick  string
	hops  int
}

// candidateLoad is one candidate's last /loadz, reduced to what a pick needs.
type candidateLoad struct {
	at    time.Time
	ok    bool    // answered, with at least one serving worker
	score float64 // (in flight + queued) per serving worker
}

// autoRoute validates one route that names candidates.
func (d *CellDoor) autoRoute(alias string, r DoorRoute) (*doorAuto, error) {
	pick := strings.TrimSpace(r.Pick)
	switch pick {
	case "":
		pick = doorPickFirst
	case doorPickFirst, doorPickLeastLoaded:
	default:
		return nil, fmt.Errorf("pick %q is neither %q nor %q", r.Pick, doorPickFirst, doorPickLeastLoaded)
	}
	if r.MaxHops < 0 {
		return nil, fmt.Errorf("maxHops %d is negative", r.MaxHops)
	}
	a := &doorAuto{alias: alias, pick: pick, hops: max(r.MaxHops, 1)}
	seen := map[string]bool{}
	for i, c := range r.Candidates {
		ca := strings.TrimSpace(c.Alias)
		if ca == "" {
			return nil, fmt.Errorf("candidate %d has no alias", i)
		}
		if seen[ca] {
			return nil, fmt.Errorf("candidate %q is listed twice", ca)
		}
		seen[ca] = true
		model := strings.TrimSpace(c.Model)
		if model == "" {
			model = ca // as for a name route: empty = the alias IS the model id
		}
		up, err := d.upstream(ca, c.Upstream, model, c.CA)
		if err != nil {
			return nil, fmt.Errorf("candidate %q: %w", ca, err)
		}
		up.tools = c.Tools
		a.cands = append(a.cands, up)
	}
	a.hops = min(a.hops, len(a.cands)-1)
	return a, nil
}

// carriesTools says the request's `tools` is a non-empty array.
func carriesTools(raw json.RawMessage) bool {
	var tools []json.RawMessage
	return json.Unmarshal(raw, &tools) == nil && len(tools) > 0
}

// forwardAuto sends one request down an auto route's candidates.
func (d *CellDoor) forwardAuto(w http.ResponseWriter, r *http.Request, a *doorAuto, body []byte, tools bool) {
	cands := make([]*doorUpstream, 0, len(a.cands))
	for _, c := range a.cands {
		if !tools || c.tools {
			cands = append(cands, c)
		}
	}
	if len(cands) == 0 {
		doorError(w, http.StatusBadRequest, "invalid_request_error", "",
			fmt.Sprintf("the request carries tools and no endpoint of model %q can call them", a.alias))
		return
	}
	if a.pick == doorPickLeastLoaded {
		cands = d.byLoad(r.Context(), cands)
	}
	tries := min(len(cands), a.hops+1)
	for i := range tries {
		up, last := cands[i], i == tries-1
		b, err := replaceModel(body, up.model)
		if err != nil {
			doorError(w, http.StatusBadRequest, "invalid_request_error", "", "the request body is not a JSON object: "+err.Error())
			return
		}
		out := r.Clone(r.Context())
		out.Body, out.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
		out.Header.Del("Content-Length")
		next := false
		p := d.proxy(up)
		if !last {
			p.ModifyResponse = func(resp *http.Response) error {
				if why := doorTryNext(resp); why != "" {
					d.log.Info("door auto route falls back", "alias", a.alias, "from", up.alias, "to", cands[i+1].alias, "why", why)
					return errDoorNextCandidate
				}
				resp.Header.Set(doorEndpointHeader, up.alias)
				return nil
			}
			p.ErrorHandler = func(_ http.ResponseWriter, req *http.Request, err error) {
				if req.Context().Err() != nil {
					return // the client left
				}
				if !errors.Is(err, errDoorNextCandidate) {
					d.log.Info("door auto route falls back", "alias", a.alias, "from", up.alias, "to", cands[i+1].alias, "why", "unreachable: "+err.Error())
				}
				next = true // nothing was written: the next candidate answers
			}
		}
		p.ServeHTTP(w, out)
		if !next {
			return
		}
	}
}

// doorTryNext says why an answer should go to the next candidate, or "" when
// it is the client's. A 400 is read only far enough to classify it, and put
// back whole when it stays.
func doorTryNext(resp *http.Response) string {
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return resp.Status
	case resp.StatusCode != http.StatusBadRequest:
		return ""
	}
	peek, err := io.ReadAll(io.LimitReader(resp.Body, doorErrorPeek))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peek), resp.Body), resp.Body}
	if err == nil && router.ClassifyError(errors.New(string(peek))) == router.ClassContextLength {
		return "context length"
	}
	return ""
}

// byLoad orders candidates for a least-loaded pick: serving candidates by
// load, then the rest, the owner's order breaking every tie.
func (d *CellDoor) byLoad(ctx context.Context, cands []*doorUpstream) []*doorUpstream {
	loads := make([]candidateLoad, len(cands))
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loads[i] = d.loadOf(ctx, c)
		}()
	}
	wg.Wait()
	idx := make([]int, len(cands))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(x, y int) bool {
		lx, ly := loads[idx[x]], loads[idx[y]]
		if lx.ok != ly.ok {
			return lx.ok
		}
		return lx.ok && lx.score < ly.score
	})
	out := make([]*doorUpstream, len(cands))
	for i, j := range idx {
		out[i] = cands[j]
	}
	return out
}

// loadOf is one candidate's load, from its /loadz (unauthenticated, a probe)
// or the copy read within doorLoadTTL.
func (d *CellDoor) loadOf(ctx context.Context, up *doorUpstream) candidateLoad {
	key := up.url.String()
	d.lmu.Lock()
	if l, ok := d.loads[key]; ok && time.Since(l.at) < doorLoadTTL {
		d.lmu.Unlock()
		return l
	}
	d.lmu.Unlock()
	l := candidateLoad{at: time.Now()}
	ctx, cancel := context.WithTimeout(ctx, doorLoadProbe)
	defer cancel()
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, up.url.JoinPath("loadz").String(), nil); err == nil {
		if resp, err := (&http.Client{Transport: up.rt}).Do(req); err == nil {
			var ld adminapi.Load
			if resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ld) == nil && ld.Workers > 0 {
				l.ok, l.score = true, float64(ld.InFlight+ld.QueueDepth)/float64(ld.Workers)
			}
			_ = resp.Body.Close()
		}
	}
	d.lmu.Lock()
	if len(d.loads) >= doorModelsCacheMax {
		d.loads = map[string]candidateLoad{}
	}
	d.loads[key] = l
	d.lmu.Unlock()
	return l
}
