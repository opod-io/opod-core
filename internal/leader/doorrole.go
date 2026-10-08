package leader

// The cell door (ADR-087, feature `door`): ONE URL for many endpoints.
//
// An endpoint is one leader serving one model identity (D1/D5), so a cell of
// five models is five base URLs — and every OpenAI-compatible service a client
// has used is one base URL where `model` chooses. A door closes that gap and
// does nothing else:
//
//   - it reads the request's `model`, finds the endpoint that answers it in a
//     watched routes file (adminapi.DoorRoutes), and forwards the request
//     there unchanged — streaming included, flushed per chunk;
//   - it chooses no worker, judges no load, retries nothing and falls back to
//     nothing. Choosing a worker stays the endpoint leader's job, so there is
//     still exactly one router per endpoint;
//   - it holds no credential. `Authorization` is forwarded as received and the
//     endpoint's leader verifies it, so a key for endpoint A sent with B's
//     model is refused by B's leader, exactly as if it had been sent to B;
//   - GET /v1/models asks each mapped endpoint with the caller's key and lists
//     the aliases whose endpoint accepted it.
//
// It is not a gateway (gatewayrole.go): a gateway is a copied front of ONE
// endpoint that mirrors its registry and pushes its usage. A door has no
// store, no registry and no usage of its own; the leaders behind it record
// every request as they always did.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/opod-io/opod-sdk/adminapi"

	"github.com/opod-io/opod/internal/api"
	"github.com/opod-io/opod/internal/config"
)

// DoorRoutes is the routes file's wire type (opod-sdk/adminapi).
type (
	DoorRoutes = adminapi.DoorRoutes
	DoorRoute  = adminapi.DoorRoute
)

const (
	// doorRoutesEvery is how often the routes file is re-read, the same period
	// as the plan, auth and policy files.
	doorRoutesEvery = 10 * time.Second
	// doorModelsTTL is how long one key's /v1/models answer is reused. Short:
	// a key revoked on an endpoint should leave the list within seconds, and
	// what it saves is N upstream calls per list, not a request's latency.
	doorModelsTTL = 10 * time.Second
	// doorModelsProbe bounds each upstream's /v1/models answer; an endpoint
	// that does not answer in time is left out of that list, not waited for.
	doorModelsProbe = 5 * time.Second
	// doorModelsCacheMax bounds the per-key cache, so a stream of random keys
	// cannot grow it without limit.
	doorModelsCacheMax = 4096
)

// CellDoor is the door process: a routes file, an HTTP surface and nothing
// else.
type CellDoor struct {
	cfg  *config.Config
	log  *slog.Logger
	path string

	mu       sync.RWMutex
	routes   map[string]*doorUpstream // alias → upstream
	revision string
	loadedAt time.Time
	loaded   bool
	lastErr  string

	// transports are shared per trust (the CA PEM, "" for the system roots):
	// one connection pool per distinct certificate, kept across reloads.
	tmu        sync.Mutex
	transports map[string]http.RoundTripper

	cmu    sync.Mutex
	models map[string]doorModelsEntry // sha256(Authorization) → answer
}

// doorUpstream is one route made usable: the parsed URL and its transport.
type doorUpstream struct {
	alias string
	model string
	url   *url.URL
	rt    http.RoundTripper
}

type doorModelsEntry struct {
	at  time.Time
	ids []string
}

// NewCellDoor prepares a door over the routes file at path. It refuses an
// empty path: a door with nothing to route would answer every request 404,
// which reads as a missing model rather than a missing configuration.
func NewCellDoor(cfg *config.Config, log *slog.Logger, path string) (*CellDoor, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("OPOD_ROLE=door needs a routes file (--routes / OPOD_DOOR_ROUTES): the door forwards each model to the endpoint the file names")
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &CellDoor{cfg: cfg, log: log, path: path, routes: map[string]*doorUpstream{},
		transports: map[string]http.RoundTripper{}, models: map[string]doorModelsEntry{}}, nil
}

// Run watches the routes file and serves until ctx ends.
func (d *CellDoor) Run(ctx context.Context) error {
	d.watchRoutes(ctx)
	listen := d.cfg.Listen
	if listen == "" {
		listen = ":8080"
	}
	srv := &http.Server{Addr: listen, Handler: d.Handler(), ReadHeaderTimeout: 30 * time.Second}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", listen, err)
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	d.log.Info("door role", "listen", listen, "routes", d.path, "serves", "/v1 by model name", "admin_surface", "none")
	if d.cfg.TLSCert != "" && d.cfg.TLSKey != "" {
		err = srv.ServeTLS(ln, d.cfg.TLSCert, d.cfg.TLSKey)
	} else {
		err = srv.Serve(ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// watchRoutes reads the routes file now and every doorRoutesEvery, the shape
// of the plan, auth and policy watchers: a file that is absent keeps the
// watcher running (the control plane may write it after the door starts), and
// a file that does not parse keeps the last good routes.
func (d *CellDoor) watchRoutes(ctx context.Context) {
	var lastMod time.Time
	load := func() {
		st, err := os.Stat(d.path)
		if err != nil || !st.ModTime().After(lastMod) {
			return
		}
		raw, err := os.ReadFile(d.path)
		if err != nil {
			return
		}
		lastMod = st.ModTime()
		if err := d.apply(raw); err != nil {
			d.mu.Lock()
			d.lastErr = err.Error()
			d.mu.Unlock()
			d.log.Warn("door routes unreadable — keeping the last good routes", "path", d.path, "err", err)
		}
	}
	load()
	go func() {
		t := time.NewTicker(doorRoutesEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				load()
			}
		}
	}()
}

// apply validates a routes document and swaps it in whole, or not at all.
func (d *CellDoor) apply(raw []byte) error {
	var doc DoorRoutes
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	next := make(map[string]*doorUpstream, len(doc.Routes))
	for i, r := range doc.Routes {
		alias := strings.TrimSpace(r.Alias)
		if alias == "" {
			return fmt.Errorf("route %d has no alias", i)
		}
		if _, dup := next[alias]; dup {
			return fmt.Errorf("alias %q is listed twice", alias)
		}
		u, err := url.Parse(strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(r.Upstream), "/"), "/v1"))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("route %q: upstream %q is not an http(s) base URL", alias, r.Upstream)
		}
		rt, err := d.transport(r.CA)
		if err != nil {
			return fmt.Errorf("route %q: %w", alias, err)
		}
		next[alias] = &doorUpstream{alias: alias, model: strings.TrimSpace(r.Model), url: u, rt: rt}
	}
	d.mu.Lock()
	changed := !d.loaded || d.revision != doc.Revision
	d.routes, d.revision, d.loadedAt, d.loaded, d.lastErr = next, doc.Revision, time.Now(), true, ""
	d.mu.Unlock()
	// A list answered under the old routes may name an alias that is gone.
	d.cmu.Lock()
	d.models = map[string]doorModelsEntry{}
	d.cmu.Unlock()
	if changed {
		d.log.Info("door routes applied", "revision", doc.Revision, "routes", len(next))
	}
	return nil
}

// transport is the round tripper for one trust: the system roots when ca is
// empty, exactly the certificates in ca otherwise — the way a worker trusts
// its leader (OPOD_LEADER_CA). One per distinct ca, so connections are reused.
func (d *CellDoor) transport(ca string) (http.RoundTripper, error) {
	ca = strings.TrimSpace(ca)
	d.tmu.Lock()
	defer d.tmu.Unlock()
	if rt, ok := d.transports[ca]; ok {
		return rt, nil
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	if ca != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(ca)) {
			return nil, errors.New("ca holds no PEM certificate")
		}
		t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	d.transports[ca] = t
	return t, nil
}

// route is the upstream for one alias, or nil.
func (d *CellDoor) route(alias string) *doorUpstream {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.routes[alias]
}

// Handler is the door's whole HTTP surface.
func (d *CellDoor) Handler() http.Handler {
	chat := d.cfg.MaxBodyBytes
	if chat <= 0 {
		chat = defaultMaxBodyBytes
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "role": "door"})
	})
	mux.HandleFunc("GET /readyz", d.readyz)
	mux.HandleFunc("GET /gatewayz", d.gatewayz)
	mux.HandleFunc("GET /v1/models", d.listModels)
	mux.HandleFunc("GET /v1/models/{id}", d.getModel)
	mux.HandleFunc("POST /v1/", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, bodyCapFor(r.URL.Path, chat))
		d.forward(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{
			"message": "a door forwards OpenAI requests by their model (POST /v1/…) and lists models (GET /v1/models); nothing else is served here",
			"type":    "not_found"}})
	})
	return mux
}

// readyz is ready once a routes file has been read: before that every
// request would be a 404 that reads as a missing model.
func (d *CellDoor) readyz(w http.ResponseWriter, _ *http.Request) {
	d.mu.RLock()
	loaded, n := d.loaded, len(d.routes)
	d.mu.RUnlock()
	if !loaded {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "role": "door",
			"reason": "no routes file read yet at " + d.path})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true, "role": "door", "routes": n})
}

// gatewayz answers GET /gatewayz for a door: what it routes from and how old
// that is. The gateway fields (doors, spend, admission) are a gateway's, of
// ONE endpoint; a door has none of them and does not pretend to.
func (d *CellDoor) gatewayz(w http.ResponseWriter, _ *http.Request) {
	d.mu.RLock()
	aliases := make([]string, 0, len(d.routes))
	for a := range d.routes {
		aliases = append(aliases, a)
	}
	out := map[string]any{
		"role":            "door",
		"routes_file":     d.path,
		"routes_loaded":   d.loaded,
		"routes_revision": d.revision,
		"routes":          len(d.routes),
	}
	if d.loaded {
		out["routes_age_s"] = int(time.Since(d.loadedAt).Seconds())
	}
	if d.lastErr != "" {
		out["routes_error"] = d.lastErr
	}
	d.mu.RUnlock()
	sort.Strings(aliases)
	out["aliases"] = aliases
	writeJSON(w, http.StatusOK, out)
}

// forward sends one /v1 request to the endpoint its model names.
func (d *CellDoor) forward(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		api.BodyReadError(w, err)
		return
	}
	var probe struct {
		Model *string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		doorError(w, http.StatusBadRequest, "invalid_request_error", "", "the request body is not JSON: "+err.Error())
		return
	}
	alias := ""
	if probe.Model != nil {
		alias = strings.TrimSpace(*probe.Model)
	}
	up := d.route(alias)
	if up == nil {
		doorModelNotFound(w, alias)
		return
	}
	if up.model != "" && up.model != alias {
		if body, err = replaceModel(body, up.model); err != nil {
			doorError(w, http.StatusBadRequest, "invalid_request_error", "", "the request body is not a JSON object: "+err.Error())
			return
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Del("Content-Length")
	d.proxy(up).ServeHTTP(w, r)
}

// proxy is the stdlib reverse proxy aimed at one upstream. FlushInterval -1
// writes every chunk as it arrives, so a stream reaches the client token by
// token whatever its content type. Headers pass as received —
// Authorization included — minus the hop-by-hop ones.
func (d *CellDoor) proxy(up *doorUpstream) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up.url)
			pr.SetXForwarded()
		},
		Transport:     up.rt,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // the client left; nobody is reading an answer
			}
			d.log.Warn("door upstream unreachable", "alias", up.alias, "upstream", up.url.String(), "err", err)
			// No retry and no fallback (ADR-087): the endpoint is unreachable,
			// and saying so is the answer.
			doorError(w, http.StatusBadGateway, "upstream_unavailable", "",
				fmt.Sprintf("the endpoint for model %q did not answer the door", up.alias))
		},
	}
}

// replaceModel sets the body's top-level `model` and leaves every other value
// byte for byte as the client sent it.
func replaceModel(body []byte, model string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	m, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	obj["model"] = m
	return json.Marshal(obj)
}

// listModels answers GET /v1/models: the aliases whose endpoint accepts the
// caller's key, asked of each endpoint with that key.
func (d *CellDoor) listModels(w http.ResponseWriter, r *http.Request) {
	ids := d.acceptedAliases(r)
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "opod"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// getModel answers GET /v1/models/{id} from the same per-key list, so a model
// whose endpoint refuses the key is as absent here as in the list.
func (d *CellDoor) getModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, a := range d.acceptedAliases(r) {
		if a == id {
			writeJSON(w, http.StatusOK, map[string]any{"id": id, "object": "model", "owned_by": "opod"})
			return
		}
	}
	doorModelNotFound(w, id)
}

// acceptedAliases is the sorted aliases whose endpoint answers 200 to
// /v1/models with the caller's Authorization. Cached per key for
// doorModelsTTL under the key's SHA-256 — the raw key is never held.
func (d *CellDoor) acceptedAliases(r *http.Request) []string {
	auth := r.Header.Get("Authorization")
	sum := sha256.Sum256([]byte(auth))
	key := hex.EncodeToString(sum[:])
	d.cmu.Lock()
	if e, ok := d.models[key]; ok && time.Since(e.at) < doorModelsTTL {
		d.cmu.Unlock()
		return e.ids
	}
	d.cmu.Unlock()

	d.mu.RLock()
	ups := make([]*doorUpstream, 0, len(d.routes))
	for _, u := range d.routes {
		ups = append(ups, u)
	}
	d.mu.RUnlock()
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids = make([]string, 0, len(ups))
	)
	for _, up := range ups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d.accepts(r.Context(), up, auth) {
				mu.Lock()
				ids = append(ids, up.alias)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	sort.Strings(ids)
	if r.Context().Err() == nil {
		d.cmu.Lock()
		if len(d.models) >= doorModelsCacheMax {
			d.models = map[string]doorModelsEntry{}
		}
		d.models[key] = doorModelsEntry{at: time.Now(), ids: ids}
		d.cmu.Unlock()
	}
	return ids
}

// accepts asks one endpoint whether it serves this caller.
func (d *CellDoor) accepts(ctx context.Context, up *doorUpstream, auth string) bool {
	ctx, cancel := context.WithTimeout(ctx, doorModelsProbe)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, up.url.JoinPath("v1", "models").String(), nil)
	if err != nil {
		return false
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := (&http.Client{Transport: up.rt}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode == http.StatusOK
}

// doorModelNotFound is OpenAI's own answer for a model nobody serves here.
// An empty model and "auto" land here too: a door looks names up and chooses
// nothing (ADR-087 §5).
func doorModelNotFound(w http.ResponseWriter, model string) {
	msg := fmt.Sprintf("The model `%s` does not exist or you do not have access to it.", model)
	if model == "" {
		msg = "The request names no model; a door forwards each request to the endpoint its model names."
	}
	doorError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", msg)
}

// doorError writes OpenAI's error shape: {error: {message, type, param, code}}.
func doorError(w http.ResponseWriter, status int, typ, code, msg string) {
	e := map[string]any{"message": msg, "type": typ, "param": nil, "code": nil}
	if code != "" {
		e["code"] = code
		e["param"] = "model"
	}
	writeJSON(w, status, map[string]any{"error": e})
}
