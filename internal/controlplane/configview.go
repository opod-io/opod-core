package controlplane

import (
	"net/http"
)

// ---- config view ----

// getConfig returns a sanitized view of the effective config (secrets
// redacted). Editing is file-based — too easy to brick a running cluster
// via a typo'd HTTP PUT.
func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	type view struct {
		Listen        string            `json:"listen"`
		ExternalURL   string            `json:"external_url"`
		DataDir       string            `json:"data_dir"`
		LogLevel      string            `json:"log_level"`
		Engine        map[string]string `json:"engine"`
		Router        map[string]any    `json:"router"`
		Storage       map[string]string `json:"storage"`
		Auth          map[string]any    `json:"auth"`
		Observability map[string]any    `json:"observability"`
		Egress        map[string]any    `json:"egress"`
		EditHint      string            `json:"edit_hint"`
	}
	v := view{
		Listen:      s.cfg.Listen,
		ExternalURL: s.cfg.ExternalURL,
		DataDir:     s.cfg.DataDir,
		LogLevel:    s.cfg.LogLevel,
		Engine: map[string]string{
			"preferred":         s.cfg.Engine.Preferred,
			"ollama_endpoint":   s.cfg.Engine.OllamaEndpoint,
			"vllm_endpoint":     s.cfg.Engine.VLLMEndpoint,
			"vllm_api_key":      redact(s.cfg.Engine.VLLMAPIKey),
			"mlx_endpoint":      s.cfg.Engine.MLXEndpoint,
			"llamacpp_endpoint": s.cfg.Engine.LlamaCppEndpoint,
		},
		Router: map[string]any{
			"default_model":   s.cfg.Router.DefaultModel,
			"sticky_sessions": s.cfg.Router.StickySessions,
		},
		Storage: map[string]string{
			"type":       s.cfg.Storage.Type,
			"dsn":        s.cfg.Storage.DSN,
			"models_dir": s.cfg.Storage.ModelsDir,
		},
		Auth: map[string]any{
			"require_keys": s.cfg.Auth.RequireKeys,
		},
		Observability: map[string]any{
			"otlp_endpoint":  s.cfg.Observability.OTLPEndpoint,
			"otlp_status":    otlpStatus(s.cfg.Observability.OTLPEndpoint),
			"response_cache": s.cfg.Observability.ResponseCache,
		},
		Egress: map[string]any{
			// OpenAI-compatible hosted gateways. Status is the
			// presence of the API key (the URL has a sensible
			// default per vendor and is rarely overridden).
		},
		EditHint: "Edit " + s.cfg.DataDir + "/config.yaml or set ANTHROPIC_API_KEY / OPENAI_API_KEY / OPOD_* env vars, then restart opod.",
	}
	writeJSON(w, http.StatusOK, v)
}

// presence returns a one-line operator-facing summary for a vendor
// passthrough — "disabled" when the key is missing, or "configured"
// when it's set. The dashboard's egress card renders this directly
// so an operator can see at-a-glance which vendors are reachable.
func presence(key, envName string) string {
	if key == "" {
		return "disabled (set " + envName + ")"
	}
	return "configured"
}

// otlpStatus returns a one-line operator-facing summary of the tracing
// state — "disabled" if no endpoint, "configured" otherwise. We don't
// probe the collector here because that would either block the request
// or require a background prober; the operator already gets feedback
// from "tracing enabled" log line at opod up.
func otlpStatus(endpoint string) string {
	if endpoint == "" {
		return "disabled (set OPOD_OTLP_ENDPOINT to a collector URL)"
	}
	return "configured → " + endpoint
}

// bedrockStatus mirrors otlpStatus shape for the Bedrock egress route.
// "configured" here means: the routing pipe is wired AND SigV4 signing
// is active for anthropic.* models. amazon.*/meta.*/mistral.* still 501.
func bedrockStatus(region string) string {
	if region == "" {
		return "disabled (set OPOD_BEDROCK_REGION to enable anthropic.* via SigV4)"
	}
	return "configured → region=" + region + ", SigV4 active for anthropic.* (other families v0.7)"
}

func vertexStatus(project string) string {
	if project == "" {
		return "disabled (set OPOD_VERTEX_PROJECT to enable ADC auth probe)"
	}
	return "configured → project=" + project + ", ADC probe active (body translation v0.7)"
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 10 {
		return "set (redacted)"
	}
	return s[:6] + "…" + s[len(s)-4:]
}
