package controlplane

import (
	"net/http"

	"github.com/opod-io/opod-sdk/adminapi"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/engines"
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
// wire type (opod-sdk/adminapi); patterns use chi placeholders exactly as registered.
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
	{Method: http.MethodPost, Path: "/admin/v1/nodes/{id}/sleep"},
	{Method: http.MethodPost, Path: "/admin/v1/nodes/{id}/resume"},
	{Method: http.MethodGet, Path: "/admin/v1/models"},
	{Method: http.MethodPost, Path: "/admin/v1/models/{id}/load"},
	{Method: http.MethodPost, Path: "/admin/v1/adapters"},
	{Method: http.MethodDelete, Path: "/admin/v1/adapters/{name}"},
	{Method: http.MethodPost, Path: "/admin/v1/healthcheck"},
	{Method: http.MethodGet, Path: "/admin/v1/events/stream"},
	{Method: http.MethodGet, Path: "/admin/v1/usage/stream"},
	{Method: http.MethodGet, Path: "/admin/v1/shards"},
	{Method: http.MethodPost, Path: "/admin/v1/shards/create"},
	{Method: http.MethodDelete, Path: "/admin/v1/shards/{model_id}"},
	{Method: http.MethodPost, Path: "/admin/v1/nodes/{id}/drain"},
	{Method: http.MethodPost, Path: "/admin/v1/nodes/{id}/undrain"},
}

// EnvContract is the process-environment half of the contract: every
// variable a manager may set on a leader or worker process (config.Env), as
// a table with the side that reads it. Library code reads nothing else from
// the environment (config's envsurface test), so a manager that renders
// exactly these names, and only these, cannot drift from the binary.
var EnvContract = config.Vars()

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
		"routing_weights":   true, // policy.routing.revisions splits traffic between plan revisions; workers register OPOD_PLAN_REVISION
		"model_revision":    true, // fetch pins a Hub revision + sha256; two revisions cache side by side (OPOD_MODEL_REVISION)
		"adapters_runtime":  true, // /admin/v1/adapters: load or drop a LoRA on every worker holding the base, no restart
		"vram_budget":       true, // `opod join --gpu --vram-budget`
		"stream_boot":       true,
		"load_signals":      true,
		"lora":              true,
		"boot_id":           true,
		"tls_listener":      true, // OPOD_TLS_CERT/KEY: the leader's one listener speaks TLS (gateway, /admin/v1, /readyz, the join path); a worker trusts it through OPOD_LEADER_CA (R9.5/R9.6 first step) // register/heartbeat carry the worker process's boot id; a changed one drops the previous incarnation's placements and shard rows at once (R10.1)
		"pd_roles":          true, // workers register a prefill|decode role (OPOD_WORKER_ROLE, hardware_json.Role); the picker routes generation to decode workers when a pair is present; the KV handoff is TARGET (R9.7)
		// adapters as variants of the plan's model: worker /v1/adapters/{load,unload}, OPOD_ADAPTERS, served as <model>:<adapter> (R9.2)
		"routing_load_aware":    true, // pick() scores workers by in-flight + queue + kvWeight × KV use from heartbeats; saturation rule; prefix affinity — policy.json routing.{kvWeight,kvSaturationPct,prefixAffinity} (R9.4)
		"worker_sleep":          true, // POST /admin/v1/nodes/{id}/sleep|resume → worker engine sleep mode; placements read "sleeping"; /readyz mode sleeping-workers // /loadz carries kv_used_pct / queue_depth / tokens_per_s / prefix_hit_pct from the workers' heartbeats
		"policy_file":           true,
		"shard_head":            true, // POST /admin/v1/shards/create accepts head: the coordinator is the named worker rank, never the leader (R15.14, D4)
		"ttft":                  true, // usage rows carry ttft_ms for streamed answers (R15.13)
		"engines":               true, // /admin/v1/capabilities lists the engine drivers linked into this binary: id, accepted aliases, native naming
		"fetch_snapshot":        true, // `opod fetch --snapshot <repo>[@rev]`: a safetensors file set under <models dir>/<repo>@<rev>/, same lock, marker and digest check as a GGUF; a vLLM or SGLang worker serves from a complete one
		"otlp_logs":             true, // OPOD_OTLP_LOGS_ENDPOINT: the leader's own log records over OTLP/HTTP, teed beside stderr; bounded queue, never blocks
		"cache_prune":           true, // `opod cache ls|prune`: the node cache is reclaimable, and only files this platform fetched (ADR-046)
		"node_drain":            true, // POST /admin/v1/nodes/{id}/drain|undrain: a draining node gets no new request and no new shard part, in-flight finishes; the state survives heartbeats and a re-register; /readyz and the waking 503 do not count it
		"gang_devices_per_rank": true, // POST /admin/v1/shards/create accepts devices (GPUs per part); TP × PP is checked against parts × devices // /etc/opod-auth/policy.json watched: fallback target, access log, guardrail webhook rules // stream batches carry "boot": cursor ids restart when the leader restarts
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

// EngineInfo is one engine driver linked into this binary, as
// /admin/v1/capabilities reports it. A manager that offers engines by name
// compares its own list against this one instead of finding out at process
// launch that a name was dropped or renamed.
//
// Core-local for now: this type (and the Engines field below) moves to
// opod-sdk/adminapi — onto adminapi.Capabilities — at the next SDK tag, with
// these exact JSON names.
type EngineInfo struct {
	// ID is the canonical engine name: what `opod up --engine`, a plan and a
	// worker's registration all resolve to.
	ID string `json:"id"`
	// Aliases are the other spellings this binary accepts for the same driver.
	Aliases []string `json:"aliases,omitempty"`
	// Native is the catalog source field the engine pulls and serves a model
	// by: "ollama_name", "repo", "path", or "id" (the catalog id itself).
	Native string `json:"native"`
}

// capabilitiesResponse is adminapi.Capabilities plus the fields that have not
// reached an SDK tag yet. Embedded, so the wire shape only gains keys.
type capabilitiesResponse struct {
	adminapi.Capabilities
	Engines []EngineInfo `json:"engines"`
}

// contractEngines lists the linked drivers, sorted by id.
func contractEngines() []EngineInfo {
	infos := engines.Infos()
	out := make([]EngineInfo, 0, len(infos))
	for _, i := range infos {
		out = append(out, EngineInfo{ID: i.Name, Aliases: i.Aliases, Native: i.Native})
	}
	return out
}

// capabilities → the frozen route list + feature flags + the linked engine
// drivers. A manager compares this against the list it was built with: a
// missing route means "do not manage this leader", never a guess.
func (s *Server) adminCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, capabilitiesResponse{
		Capabilities: adminapi.Capabilities{Contract: ContractVersion, Routes: LeaderContract, Features: contractFeatures()},
		Engines:      contractEngines(),
	})
}
