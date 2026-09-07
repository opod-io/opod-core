package controlplane

import (
	"net/http"

	"github.com/opod-io/opod/pkg/adminapi"
)

// The leader's stable admin surface, v1.
//
// An external manager (anything that provisions leaders and reads their
// state over HTTP — a fleet manager, a Kubernetes operator, a script) may
// rely on exactly the routes listed here. The list is ADDITIVE-ONLY: a
// route on it is never removed, renamed, or given a different method, and
// its response shape only gains fields. TestLeaderContract walks the real
// router and fails the build the moment one of them is missing, so a PR
// that deletes a listed route cannot go green. Everything not on the list
// is free to change.
//
// Nothing here knows who the manager is; the leader answers the same way
// to a curl on a laptop.

// ContractVersion is reported by /admin/v1/version and /admin/v1/capabilities.
const ContractVersion = adminapi.ContractVersion

// ContractRoute is one method+pattern pair of the frozen surface — the shared
// wire type (pkg/adminapi); patterns use chi placeholders exactly as registered.
type ContractRoute = adminapi.Route

// LeaderContract is the frozen list. Keep it sorted by section; append only.
var LeaderContract = []ContractRoute{
	// probes — unauthenticated
	{Method: http.MethodGet, Path: "/healthz"},
	{Method: http.MethodGet, Path: "/readyz"},
	{Method: http.MethodGet, Path: "/loadz"},
	{Method: http.MethodGet, Path: "/metrics"},
	// gateway — API key
	{Method: http.MethodGet, Path: "/v1/models"},
	{Method: http.MethodPost, Path: "/v1/chat/completions"},
	// worker protocol — node-scoped key (what `opod join` speaks)
	{Method: http.MethodPost, Path: "/admin/v1/nodes/register"},
	{Method: http.MethodPost, Path: "/admin/v1/nodes/heartbeat"},
	// manager surface — admin-scoped key
	{Method: http.MethodGet, Path: "/admin/v1/version"},
	{Method: http.MethodGet, Path: "/admin/v1/capabilities"},
	{Method: http.MethodGet, Path: "/admin/v1/nodes"},
	{Method: http.MethodGet, Path: "/admin/v1/models"},
	{Method: http.MethodPost, Path: "/admin/v1/models/{id}/load"},
	{Method: http.MethodPost, Path: "/admin/v1/healthcheck"},
	{Method: http.MethodGet, Path: "/admin/v1/events/stream"},
	{Method: http.MethodGet, Path: "/admin/v1/usage/stream"},
	{Method: http.MethodGet, Path: "/admin/v1/shards"},
	{Method: http.MethodPost, Path: "/admin/v1/shards/create"},
	{Method: http.MethodDelete, Path: "/admin/v1/shards/{model_id}"},
}

// contractFeatures names the mechanisms a manager may probe for instead of
// sniffing behaviour. Only ever add keys; a key present means the mechanism
// is shipped in this binary.
func contractFeatures() map[string]bool {
	return map[string]bool{
		"events_stream":     true, // /admin/v1/events/stream with cursor + replay
		"usage_stream":      true, // /admin/v1/usage/stream with cursor + replay
		"loadz":             true, // in-flight / rpm / plan revision for autoscaling
		"shards":            true, // llama.cpp-RPC gangs, live status per part
		"plan_file":         true, // /etc/opod/plan.json watched (managed mode)
		"auth_file":         true, // /etc/opod-auth/auth.json watched: keys + requireKeys at runtime
		"router_only_ready": true, // /readyz answers ready with no local engine
		"vram_budget":       true, // `opod join --gpu --vram-budget`
		"stream_boot":       true,
		"policy_file":       true, // /etc/opod-auth/policy.json watched: fallback target, access log, guardrail webhook rules // stream batches carry "boot": cursor ids restart when the leader restarts
	}
}

// version → {"version","contract"}. Version is stamped at link time (main.go)
// and copied onto the Server before Start; "dev" when unset.
func (s *Server) adminVersion(w http.ResponseWriter, _ *http.Request) {
	v := s.Version
	if v == "" {
		v = "dev"
	}
	writeJSON(w, http.StatusOK, adminapi.Version{Version: v, Contract: ContractVersion})
}

// capabilities → the frozen route list + feature flags. A manager compares
// this against the list it was built with: a missing route means "do not
// manage this leader", never a guess.
func (s *Server) adminCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, adminapi.Capabilities{Contract: ContractVersion, Routes: LeaderContract, Features: contractFeatures()})
}
