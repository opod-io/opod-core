package guardrails

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/opod-io/opod/internal/httpsafe"
)

// Webhook is a generic guardrail that POSTs the body to an external
// service and parses the response as `{action, replacement, reason}`.
//
// Response shape:
//
//	{"action":"allow"}
//	{"action":"block","reason":"detected PII"}
//	{"action":"rewrite","replacement": <new body as JSON>}
//	{"action":"flag","reason":"low confidence"}
//
// The webhook is the right starting point for Presidio + Bedrock
// Guardrails too — both have published HTTP APIs that map cleanly to
// this contract via a thin shim on the operator's side.
type Webhook struct {
	id       string
	mode     Mode
	url      string
	authKey  string
	headers  map[string]string
	failOpen bool
	timeout  time.Duration
	client   *http.Client
}

// WebhookConfig describes one webhook guardrail row from the YAML.
type WebhookConfig struct {
	ID                  string            // logical name
	Mode                Mode              // pre | post | logging_only
	URL                 string            // required
	AuthKey             string            // optional Bearer token
	Headers             map[string]string // optional extra headers (e.g. tenant id)
	FailOpen            bool              // true → on error, return Allow; false → return Block
	Timeout             time.Duration     // request timeout; 0 → 5s
	BlockPrivateTargets bool              // refuse to POST to private/loopback addresses
}

// NewWebhook builds a webhook driver. No goroutines started yet —
// guardrails are called synchronously in the request path.
func NewWebhook(cfg WebhookConfig) *Webhook {
	if cfg.ID == "" {
		cfg.ID = "guardrail-webhook"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	return &Webhook{
		id:       cfg.ID,
		mode:     cfg.Mode,
		url:      cfg.URL,
		authKey:  cfg.AuthKey,
		headers:  cfg.Headers,
		failOpen: cfg.FailOpen,
		timeout:  cfg.Timeout,
		client:   httpsafe.NewClient(cfg.Timeout, cfg.BlockPrivateTargets),
	}
}

func (w *Webhook) Name() string { return w.id }
func (w *Webhook) Mode() Mode   { return w.mode }

// Check forwards the body to the configured URL and translates the
// JSON response into an Action. On any error, the configured fail-open
// posture decides whether the request is allowed (true) or blocked
// (false). Caller is responsible for ensuring the body is the right
// shape — Webhook is content-agnostic.
func (w *Webhook) Check(ctx context.Context, body []byte) (Action, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return w.onError(err), err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "opod/0.2 (guardrail)")
	if w.authKey != "" {
		req.Header.Set("Authorization", "Bearer "+w.authKey)
	}
	for k, v := range w.headers {
		req.Header.Set(k, v)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return w.onError(err), err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("guardrail %s returned HTTP %d", w.id, resp.StatusCode)
		return w.onError(err), err
	}
	var parsed struct {
		Action      string          `json:"action"`
		Reason      string          `json:"reason"`
		Replacement json.RawMessage `json:"replacement"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return w.onError(err), err
	}
	switch parsed.Action {
	case "allow", "":
		return Allow(), nil
	case "block":
		return Block(parsed.Reason), nil
	case "rewrite":
		if len(parsed.Replacement) == 0 {
			// A rewrite without a replacement is a malformed verdict —
			// let the configured fail-open posture decide rather than
			// silently allowing.
			err := fmt.Errorf("guardrail %s returned rewrite with no replacement", w.id)
			return w.onError(err), err
		}
		return Rewrite([]byte(parsed.Replacement)), nil
	case "flag":
		return Flag(parsed.Reason), nil
	default:
		// Unknown verdicts go through the fail-open / fail-closed
		// contract too — fail_open=false must not silently allow.
		err := fmt.Errorf("guardrail %s returned unknown action %q", w.id, parsed.Action)
		return w.onError(err), err
	}
}

// onError implements the fail-open / fail-closed contract.
func (w *Webhook) onError(err error) Action {
	if w.failOpen {
		return Allow()
	}
	return Block("guardrail " + w.id + " unreachable: " + err.Error())
}

var _ Guardrail = (*Webhook)(nil)
