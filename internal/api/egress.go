// Egress proxies — when a request asks for a model owned by a third-party
// vendor (Anthropic, OpenAI), Opod forwards the request to that vendor's API
// using a team-scoped key, logs the call via usage + audit, and returns the
// vendor's response transparently.
//
// Configuration lives in config.Router.Fallback (env vars at minimum).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/metrics"
	"github.com/opod-io/opod/internal/store"
)

// FallbackConfig is what the gateway needs to proxy to upstream vendors.
type FallbackConfig struct {
	AnthropicKey string
	AnthropicURL string // default https://api.anthropic.com
	OpenAIKey    string
	OpenAIURL    string // default https://api.openai.com

	// Bedrock (AWS) — model names like `anthropic.claude-*` or
	// `amazon.titan-*` route here when BedrockRegion is set. Auth requires
	// AWS SigV4 signing using credentials from the standard AWS chain
	// (env, shared config, instance role). Signing implementation is
	// tracked as v0.7 in ROADMAP P1 — for now Vendor() recognizes the
	// model prefix and ServeBedrock returns a 501 with a clear setup hint.
	BedrockRegion string // e.g. us-east-1; empty disables routing
	BedrockURL    string // optional override; default https://bedrock-runtime.<region>.amazonaws.com

	// Vertex (GCP) — model names like `gemini-*` route here when
	// VertexProject is set. Auth uses Application Default Credentials
	// (gcloud auth, service account JSON, or workload identity). Same
	// v0.7 story as Bedrock.
	VertexProject  string // GCP project id; empty disables routing
	VertexLocation string // e.g. us-central1; default us-central1
	VertexURL      string // optional override; default https://<location>-aiplatform.googleapis.com

	// OpenAI-compatible hosted gateways. Each just needs a base URL and
	// a bearer key — request/response shape is the OpenAI shape we
	// already speak. Model ids are prefix-tagged in the request
	// (`openrouter/anthropic/claude-3-haiku`); the prefix is stripped
	// before forwarding so the upstream sees its native id.
	OpenRouterKey string
	OpenRouterURL string // default https://openrouter.ai/api/v1
	GroqKey       string
	GroqURL       string // default https://api.groq.com/openai/v1
	TogetherKey   string
	TogetherURL   string // default https://api.together.xyz/v1
	FireworksKey  string
	FireworksURL  string // default https://api.fireworks.ai/inference/v1
	CohereKey     string
	CohereURL     string // default https://api.cohere.com/compatibility/v1
	MistralKey    string
	MistralURL    string // default https://api.mistral.ai/v1
	PerplexityKey string
	PerplexityURL string // default https://api.perplexity.ai

	EnabledModels map[string]bool
}

// Vendor returns the vendor a model name belongs to (or "" if it's local).
// Order matters in two ways:
//   - The slash-namespaced gateway prefixes (openrouter/, groq/, …) are
//     matched FIRST so `openrouter/anthropic/claude-3-haiku` routes to
//     OpenRouter, not the plain Anthropic adapter.
//   - Bedrock-flavored Anthropic model IDs like
//     "anthropic.claude-3-sonnet-20240229-v1:0" carry the cloud prefix,
//     so they must beat the plain `claude-` rule.
func Vendor(model string) string {
	switch {
	// OpenAI-compatible hosted gateways. Slash-namespaced so the user's
	// intent (which gateway to use) is unambiguous.
	case strings.HasPrefix(model, "openrouter/"):
		return "openrouter"
	case strings.HasPrefix(model, "groq/"):
		return "groq"
	case strings.HasPrefix(model, "together/"):
		return "together"
	case strings.HasPrefix(model, "fireworks/"):
		return "fireworks"
	case strings.HasPrefix(model, "cohere/"):
		return "cohere"
	case strings.HasPrefix(model, "mistral/"):
		return "mistral"
	case strings.HasPrefix(model, "perplexity/"):
		return "perplexity"

	// Bedrock model IDs: "anthropic.*", "amazon.*", "meta.*", "mistral.*"
	case strings.HasPrefix(model, "anthropic."),
		strings.HasPrefix(model, "amazon."),
		strings.HasPrefix(model, "meta."),
		strings.HasPrefix(model, "mistral."),
		strings.HasPrefix(model, "ai21."),
		strings.HasPrefix(model, "cohere."):
		return "bedrock"
	// Vertex Gemini IDs: "gemini-*", "publishers/google/models/gemini-*"
	case strings.HasPrefix(model, "gemini-"),
		strings.Contains(model, "publishers/google/"):
		return "vertex"
	case strings.HasPrefix(model, "claude-"):
		return "anthropic"
	case strings.HasPrefix(model, "gpt-"),
		strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"),
		strings.HasPrefix(model, "o4"):
		return "openai"
	}
	// Registry providers (deepseek/, cerebras/, gemini/, …) are slash-
	// namespaced, so they're matched after the built-in rules and never
	// collide with the bare claude-/gpt-/gemini- prefixes above.
	return genericVendor(model)
}

// EgressHandler proxies requests to vendor APIs.
type EgressHandler struct {
	Store  store.Store
	Config FallbackConfig

	// Keys, when non-nil, holds per-vendor pools of API keys. The egress
	// layer rotates across them and parks any key that hits a 429 / 5xx so
	// a request can fail over to the user's next key for the same provider
	// instead of surfacing the rate-limit error. Nil → single-key behavior
	// (the legacy Config.XxxKey field is used as a pool of one).
	Keys *KeyPool

	// ProviderURLs holds resolved base URLs for registry providers
	// (GenericProviders), keyed by vendor name. Built at startup from each
	// provider's DefaultURL or its <NAME>_BASE_URL override.
	ProviderURLs map[string]string
}

// candidateKeys returns the rotation-ordered keys to try for a vendor: the
// multi-key pool when configured, otherwise the single legacy key. Empty
// result means the provider is unconfigured.
func (e *EgressHandler) candidateKeys(vendor, primary string) []string {
	if e.Keys != nil {
		if ks := e.Keys.Candidates(vendor); len(ks) > 0 {
			return ks
		}
	}
	if primary != "" {
		return []string{primary}
	}
	return nil
}

// ServeAnthropic proxies a /v1/messages request to the real Anthropic API.
func (e *EgressHandler) ServeAnthropic(w http.ResponseWriter, r *http.Request) {
	keys := e.candidateKeys("anthropic", e.Config.AnthropicKey)
	if len(keys) == 0 {
		writeAnthropicError(w, http.StatusServiceUnavailable, "configuration_error",
			"Anthropic fallback not configured; set ANTHROPIC_API_KEY")
		return
	}
	url := orDefault(e.Config.AnthropicURL, "https://api.anthropic.com") + r.URL.Path
	e.proxy(w, r, url, "anthropic", keys, func(key string) map[string]string {
		return map[string]string{
			"x-api-key":         key,
			"anthropic-version": "2023-06-01",
			"Content-Type":      "application/json",
			"User-Agent":        "opod/0.2",
		}
	})
}

// ServeBedrock proxies a /v1/messages request to AWS Bedrock for an
// `anthropic.*` model. The body format Bedrock-Anthropic expects is
// identical to Anthropic's own /v1/messages (Bedrock just adds SigV4
// signing), so we forward the body verbatim, signed with SigV4 via
// the standard AWS credentials chain (env, shared config, instance role).
//
// Other model families (amazon.*, meta.*, mistral.*) use Bedrock-specific
// body shapes — those return a 501 with the family-specific install hint
// until v0.7.
func (e *EgressHandler) ServeBedrock(w http.ResponseWriter, r *http.Request) {
	if e.Config.BedrockRegion == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "configuration_error",
			"Bedrock egress not configured; set router.fallback.bedrock_region (or OPOD_BEDROCK_REGION)")
		return
	}
	start := time.Now()
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read_error", err.Error())
		recordEgress(r.Context(), e.Store, "bedrock", "unknown", start, "error")
		return
	}
	model := peekVendorModel(body)
	if !strings.HasPrefix(model, "anthropic.") {
		writeJSONError(w, http.StatusNotImplemented, "not_implemented",
			"Bedrock body translation for "+model+" lands in v0.7 — "+
				"only anthropic.* models work today via Bedrock egress. "+
				"In the meantime, call Bedrock directly for non-Anthropic families.")
		recordEgress(r.Context(), e.Store, "bedrock", model, start, "not_implemented")
		return
	}
	// Bedrock expects the Anthropic body shape but DOES NOT want the "model"
	// field (the model id goes in the URL path). Strip it.
	stripped, err := stripModelField(body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "body_error", err.Error())
		recordEgress(r.Context(), e.Store, "bedrock", model, start, "error")
		return
	}
	// Also Bedrock requires "anthropic_version" in the body (not a header).
	stripped = ensureAnthropicVersion(stripped)

	if err := e.invokeBedrock(w, r, model, stripped); err != nil {
		writeJSONError(w, http.StatusBadGateway, "upstream_error", err.Error())
		recordEgress(r.Context(), e.Store, "bedrock", model, start, "error")
		return
	}
	recordEgress(r.Context(), e.Store, "bedrock", model, start, "ok")
}

// ServeVertex proxies to Google Vertex AI for a `gemini-*` model. As of v0.6
// ADC auth is wired (cloud.google.com/go/auth) so the credentials chain
// works (gcloud, service account JSON, workload identity). What's NOT yet
// wired is the body-shape translation from OpenAI / Anthropic message
// format → Vertex's `generateContent` Contents shape — that's v0.7.
//
// To make the 501 actionable we mint a token to confirm ADC actually works
// against the configured project, then return the error with project id.
func (e *EgressHandler) ServeVertex(w http.ResponseWriter, r *http.Request) {
	if e.Config.VertexProject == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "configuration_error",
			"Vertex egress not configured; set router.fallback.vertex_project (or OPOD_VERTEX_PROJECT)")
		return
	}
	model := peekVendorModelFromRequest(r)
	tokenOK := e.checkVertexADC(r.Context())
	hint := "ADC auth check: "
	if tokenOK == "" {
		hint += "✓ OK (token obtained for project " + e.Config.VertexProject + ")"
	} else {
		hint += "✗ FAILED — " + tokenOK
	}
	writeJSONError(w, http.StatusNotImplemented, "not_implemented",
		"Vertex egress: "+hint+". Body translation (OpenAI/Anthropic → "+
			"Vertex generateContent) lands in v0.7. In the meantime, call "+
			"Vertex directly for "+model+".")
	recordEgress(r.Context(), e.Store, "vertex", model, time.Now(), "not_implemented")
}

// peekVendorModelFromRequest sniffs the model name from the request body
// without consuming it for downstream handlers. Cheap; used only for usage
// metering on the stubbed Bedrock/Vertex paths.
func peekVendorModelFromRequest(r *http.Request) string {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "unknown"
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return peekVendorModel(body)
}

// ServeOpenAI proxies a /v1/chat/completions request to the real OpenAI API.
func (e *EgressHandler) ServeOpenAI(w http.ResponseWriter, r *http.Request) {
	keys := e.candidateKeys("openai", e.Config.OpenAIKey)
	if len(keys) == 0 {
		writeJSONError(w, http.StatusServiceUnavailable, "configuration_error",
			"OpenAI fallback not configured; set OPENAI_API_KEY")
		return
	}
	url := orDefault(e.Config.OpenAIURL, "https://api.openai.com") + r.URL.Path
	e.proxy(w, r, url, "openai", keys, bearerHeaders)
}

// bearerHeaders builds the standard Authorization: Bearer header set used by
// every OpenAI-shaped upstream. Passed to proxy() so the key can vary per
// rotation attempt.
func bearerHeaders(key string) map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + key,
		"Content-Type":  "application/json",
		"User-Agent":    "opod/0.2",
	}
}

// ServeOpenRouter / ServeGroq / ServeTogether / ServeFireworks all
// forward to an OpenAI-shape gateway. They share an implementation
// because the only differences are: (1) the prefix to strip from the
// model id, (2) the upstream URL, (3) the bearer key. The proxy()
// helper handles the rest — request streaming, response framing,
// usage record.
func (e *EgressHandler) ServeOpenRouter(w http.ResponseWriter, r *http.Request) {
	e.serveOpenAICompatible(w, r, "openrouter", "openrouter/",
		e.Config.OpenRouterKey,
		orDefault(e.Config.OpenRouterURL, "https://openrouter.ai/api/v1"),
		"OPENROUTER_API_KEY")
}
func (e *EgressHandler) ServeGroq(w http.ResponseWriter, r *http.Request) {
	e.serveOpenAICompatible(w, r, "groq", "groq/",
		e.Config.GroqKey,
		orDefault(e.Config.GroqURL, "https://api.groq.com/openai/v1"),
		"GROQ_API_KEY")
}
func (e *EgressHandler) ServeTogether(w http.ResponseWriter, r *http.Request) {
	e.serveOpenAICompatible(w, r, "together", "together/",
		e.Config.TogetherKey,
		orDefault(e.Config.TogetherURL, "https://api.together.xyz/v1"),
		"TOGETHER_API_KEY")
}
func (e *EgressHandler) ServeFireworks(w http.ResponseWriter, r *http.Request) {
	e.serveOpenAICompatible(w, r, "fireworks", "fireworks/",
		e.Config.FireworksKey,
		orDefault(e.Config.FireworksURL, "https://api.fireworks.ai/inference/v1"),
		"FIREWORKS_API_KEY")
}
func (e *EgressHandler) ServeCohere(w http.ResponseWriter, r *http.Request) {
	// Cohere ships an OpenAI-compatible surface at /compatibility/v1
	// that translates OpenAI message shape → Cohere's native shape
	// server-side, so we get the same identity treatment as the rest.
	e.serveOpenAICompatible(w, r, "cohere", "cohere/",
		e.Config.CohereKey,
		orDefault(e.Config.CohereURL, "https://api.cohere.com/compatibility/v1"),
		"COHERE_API_KEY")
}
func (e *EgressHandler) ServeMistral(w http.ResponseWriter, r *http.Request) {
	e.serveOpenAICompatible(w, r, "mistral", "mistral/",
		e.Config.MistralKey,
		orDefault(e.Config.MistralURL, "https://api.mistral.ai/v1"),
		"MISTRAL_API_KEY")
}
func (e *EgressHandler) ServePerplexity(w http.ResponseWriter, r *http.Request) {
	e.serveOpenAICompatible(w, r, "perplexity", "perplexity/",
		e.Config.PerplexityKey,
		orDefault(e.Config.PerplexityURL, "https://api.perplexity.ai"),
		"PERPLEXITY_API_KEY")
}

// serveOpenAICompatible reads the inbound body, strips the gateway
// prefix from the `model` field, then forwards to the upstream. The
// strip step is what makes `openrouter/anthropic/claude-3-haiku` work
// — OpenRouter sees the bare `anthropic/claude-3-haiku` and routes it
// natively.
func (e *EgressHandler) serveOpenAICompatible(w http.ResponseWriter, r *http.Request,
	vendor, modelPrefix, key, baseURL, envName string) {
	keys := e.candidateKeys(vendor, key)
	if len(keys) == 0 {
		writeJSONError(w, http.StatusServiceUnavailable, "configuration_error",
			fmt.Sprintf("%s egress not configured; set %s (or router.fallback.%s_url + key in config.yaml)",
				vendor, envName, vendor))
		return
	}
	// Read + rewrite the body. We have to read it anyway to log the
	// model id, so the prefix strip is essentially free.
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read_error", err.Error())
		return
	}
	body = stripModelPrefix(body, modelPrefix)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	url := baseURL + r.URL.Path
	e.proxy(w, r, url, vendor, keys, bearerHeaders)
}

// stripModelPrefix rewrites the JSON body so the `model` field has its
// gateway prefix removed. Returns the body unchanged when the prefix
// isn't present or the body isn't valid JSON — both are reasons to
// trust the upstream's own error message rather than fail eagerly.
func stripModelPrefix(body []byte, prefix string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	m, _ := obj["model"].(string)
	if !strings.HasPrefix(m, prefix) {
		return body
	}
	obj["model"] = strings.TrimPrefix(m, prefix)
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return rewritten
}

// hopByHopHeaders are the headers defined in RFC 7230 §6.1 that must not be
// forwarded by intermediaries. We also strip Content-Length so net/http on
// the outbound side recomputes it for our streamed response.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Content-Length":      true,
}

// copyUpstreamHeaders mirrors an upstream response's headers onto dst,
// skipping hop-by-hop headers (and Content-Length) so net/http recomputes the
// outbound framing. Shared by the egress proxy and the audio/rerank proxies —
// copying these verbatim alongside an io.Copy can produce a Content-Length /
// body mismatch when the upstream response is chunked.
func copyUpstreamHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopByHopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// proxy forwards the inbound request body to upstream and streams the
// response back. It tries each key in `keys` in order: when a key returns a
// rotatable status (429 / 5xx) or a transport error AND another key remains,
// that key is parked on the pool's cooldown and the next is tried. The last
// candidate is always streamed through verbatim so the client sees the real
// upstream error if every key is exhausted. headerFn builds the request
// headers for a given key, so the auth credential varies per attempt.
//
// We re-read the body into a fresh bytes.Reader per attempt so the outbound
// request carries an accurate Content-Length (the dispatcher wraps the body
// in io.NopCloser, masking the underlying *bytes.Reader from
// http.NewRequest's known-type fast-path).
func (e *EgressHandler) proxy(w http.ResponseWriter, r *http.Request, url, vendor string, keys []string, headerFn func(key string) map[string]string) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read_error", err.Error())
		recordEgress(ctx, e.Store, vendor, peekVendorModel(body), start, "error")
		return
	}
	model := peekVendorModel(body)

	for i, key := range keys {
		last := i == len(keys)-1

		req, err := http.NewRequestWithContext(ctx, r.Method, url, bytes.NewReader(body))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "proxy_error", err.Error())
			recordEgress(ctx, e.Store, vendor, model, start, "error")
			return
		}
		req.ContentLength = int64(len(body))
		for k, v := range headerFn(key) {
			req.Header.Set(k, v)
		}
		if accept := r.Header.Get("Accept"); accept != "" {
			req.Header.Set("Accept", accept)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			// Transport failure (DNS, connect, timeout). Rotate to the next
			// key unless this was the last candidate.
			if !last {
				e.penalize(ctx, vendor, key, model, 5*time.Second)
				continue
			}
			writeJSONError(w, http.StatusBadGateway, "upstream_error", err.Error())
			recordEgress(ctx, e.Store, vendor, model, start, "error")
			return
		}

		// Rotatable upstream status (rate limit / transient 5xx): park this
		// key and try the next, but only while another remains. Nothing has
		// been written to the client yet, so failover is transparent.
		if !last && retryableStatus(resp.StatusCode) {
			e.penalize(ctx, vendor, key, model, retryAfter(resp))
			drain(resp.Body)
			_ = resp.Body.Close()
			continue
		}

		e.stream(w, r, ctx, resp, vendor, model, start)
		return
	}
}

// stream mirrors the upstream response headers/status to the client and
// copies the body through, flushing for SSE. Records the final usage row.
func (e *EgressHandler) stream(w http.ResponseWriter, r *http.Request, ctx context.Context, resp *http.Response, vendor, model string, start time.Time) {
	defer resp.Body.Close()

	// Mirror response headers EXCEPT hop-by-hop. Let net/http compute the
	// outbound framing (chunked) on its own.
	copyUpstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		// Check for client disconnect between iterations.
		if r.Context().Err() != nil {
			recordEgress(ctx, e.Store, vendor, model, start, "cancelled")
			return
		}
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := w.Write(buf[:n]); wErr != nil {
				recordEgress(ctx, e.Store, vendor, model, start, "error")
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				recordEgress(ctx, e.Store, vendor, model, start, "error")
				return
			}
			break
		}
	}
	outcome := "ok"
	if resp.StatusCode >= 400 {
		outcome = "error"
	}
	recordEgress(ctx, e.Store, vendor, model, start, outcome)
}

// penalize parks a key on the pool's cooldown and records the rotation in the
// audit log (not usage — a rotated attempt served no tokens, so counting it
// would inflate accounting). No-op on the pool when single-key.
func (e *EgressHandler) penalize(ctx context.Context, vendor, key, model string, d time.Duration) {
	if e.Keys != nil {
		e.Keys.Penalize(vendor, key, d)
	}
	if e.Store != nil {
		_ = e.Store.Audit().Record(ctx, store.AuditEntry{
			TS: time.Now(), Action: "egress.rotate." + vendor, Target: model,
		})
	}
}

// retryableStatus reports whether an upstream status warrants rotating to the
// next key: rate limits and transient server errors. A 4xx other than 429
// (bad request, auth failure) is the client's problem and is surfaced as-is.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// retryAfter extracts a cooldown from a rotatable response: the Retry-After
// header (delta-seconds or HTTP-date) when present, else a status-appropriate
// default (longer for an explicit 429 than a transient 5xx).
func retryAfter(resp *http.Response) time.Duration {
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return 30 * time.Second
	}
	return 5 * time.Second
}

// drain discards up to 1 MiB of a response body so the underlying connection
// can be reused before we move on to the next key.
func drain(rc io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
}

// peekVendorModel decodes just the "model" field from a JSON body. Returns ""
// if absent or unparseable.
func peekVendorModel(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	if m.Model == "" {
		return "unknown"
	}
	return m.Model
}

func recordEgress(ctx context.Context, st store.Store, vendor, model string, start time.Time, outcome string) {
	dur := time.Since(start)
	// Bound Prometheus label cardinality: a failed egress call may carry
	// an arbitrary client-supplied model string — label those "unknown".
	// Successful vendor calls keep the vendor model label, and the usage
	// + audit rows below keep the raw string for debugging either way.
	metricsModel := model
	if outcome != "ok" {
		metricsModel = "unknown"
	}
	metrics.ObserveRequest(metricsModel, vendor, outcome, dur, 0, 0)
	key := auth.KeyFrom(ctx)
	keyID, userID := "", ""
	if key != nil {
		keyID = key.ID
		userID = key.UserID
	}
	_ = st.Usage().Record(ctx, store.Usage{
		TS: time.Now(), APIKeyID: keyID, UserID: userID,
		Model: model, Protocol: vendor,
		LatencyMS: int(dur.Milliseconds()), Outcome: outcome,
	})
	_ = st.Audit().Record(ctx, store.AuditEntry{
		TS: time.Now(), Actor: userID,
		Action: fmt.Sprintf("egress.%s", vendor),
		Target: model,
	})
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
