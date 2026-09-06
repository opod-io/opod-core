package api

import (
	"fmt"
	"net/http"
	"os"
	"strings"
)

// GenericProvider describes an upstream that speaks the OpenAI
// /v1/chat/completions + /v1/embeddings shape and needs nothing more than a
// base URL and a bearer key. Adding a provider is a single table row below —
// the rotation/failover engine, prefix routing, and dispatch all derive from
// this registry.
//
// Models are addressed with a slash-namespaced prefix (e.g.
// `deepseek/deepseek-chat`, `cerebras/llama-3.3-70b`); the prefix is stripped
// before forwarding so the upstream sees its native id. Keys come from
// `<EnvKey>` plus numbered siblings `<EnvKey>_2..N` (rotation pool). The base
// URL may be overridden per provider with `<urlEnv>` (DefaultURL otherwise).
type GenericProvider struct {
	Name       string // vendor id == model prefix (sans slash): "deepseek"
	DefaultURL string // OpenAI-compatible base URL; "" → user must set the URL env
	EnvKey     string // primary key env var
}

// urlEnv is the env var that overrides a provider's base URL: e.g. provider
// "deepseek" → DEEPSEEK_BASE_URL.
func (p GenericProvider) urlEnv() string {
	return strings.ToUpper(strings.ReplaceAll(p.Name, "-", "_")) + "_BASE_URL"
}

// GenericProviders is the registry of OpenAI-compatible upstreams Opod can
// rotate keys across. Defaults are set only where the endpoint is stable and
// known; the rest require the operator to supply <NAME>_BASE_URL (we refuse
// to ship a guessed URL). All are overridable via that env var.
var GenericProviders = []GenericProvider{
	// Stable, well-known OpenAI-compatible endpoints.
	{"deepseek", "https://api.deepseek.com/v1", "DEEPSEEK_API_KEY"},
	{"cerebras", "https://api.cerebras.ai/v1", "CEREBRAS_API_KEY"},
	{"nvidia", "https://integrate.api.nvidia.com/v1", "NVIDIA_API_KEY"},
	{"gemini", "https://generativelanguage.googleapis.com/v1beta/openai", "GEMINI_API_KEY"},
	{"huggingface", "https://router.huggingface.co/v1", "HF_TOKEN"},

	// Endpoint varies by account/region or has shifted recently — require an
	// explicit <NAME>_BASE_URL rather than baking a stale guess.
	{"zai", "", "ZAI_API_KEY"},                   // Z.ai / Zhipu OpenAI-compat surface
	{"ollama-cloud", "", "OLLAMA_CLOUD_API_KEY"}, // ollama.com hosted
	{"github", "", "GITHUB_MODELS_TOKEN"},        // GitHub Models
	{"cloudflare", "", "CLOUDFLARE_API_KEY"},     // account-scoped (.../accounts/<id>/ai/v1)
	{"ovh", "", "OVH_API_KEY"},                   // OVHcloud AI Endpoints
	{"kilo", "", "KILO_API_KEY"},                 // Kilo Gateway
	{"pollinations", "", "POLLINATIONS_API_KEY"}, // Pollinations
	{"llm7", "", "LLM7_API_KEY"},                 // LLM7
	{"opencode-zen", "", "OPENCODE_ZEN_API_KEY"}, // OpenCode Zen
}

var genericByName = func() map[string]GenericProvider {
	m := make(map[string]GenericProvider, len(GenericProviders))
	for _, p := range GenericProviders {
		m[p.Name] = p
	}
	return m
}()

// LookupGeneric returns the registry entry for a vendor name, if any.
func LookupGeneric(name string) (GenericProvider, bool) {
	p, ok := genericByName[name]
	return p, ok
}

// genericVendor returns the registry vendor for a slash-namespaced model id
// (`deepseek/...` → "deepseek"), or "" if the prefix isn't a registry entry.
func genericVendor(model string) string {
	i := strings.IndexByte(model, '/')
	if i <= 0 {
		return ""
	}
	if _, ok := genericByName[model[:i]]; ok {
		return model[:i]
	}
	return ""
}

// ProviderKeysFromEnv reads a generic provider's rotation keys from the
// environment: <EnvKey> plus numbered overflow <EnvKey>_2..20.
func ProviderKeysFromEnv(p GenericProvider) []string {
	var out []string
	if v := strings.TrimSpace(os.Getenv(p.EnvKey)); v != "" {
		out = append(out, v)
	}
	for i := 2; i <= 20; i++ {
		if v := strings.TrimSpace(os.Getenv(fmt.Sprintf("%s_%d", p.EnvKey, i))); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ProviderURLFromEnv returns the configured base URL for a provider: the
// <NAME>_BASE_URL override if set, else the registry default (possibly "").
func ProviderURLFromEnv(p GenericProvider) string {
	if v := strings.TrimSpace(os.Getenv(p.urlEnv())); v != "" {
		return v
	}
	return p.DefaultURL
}

// ServeGeneric proxies an OpenAI-shape request to a registry provider,
// rotating across the user's key pool with 429 failover. The base URL must be
// resolved (default or <NAME>_BASE_URL); an empty URL yields an actionable
// configuration error instead of a blind request.
func (e *EgressHandler) ServeGeneric(w http.ResponseWriter, r *http.Request, vendor string) {
	p, ok := LookupGeneric(vendor)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "unknown_provider",
			"no such provider: "+vendor)
		return
	}
	baseURL := e.ProviderURLs[vendor]
	if baseURL == "" {
		baseURL = p.DefaultURL
	}
	if baseURL == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "configuration_error",
			fmt.Sprintf("%s egress needs a base URL; set %s", vendor, p.urlEnv()))
		return
	}
	e.serveOpenAICompatible(w, r, vendor, vendor+"/", "", baseURL, p.EnvKey)
}
