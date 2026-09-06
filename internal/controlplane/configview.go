package controlplane

import (
	"net/http"

	"github.com/opod-io/opod/internal/config"
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
	// Sanitized copies of the callback / guardrail rows — the structs
	// carry webhook signing secrets and Langfuse keys that must get the
	// same redaction as the engine/vendor keys below. Copies, not
	// in-place edits: s.cfg stays untouched.
	cbs := make([]config.CallbackConfig, len(s.cfg.Observability.Callbacks))
	for i, cb := range s.cfg.Observability.Callbacks {
		cb.Secret = redact(cb.Secret)
		cb.PublicKey = redact(cb.PublicKey)
		cb.SecretKey = redact(cb.SecretKey)
		cb.AccessKeyID = redact(cb.AccessKeyID)
		cb.SecretAccessKey = redact(cb.SecretAccessKey)
		cbs[i] = cb
	}
	grs := make([]config.GuardrailConfig, len(s.cfg.Observability.Guardrails))
	for i, g := range s.cfg.Observability.Guardrails {
		g.AuthKey = redact(g.AuthKey)
		grs[i] = g
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
			"whisper_endpoint":  s.cfg.Engine.WhisperEndpoint,
			"piper_endpoint":    s.cfg.Engine.PiperEndpoint,
		},
		Router: map[string]any{
			"default_model":   s.cfg.Router.DefaultModel,
			"sticky_sessions": s.cfg.Router.StickySessions,
			"fallback": map[string]any{
				"enabled":       s.cfg.Router.Fallback.Enabled,
				"anthropic_url": s.cfg.Router.Fallback.AnthropicURL,
				"openai_url":    s.cfg.Router.Fallback.OpenAIURL,
				"anthropic_key": redact(s.cfg.Router.Fallback.AnthropicKey),
				"openai_key":    redact(s.cfg.Router.Fallback.OpenAIKey),
			},
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
			"callbacks":      cbs, // names + kinds; secret / langfuse keys redacted
			"guardrails":     grs, // mode + url; auth_key redacted
			"response_cache": s.cfg.Observability.ResponseCache,
		},
		Egress: map[string]any{
			"bedrock_region":  s.cfg.Router.Fallback.BedrockRegion,
			"bedrock_status":  bedrockStatus(s.cfg.Router.Fallback.BedrockRegion),
			"vertex_project":  s.cfg.Router.Fallback.VertexProject,
			"vertex_location": s.cfg.Router.Fallback.VertexLocation,
			"vertex_status":   vertexStatus(s.cfg.Router.Fallback.VertexProject),
			// OpenAI-compatible hosted gateways. Status is the
			// presence of the API key (the URL has a sensible
			// default per vendor and is rarely overridden).
			"openrouter_status": presence(s.cfg.Router.Fallback.OpenRouterKey, "OPENROUTER_API_KEY"),
			"groq_status":       presence(s.cfg.Router.Fallback.GroqKey, "GROQ_API_KEY"),
			"together_status":   presence(s.cfg.Router.Fallback.TogetherKey, "TOGETHER_API_KEY"),
			"fireworks_status":  presence(s.cfg.Router.Fallback.FireworksKey, "FIREWORKS_API_KEY"),
			"cohere_status":     presence(s.cfg.Router.Fallback.CohereKey, "COHERE_API_KEY"),
			"mistral_status":    presence(s.cfg.Router.Fallback.MistralKey, "MISTRAL_API_KEY"),
			"perplexity_status": presence(s.cfg.Router.Fallback.PerplexityKey, "PERPLEXITY_API_KEY"),
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
