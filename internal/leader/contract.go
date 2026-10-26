package leader

import (
	"net/http"
	"sort"

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
	// gateway — API key (ADR-084, 2026-10-06: rerank returned to core)
	{Method: http.MethodPost, Path: "/v1/rerank"},
	// gateway — API key (ADR-092 §1, 2026-10-19: Anthropic Messages, translated to the chat path)
	{Method: http.MethodPost, Path: "/v1/messages"},
	{Method: http.MethodPost, Path: "/v1/messages/count_tokens"},
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
		// The worker loads the model it was started for itself (T10.7):
		// OPOD_LOAD_MODEL / _REPO / _FILE are read by `opod join`, which waits
		// for its engine and loads in-process. Without this key the container
		// image did it with `curl -H "Authorization: Bearer …"` against the
		// worker's own API, so a manager must not set OPOD_REJECT_BEARER on a
		// binary that lacks it: the worker would refuse its own load.
		"self_load":  true,
		"probe_port": true, // OPOD_PROBE_LISTEN: /healthz, /readyz, /loadz and /metrics on a second, always-plain listener, so a scraper or an autoscaler reads a number without the listener's certificate
		// A worker may be identified by a CLIENT CERTIFICATE on the join path
		// instead of by the join token it holds (R9.6, ADR-005's P1): the
		// leader reads the node id from the certificate's SPIFFE URI SAN, a
		// certificate that names another node is refused, and a revoked serial
		// is refused too. The mode (off | allow | require), the CA and the
		// revoked list are in the auth snapshot, so all three change on a
		// serving leader. Without this key a manager must assume the join token
		// is the only worker identity, which it was.
		"mtls":            true,
		"tls_listener":    true, // OPOD_TLS_CERT/KEY: the leader's one listener speaks TLS (gateway, /admin/v1, /readyz, the join path); a worker trusts it through OPOD_LEADER_CA (R9.5/R9.6 first step) // register/heartbeat carry the worker process's boot id; a changed one drops the previous incarnation's placements and shard rows at once (R10.1)
		"pd_roles":        true, // workers register a prefill|decode role (OPOD_WORKER_ROLE, hardware_json.Role); the picker routes generation to decode workers when a pair is present; the KV handoff is TARGET (R9.7)
		"tool_calls":      true, // /v1/chat/completions carries `tools` + `tool_choice` to the engine and `tool_calls` back, streaming and aggregated, and reads them + `tool_call_id` back in on the next turn; an engine that cannot is refused 400 `unsupported_request` naming the field (T10.8)
		"response_format": true, // /v1/chat/completions carries `response_format` (json_object | json_schema) to the engine — verbatim for OpenAI-shaped servers, translated for Ollama's `format` (T10.14)
		// /v1/chat/completions carries every top-level body key it does not
		// model (`seed`, `logprobs`, `top_k`, `user`, …) to an OpenAI-shaped
		// engine verbatim, and the answer's `logprobs` back; `model`,
		// `messages`, `stream` and `stream_options` stay ours, and `n` or
		// `best_of` above 1 is refused 400 (T17.1).
		"openai_passthrough": true,
		// POST /v1/rerank {model, query, documents, top_n, return_documents}
		// → scores, most relevant first (ADR-084, amending ADR-022). Served by
		// vLLM, SGLang and llama.cpp (--reranking) engines; an engine without a
		// rerank route is refused 501 `rerank_not_supported`, never a 404.
		"rerank": true,
		// POST /v1/messages (streamed or not) and /v1/messages/count_tokens
		// answer Anthropic's Messages shapes on every chat endpoint, translated
		// to the chat path: system, text and base64 images, tool_use and
		// tool_result, tools and tool_choice. Extended thinking, server tools,
		// documents and image URLs are refused 400 in Anthropic's envelope
		// (ADR-092 §1, T19.3). The usage row's protocol is "anthropic".
		"anthropic_messages": true,
		// An image (`image_url` content parts, or an Anthropic base64 image
		// block) reaches the engine: the leader re-sends content parts to its
		// worker and the shared serialiser sends them on. A catalog model
		// without `vision` is refused 400 naming `image_url`, and so is an
		// engine not proven to take images — SGLang, llama.cpp, MLX — rather
		// than answering about the text alone (T16.2).
		"vision": true,
		// OPOD_VERTEX=1 serves a Vertex AI custom container: POST
		// AIP_PREDICT_ROUTE ({instances, parameters} → {predictions}, or a chat
		// body passed through) and GET AIP_HEALTH_ROUTE, keyless, recorded as
		// protocol vertex (ADR-092 §1, T19.4).
		"vertex": true,
		// Vertex AI's gRPC PredictionService on the leader's own port, over
		// HTTP/2 (h2 / h2c): ChatCompletions and StreamRawPredict stream,
		// RawPredict answers once, each carrying OpenAI JSON in an HttpBody and
		// served through the /v1 chain — keys from the call's metadata, usage
		// protocol grpc-vertex. Other methods are Unimplemented (T19.5).
		"grpc_vertex": true,
		// /metrics (both listeners) carries vllm:num_requests_waiting,
		// vllm:num_requests_running and vllm:kv_cache_usage_perc beside the
		// opod_* names, so an inference pool whose picker reads vLLM's names
		// can route to a leader unmodified (ADR-083, vllmalias.go).
		"vllm_metric_aliases": true,
		// /loadz fills ttft_p50_ms / ttft_p95_ms: nearest-rank over the streamed
		// answers of the last 60 s (at most the last 4096), probes excluded
		// (T16.3). Without it both fields are always absent, so a scaler must not
		// render a latency-budget trigger.
		"loadz_ttft": true,
		// adapters as variants of the plan's model: worker /v1/adapters/{load,unload}, OPOD_ADAPTERS, served as <model>:<adapter> (R9.2)
		"routing_load_aware": true, // pick() scores workers by in-flight + queue + kvWeight × KV use from heartbeats; saturation rule; prefix affinity — policy.json routing.{kvWeight,kvSaturationPct,prefixAffinity} (R9.4)
		"kv_block_events":    true, // heartbeat kv_blocks → the leader's prefix-cache block index; policy routing.prefixBlockWeight scores a worker by the leading blocks it holds; /loadz prefix_index
		"request_wake":       true, // a request for a model whose engine sleeps resumes it and is held through the wake (≤ 3 s), then served; 503 + Retry-After only past the budget
		"worker_sleep":       true, // POST /admin/v1/nodes/{id}/sleep|resume → worker engine sleep mode; placements read "sleeping"; /readyz mode sleeping-workers // /loadz carries kv_used_pct / queue_depth / tokens_per_s / prefix_hit_pct from the workers' heartbeats
		"policy_file":        true,
		"shard_head":         true, // POST /admin/v1/shards/create accepts head: the coordinator is the named worker rank, never the leader (R15.14, D4)
		// Several gangs of one model under one leader (build item 6): create
		// accepts gang, DELETE /shards/{model_id}/{gang_id} removes one, and the
		// router load-balances across every gang whose coordinator is ready.
		// Without it a caller must assume one gang per model — which is what
		// every create did, replacing the previous one.
		"shard_groups": true,
		// SGLang gangs (T8.10): a catalog entry with sharding.engine=sglang is
		// split by SGLang's OWN distributed launcher — one process per rank,
		// one rendezvous address, rank 0 serving the group's API — rather than
		// by llama.cpp's rpc parts or a Ray cluster. `tp` is the tensor width
		// over the WHOLE gang and `pp` its pipeline stages. Without this key a
		// manager must assume SGLang cannot be split at all, which is what the
		// manager refused with before ADR-068 dropped the rule that made
		// SGLang's native shape illegal.
		"shard_sglang": true,
		// A gang's pressure on /loadz comes from its coordinator — the process
		// that holds the KV cache and the queue — because a gang's parts are
		// rpc-servers that run no engine and send no sample. Without it a
		// sharded endpoint's kv_used_pct and queue_depth are constants, so an
		// autoscaler must not render a trigger on either of them.
		"gang_load": true,
		// Gateway replicas, leader side (T11.1, ADR-063): POST
		// /admin/v1/usage/push takes usage rows a copied front recorded,
		// deduplicated by a row id the GATEWAY mints; GET /admin/v1/spend
		// serves the live door count and the lag bound (the per-key ceilings it
		// once carried left with ADR-077). Without it a manager must keep every
		// request on one front door.
		"gateway_spend": true,
		// Per-key limits (ADR-092 §8, T19.17): the snapshot key's rpmLimit,
		// tpmLimit, quotaDailyTokens and allowedModels are enforced from memory
		// — 429 with Retry-After naming the limit, 403 model_not_allowed, and
		// /v1/models filtered. A manager must not set a limit on an endpoint
		// whose leader lacks this: an older leader ignores the fields.
		"key_limits": true,
		// A revocation is a TOMBSTONE (ADR-085): the auth snapshot's
		// revokedKeys refuses a key id even while its row is still in keys,
		// and a tombstone seen once is never undone by a later snapshot.
		"auth_tombstones": true,
		// The auth snapshot's issuedAt is tracked; /loadz auth_snapshot reports
		// its revision and age, and policy auth.maxSnapshotAgeSec > 0 refuses
		// keyed /v1 traffic past that age with 503 + a reason (ADR-085).
		"auth_snapshot_age": true,
		// A request that finds no worker slot is HELD up to policy
		// admission.holdMs for one before the 503 + Retry-After, at most
		// admission.maxHeld at once (ADR-082, ADR-091). With holdMs 0 a
		// request that finds NO worker able to serve gets the 503 at once,
		// and one that finds every slot busy waits with no leader deadline.
		"admission_hold": true,
		// With policy admission.classes, the hold is one queue per request
		// class — critical, standard, sheddable — served strictly in that
		// order and by deficit round robin per flow (the key, or its team)
		// inside a class; each class has its own holdMs, maxHeld and
		// maxShare, and a full hold sheds the lowest class first. The class
		// is the auth snapshot key's; X-Opod-Class may only lower it
		// (ADR-086). No classes = one class, standard, on the same gate.
		"admission_classes": true,
		// Admission is governed by worker SLOTS (ADR-091): the heartbeat's
		// `slots` is how many requests a worker's engine serves at once; the
		// leader dispatches to a model's workers only while its requests in
		// flight are below their total, grants exactly one held request per
		// completion, caps each class at policy maxShare of the slots
		// (default critical 1, standard 1, sheddable 0.25), and lands each
		// request on a worker with a free slot. A worker that reports no
		// slots is ungoverned (unbounded); /gatewayz admission names it and
		// opod_admission_ungoverned_workers counts it. Chat completions only.
		"slot_admission": true,
		// `opod up --role door --routes <file>`: one URL for a cell's
		// endpoints. The door reads a request's model, looks the alias up in
		// the watched routes file (adminapi.DoorRoutes) and forwards the
		// request unchanged to that endpoint; it holds no credential, so the
		// endpoint's leader verifies the key. GET /v1/models lists the aliases
		// whose endpoint accepts the caller's key (ADR-087).
		"door": true,
		// A worker may state a KV cache tier on registration (OPOD_KV_TIER,
		// hardware_json.KVTier). When two candidates both state `peer`, the
		// picker counts the leading blocks one holds as a PEER hit for the
		// other — credited below a block of its own and above none — so an idle
		// worker that can fetch a prefix outranks a busy one that holds it
		// (ADR-089). With no peer tier the prefix score is kv_block_events'.
		"kv_peer_hits": true,
		// POST /admin/v1/shards/create accepts expert: true (PLAN T18.5): the
		// gang is a mixture-of-experts model's expert-parallel shape on vLLM's
		// own launcher — one `vllm serve` per part, data-parallel attention
		// over every device (tp inside a part, no pp), the experts spread over
		// all of them, rank 0 serving the API and the others headless. The
		// router dials rank 0 only. Without this key a leader ignores the
		// field and would form a tensor/pipeline gang, so a manager must not
		// ask it for one.
		"shard_expert": true,
		// A worker states the accelerators it was GIVEN (hardware_json.
		// GivenDevices): read from inside its own sandbox, so under a device
		// plugin it is the subset handed to the container, each by PCI address
		// or UUID. The leader stores it with the rest of the capability
		// document and serves it on GET /admin/v1/nodes; it routes nothing by
		// it. A manager that asked a device plugin for N devices reads which N
		// it got here instead of inferring it. Absent = a worker that predates
		// the field; an empty list = a worker given none.
		"worker_devices": true,
		// A failed POST /admin/v1/shards/create says WHY in error.code, beside
		// the unchanged message, status and type: "gang_part_lost" — the gang
		// did not form because a part's worker went away while it was forming
		// (replaced, removed, or its process gone); the create's parts were
		// stopped, it will not finish on its own, and the message names the
		// gang, the node and the last output polled — "gang_part_failed" —
		// the gang did not form because one of its parts' processes reported
		// failed (or crashloop once its restarts ran out) while it was
		// forming; definite in the same way, and the message carries the
		// engine's own last lines (added 2026-10-12; a manager that does not
		// know a code keeps reading the failure as it did before) — and
		// "gang_create_in_flight" — refused because another create of this
		// gang (or of the whole model) is running; nothing was touched, and the
		// running create is what decides. A 409 refusal may carry a code too:
		// "sglang_pp_tied_llama" — an SGLang pipeline (pp > 1) of a
		// Llama-family model with tied word embeddings, refused before
		// anything was touched because its last stage cannot load the output
		// layer (added 2026-10-19; a definite answer, the message names the
		// remedy). A manager keys on the code, never on the English, to tell a
		// definite answer from a collision to retry.
		"shard_create_error_codes": true,
		// A heal-within-plan create that fails records shard.heal_failed on
		// /admin/v1/events/stream (subject: the model) with data {gang, parts,
		// by: "plan", revision: the plan revision the heal read, code: the
		// create's error.code ("" when its cause has none), reason, retry_in_s},
		// and the leader backs that gang's heal off (15 s → 30 s → 60 s) so a
		// manager's own create gets the guard in between. A manager reads
		// code "gang_part_lost" or "gang_part_failed" for its gang and
		// revision as the leader's definite "this gang did not form".
		"shard_heal_failed_event": true,
		// A worker says GOODBYE on its way out — a final heartbeat declaring its
		// engine `stopped` — and the leader takes it out of rotation at once
		// (T11.2, ADR-065). Without it a pod being terminated keeps heartbeating
		// through its grace period with its last good report, and every request
		// in that window is routed into a dead engine and answered 502: an
		// error a client cannot act on, where 503 + Retry-After is the truth.
		// A manager reading this key knows a park window costs refusals a
		// caller can retry, not failures.
		"worker_goodbye":        true,
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

		// OPOD_CARD_MIB: the card's size as the manager measured it. A budgeted
		// vLLM or SGLang launch divides by it and asks nvidia-smi only when it
		// is unset, so a budget applies on a host with no nvidia-smi (PLAN
		// T23.3). Without this key a manager must assume a budget is applied
		// only where nvidia-smi answers.
		"card_mib": true,
		// The engines' own word on the KV pool they built (PLAN T23.2): a
		// heartbeat's load carries kv_capacity_tokens, kv_used_tokens,
		// preemptions_total and evictions_total when the engine states them
		// (vLLM, SGLang, llama.cpp — each from its own report, absent when it
		// does not say); GET /admin/v1/nodes carries each worker's `load`, and
		// /loadz the sums. Without this key the fields are never present.
		"kv_readback": true,
		// The engines' host KV tiers summed on /loadz (PLAN T26.4): the tier
		// counters of each counted sample's load added up, tier_used_pct the
		// fullest. Without this key a manager reads tiers per worker only.
		"kv_tier_load": true,
		// OPOD_ENGINE_OVERHEAD_MIB: the engine's overhead outside its pool for
		// the card's vendor, which a budgeted launch takes off the budget before
		// dividing (PLAN T23.3). Unset, the launch uses the SDK table's figure
		// for an unmeasured vendor (adminapi.EngineOverheadMiB).
		"engine_overhead": true,
		// /loadz carries tier_ghost (PLAN T26.14): from the workers' cache
		// events, the blocks their engines computed and the evicted blocks they
		// computed again, by eviction depth — what a host KV tier would have
		// served. Without this key a manager cannot size a tier from traffic.
		"tier_ghost": true,
		// Admission queues at the smaller of the engines' slots and the
		// requests their KV pools hold at the endpoint's context — the plan's
		// workload (maxContext, else inputTokens + outputTokens) — and /loadz
		// says which bound it used in admission_bound: slots | kv (PLAN
		// T23.11). Without this key admission counts slots only, and a vLLM
		// worker queues the excess inside the engine.
		"admission_kv_bound": true,
		// A vLLM or SGLang worker's heartbeat load carries engine_ttft_p50_ms,
		// the engine's own median time to first token over about the last
		// minute (its time_to_first_token_seconds histogram), and /loadz the
		// median over the workers that state it (PLAN T24.25). llama.cpp
		// publishes none and states none. Without this key a manager cannot
		// tell the leader's share of ttft_p50_ms from the engine's.
		"engine_ttft": true,
		// The policy snapshot's routing.cacheIsolation (team | key | none,
		// default team) separates callers' prefix caches (PLAN T26.8): the
		// leader sends `cache_salt` — an HMAC of the caller's team or key id —
		// to vLLM, SGLang and TensorRT-LLM, and its prefix pins carry it too.
		// Without this key every caller of an endpoint shares its cache.
		"cache_salt": true,
		// The policy snapshot's wake.noResumeInPlace names sleeping workers the
		// request wake must not resume (PLAN T11.31): a request that finds only
		// those is held by the admission hold, not served by a resume into a
		// card the manager gave away. Without this key the leader resumes any
		// sleeper, and a manager must park a worker before it hands its card on.
		"wake_no_resume": true,
		// A vLLM or SGLang launch — a worker's own, or a gang's — adds the
		// tool-call parser the model's catalog entry names for that engine
		// (tool_parser), unless the plan's extra already names one (PLAN
		// T19.25). Without this key a manager that wants tool calls parsed puts
		// the parser in extra itself.
		"tool_parsers": true,
		// A vLLM worker's engine flag kv_tier_fs=<dir> (PLAN T26.9): with the
		// host tier on the line, the launch adds a disk tier below it in that
		// directory — one --kv-transfer-config (TieringOffloadingSpec, an fs
		// secondary tier), PYTHONHASHSEED fixed — and `opod cache kv-prune`
		// bounds the directory, which the engine never deletes from. Without
		// this key a worker drops the flag and keeps the host tier alone.
		"kv_tier_fs": true,
		// OPOD_PLAIN_LISTEN: the whole request API — the main listener's own
		// handler — on a second, always-plain listener, beside a main one that
		// may speak TLS. For a gateway that load-balances several leaders and
		// cannot originate TLS to each one's own certificate (an InferencePool's
		// members). The hop to it is clear text; the network decides who may
		// reach it. Without this key a manager must assume the leader serves its
		// API on one listener only, and a second port it points at is closed.
		"plain_listen": true,
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

// FeatureKeys is the feature list this binary advertises on
// /admin/v1/capabilities, sorted: the same map, so the two cannot disagree.
// `opod version --features` prints it, and the image build stamps it on the
// leader image as the io.opod.leader.features label, so a manager can learn
// what a leader speaks from its image before any pod of it runs.
func FeatureKeys() []string {
	f := contractFeatures()
	out := make([]string, 0, len(f))
	for k, on := range f {
		if on {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
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
