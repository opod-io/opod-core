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
	{Method: http.MethodDelete, Path: "/admin/v1/shards/{model_id}/{gang_id}"},
	{Method: http.MethodPost, Path: "/admin/v1/nodes/{id}/drain"},
	{Method: http.MethodPost, Path: "/admin/v1/nodes/{id}/undrain"},
	{Method: http.MethodPost, Path: "/admin/v1/models/{id}/move"},
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
		"engine_liveness":   true, // heartbeats carry what the worker's ENGINE PROCESS is doing (serving | starting | crash-looping | stopped); /loadz says how many workers are holding a card with a dead engine
		"plan_heal_gangs":   true, // the leader forms a gang its plan declares when that gang has no parts at all and free workers have registered — so a gang whose parts were restored by something outside the leader comes back without an admin call (§13 item 3)
		"probe_header":      true, // a request carrying X-Opod-Probe is served like any other but counted like none: not in in_flight, rpm_1m, unavailable_1m or the idle clock, so a prover cannot move the number an autoscaler reads
		"probe_port":        true, // OPOD_PROBE_LISTEN: /healthz, /readyz, /loadz and /metrics on a second, always-plain listener, so a scraper or an autoscaler reads a number without the listener's certificate
		"tls_listener":      true, // OPOD_TLS_CERT/KEY: the leader's one listener speaks TLS (gateway, /admin/v1, /readyz, the join path); a worker trusts it through OPOD_LEADER_CA (R9.5/R9.6 first step) // register/heartbeat carry the worker process's boot id; a changed one drops the previous incarnation's placements and shard rows at once (R10.1)
		"pd_roles":          true, // workers register a prefill|decode role (OPOD_WORKER_ROLE, hardware_json.Role); the picker routes generation to decode workers when a pair is present; the KV handoff is TARGET (R9.7)
		// adapters as variants of the plan's model: worker /v1/adapters/{load,unload}, OPOD_ADAPTERS, served as <model>:<adapter> (R9.2)
		"routing_load_aware": true, // pick() scores workers by in-flight + queue + kvWeight × KV use from heartbeats; saturation rule; prefix affinity — policy.json routing.{kvWeight,kvSaturationPct,prefixAffinity} (R9.4)
		"worker_sleep":       true, // POST /admin/v1/nodes/{id}/sleep|resume → worker engine sleep mode; placements read "sleeping"; /readyz mode sleeping-workers // /loadz carries kv_used_pct / queue_depth / tokens_per_s / prefix_hit_pct from the workers' heartbeats
		"policy_file":        true,
		"shard_head":         true, // POST /admin/v1/shards/create accepts head: the coordinator is the named worker rank, never the leader (R15.14, D4)
		// Several gangs of one model under one leader (build item 6): create
		// accepts gang, DELETE /shards/{model_id}/{gang_id} removes one, and the
		// router load-balances across every gang whose coordinator is ready.
		// Without it a caller must assume one gang per model — which is what
		// every create did, replacing the previous one.
		"shard_groups": true,
		// A gang's pressure on /loadz comes from its coordinator — the process
		// that holds the KV cache and the queue — because a gang's parts are
		// rpc-servers that run no engine and send no sample. Without it a
		// sharded endpoint's kv_used_pct and queue_depth are constants, so an
		// autoscaler must not render a trigger on either of them.
		"gang_load": true,
		// Gateway replicas, leader side (T11.1, ADR-063): POST
		// /admin/v1/usage/push takes usage rows a copied front recorded,
		// deduplicated by a row id the GATEWAY mints; GET /admin/v1/spend
		// serves the per-key spend, the ceilings, this door's 1/N share of them
		// and the lag bound a quota may drift by. Without it a manager must keep
		// every request on one front door.
		"gateway_spend":         true,
		"ttft":                  true, // usage rows carry ttft_ms for streamed answers (R15.13)
		"engines":               true, // /admin/v1/capabilities lists the engine drivers linked into this binary: id, accepted aliases, native naming
		"fetch_snapshot":        true, // `opod fetch --snapshot <repo>[@rev]`: a safetensors file set under <models dir>/<repo>@<rev>/, same lock, marker and digest check as a GGUF; a vLLM or SGLang worker serves from a complete one
		"otlp_logs":             true, // OPOD_OTLP_LOGS_ENDPOINT: the leader's own log records over OTLP/HTTP, teed beside stderr; bounded queue, never blocks
		"cache_prune":           true, // `opod cache ls|prune`: the node cache is reclaimable, and only files this platform fetched (ADR-046)
		"placement_drain":       true, // a placement the leader marks draining stays out of rotation across the worker's heartbeats, until it is set back, the model leaves the worker, or the leader restarts
		"heartbeat_no_report":   true, // a heartbeat with loaded_models null changes no placement row (marks survive a slow engine tick); an engine silent past the heartbeat bound takes the node out of rotation — state engine-silent, events node.engine_silent|engine_reporting
		"model_move":            true, // POST /admin/v1/models/{id}/move {from, to}: a whole model changes worker by overlap — load on the target, flip when it is routable, drain the source, unload it; a target that never serves leaves the source serving; events model.move_*
		"worker_engine":         true, // a worker registers the canonical id of its engine driver (hardware_json.Engine); /admin/v1/capabilities `engines` lists the ids
		"resident_models":       true, // heartbeats from an engine that loads on request carry resident_models beside loaded_models; the rest of its placements are cold — routable, holding no memory
		"worker_unload":         true, // POST /v1/model/unload on a worker (scheduler.UnloadFromNode): the engine's unload, or a stop of the engine process the worker launched; idempotent; 409 for a shard part or a held adapter, 501 when the engine cannot
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
