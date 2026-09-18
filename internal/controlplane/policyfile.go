package controlplane

// Policy as a watched file (§13 item 2, v3 — P12-2): in managed mode the
// executor mounts the endpoint's policy snapshot beside the auth snapshot
// (/etc/opod-auth/policy.json, same Secret — webhook keys are secrets) and
// the leader applies it at runtime. The snapshot carries the three things
// a manager governs on the request path without being on it (ADR-001):
//
//   - routing.fallback: an OpenAI-compatible base URL (+ optional model and
//     bearer) the leader forwards to when it has NO serving capacity — the
//     honest 503 "waking" becomes a served answer from the fallback;
//   - logging.accessLog: whether the leader writes its per-request access
//     log line (noise control per endpoint);
//   - guardrails: webhook rules (phase pre|post|logging_only, URL, bearer,
//     fail posture, timeout) that become the guardrail chain the gateway
//     walks on every request.
//
// No file → no-op: standalone `opod up` keeps its config behaviour. A bad
// file keeps the last good policy (never a half-applied one).

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opod-io/opod-sdk/adminapi"

	"github.com/opod-io/opod/internal/guardrails"
	"github.com/opod-io/opod/internal/router"
)

const defaultPolicyPath = "/etc/opod-auth/policy.json"

// PolicySnapshot and its parts are the shared wire types (opod-sdk/adminapi).
type (
	PolicySnapshot = adminapi.PolicySnapshot
	PolicyRouting  = adminapi.PolicyRouting
	PolicyLogging  = adminapi.PolicyLogging
	GuardrailRule  = adminapi.GuardrailRule
)

type policyFileState struct {
	mu       sync.Mutex
	present  bool
	revision string
	rules    int

	fallback  atomic.Pointer[PolicyRouting]
	accessLog atomic.Value // bool; unset = config default
}

// fallbackRouting returns the active fallback target, or nil.
func (p *policyFileState) fallbackRouting() *PolicyRouting {
	r := p.fallback.Load()
	if r == nil || r.FallbackURL == "" {
		return nil
	}
	return r
}

// accessLogEnabled reports the per-endpoint access-log switch (default on).
func (p *policyFileState) accessLogEnabled() bool {
	v, ok := p.accessLog.Load().(bool)
	return !ok || v
}

// StartPolicyWatcher polls the policy file and applies it. Like the auth
// file it may appear after boot, so an absent file keeps the watcher
// running; OPOD_POLICY_FILE=off disables it.
func (s *Server) StartPolicyWatcher(ctx context.Context) {
	path := s.cfg.Env.PolicyFile
	if path == "" {
		path = defaultPolicyPath
	}
	if path == "off" {
		return
	}
	var lastMod time.Time
	load := func() {
		st, err := os.Stat(path)
		if err != nil || !st.ModTime().After(lastMod) {
			return
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var doc PolicySnapshot
		if err := json.Unmarshal(raw, &doc); err != nil {
			s.log.Warn("policy file unreadable — keeping last good policy", "path", path, "err", err)
			return
		}
		lastMod = st.ModTime()
		s.applyPolicySnapshot(&doc)
		// R15.17: the revision split is read from the raw document rather than
		// from the shared type, because the field is additive and the published
		// SDK module has not been tagged with it yet. Reading the JSON directly
		// means the mechanism works the day a manager writes the field, and the
		// line below collapses to doc.Routing.Revisions when the tag lands.
		s.applyRevisionWeights(raw)
	}
	load()
	go func() {
		t := time.NewTicker(10 * time.Second)
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

// applyPolicySnapshot swaps the guardrail registry, the fallback target and
// the access-log switch atomically (each is one pointer store); a request in
// flight finishes under the policy it started with. Idempotent.
func (s *Server) applyPolicySnapshot(doc *PolicySnapshot) {
	s.policy.mu.Lock()
	defer s.policy.mu.Unlock()
	reg, skipped := buildGuardrailRegistry(doc.Guardrails)
	s.openaiH.SetPolicy(s.openaiH.Policy().WithGuardrails(reg))
	r := doc.Routing
	r.FallbackURL = strings.TrimRight(strings.TrimSpace(r.FallbackURL), "/")
	r.FallbackURL = strings.TrimSuffix(r.FallbackURL, "/v1")
	s.policy.fallback.Store(&r)
	if s.router != nil {
		s.router.SetLoadAware(r.KVWeight, r.KVSaturationPct, r.PrefixAffinity)
	}
	if doc.Logging.AccessLog != nil {
		s.policy.accessLog.Store(*doc.Logging.AccessLog)
	} else {
		s.policy.accessLog.Store(true)
	}
	changed := !s.policy.present || s.policy.revision != doc.Revision
	s.policy.present = true
	s.policy.revision = doc.Revision
	s.policy.rules = len(doc.Guardrails) - skipped
	if changed {
		s.log.Info("policy snapshot applied", "revision", doc.Revision, "guardrails", s.policy.rules, "skipped", skipped,
			"fallback", r.FallbackURL != "", "accessLog", s.policy.accessLogEnabled())
		s.logEvent("policy.updated", doc.Revision, map[string]any{"guardrails": s.policy.rules, "skipped": skipped,
			"fallback": r.FallbackURL != "", "accessLog": s.policy.accessLogEnabled()})
	}
}

// buildGuardrailRegistry turns the rules into the three mode chains. Rules
// without a URL or with an unknown phase are skipped (counted), never
// guessed. Private targets are allowed: guardrail receivers run inside the
// cluster by design.
func buildGuardrailRegistry(rules []GuardrailRule) (*guardrails.Registry, int) {
	var pre, post, logOnly []guardrails.Guardrail
	skipped := 0
	for _, r := range rules {
		mode := guardrails.Mode(strings.ToLower(strings.TrimSpace(r.Phase)))
		if r.URL == "" || (mode != guardrails.ModePre && mode != guardrails.ModePost && mode != guardrails.ModeLoggingOnly) {
			skipped++
			continue
		}
		id := r.ID
		if id == "" {
			id = "rule-" + string(mode)
		}
		w := guardrails.NewWebhook(guardrails.WebhookConfig{
			ID: id, Mode: mode, URL: r.URL, AuthKey: r.AuthKey, Headers: r.Headers, FailOpen: r.FailOpen,
			Timeout: time.Duration(r.TimeoutMs) * time.Millisecond,
		})
		switch mode {
		case guardrails.ModePre:
			pre = append(pre, w)
		case guardrails.ModePost:
			post = append(post, w)
		default:
			logOnly = append(logOnly, w)
		}
	}
	if len(pre)+len(post)+len(logOnly) == 0 {
		return nil, skipped
	}
	return &guardrails.Registry{Pre: guardrails.NewChain(pre...), Post: guardrails.NewChain(post...), LoggingOnly: guardrails.NewChain(logOnly...)}, skipped
}

// applyRevisionWeights reads policy.routing.revisions and hands it to the
// router (R15.17, feature routing_weights). Empty or absent turns the split
// off, which is every endpoint that is not mid-canary.
//
// This parses the raw snapshot instead of the shared adminapi type on purpose:
// the field is additive and the published SDK module is not yet tagged with it.
// A manager that writes it gets the behaviour today; when the tag lands this
// becomes one line off doc.Routing.
func (s *Server) applyRevisionWeights(raw []byte) {
	if s.router == nil {
		return
	}
	var doc struct {
		Routing struct {
			Revisions []router.RevisionWeight `json:"revisions"`
		} `json:"routing"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return // the snapshot already parsed above; a shape we cannot read here just means no split
	}
	s.router.SetRevisionWeights(doc.Routing.Revisions)
}
