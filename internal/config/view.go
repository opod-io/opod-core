package config

import (
	"net/url"
	"regexp"
)

// View is the effective config as it may be shown: every secret redacted.
// The leader serves it at GET /admin/v1/config, and `opod config show` prints
// the same view when no leader answers — one view, so the CLI cannot print a
// token the API hides (it printed the admin and join tokens in clear).
type View struct {
	Listen        string            `json:"listen"`
	ExternalURL   string            `json:"external_url"`
	DataDir       string            `json:"data_dir"`
	LogLevel      string            `json:"log_level"`
	Engine        map[string]string `json:"engine"`
	Router        map[string]any    `json:"router"`
	Storage       map[string]string `json:"storage"`
	Auth          map[string]any    `json:"auth"`
	Observability map[string]any    `json:"observability"`
	EditHint      string            `json:"edit_hint"`
}

// ViewOf is c with its secrets redacted. A field left out of the view is
// never shown: the admin and join tokens are not in it at all.
func ViewOf(c *Config) View {
	return View{
		Listen:      c.Listen,
		ExternalURL: c.ExternalURL,
		DataDir:     c.DataDir,
		LogLevel:    c.LogLevel,
		Engine: map[string]string{
			"preferred":         c.Engine.Preferred,
			"ollama_endpoint":   c.Engine.OllamaEndpoint,
			"vllm_endpoint":     c.Engine.VLLMEndpoint,
			"vllm_api_key":      Redact(c.Engine.VLLMAPIKey),
			"mlx_endpoint":      c.Engine.MLXEndpoint,
			"llamacpp_endpoint": c.Engine.LlamaCppEndpoint,
		},
		Router: map[string]any{
			"default_model":   c.Router.DefaultModel,
			"sticky_sessions": c.Router.StickySessions,
		},
		Storage: map[string]string{
			"type":       c.Storage.Type,
			"dsn":        RedactDSN(c.Storage.DSN),
			"models_dir": c.Storage.ModelsDir,
		},
		Auth: map[string]any{
			"require_keys": c.Auth.RequireKeys,
		},
		Observability: map[string]any{
			"otlp_endpoint":  c.Observability.OTLPEndpoint,
			"otlp_status":    otlpStatus(c.Observability.OTLPEndpoint),
			"response_cache": c.Observability.ResponseCache,
		},
		EditHint: "Edit " + DefaultPath() + " or set OPOD_* env vars, then restart opod.",
	}
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

// RedactDSN is a database DSN with its password replaced: a URL's userinfo
// password (postgres://user:pass@host/db) or a key=value `password=` field.
// A DSN with no password (a SQLite path) is returned as it is.
func RedactDSN(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		if _, ok := u.User.Password(); ok {
			u.User = url.UserPassword(u.User.Username(), "redacted")
			return u.String()
		}
		return dsn
	}
	return dsnPassword.ReplaceAllString(dsn, "${1}redacted")
}

var dsnPassword = regexp.MustCompile(`(?i)(\bpassword\s*=\s*)('[^']*'|\S+)`)

// Redact shows that a secret is set without showing it: "" stays "", a
// short one is "set (redacted)", a longer one keeps its first 6 and last 4.
func Redact(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 10 {
		return "set (redacted)"
	}
	return s[:6] + "…" + s[len(s)-4:]
}
