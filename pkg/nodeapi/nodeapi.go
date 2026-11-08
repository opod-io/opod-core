// Package nodeapi is the node protocol: the JSON a worker and its leader
// exchange. It is the second half of the opod wire, beside adminapi (what a
// leader serves to whoever manages it).
//
// Two directions, two listeners:
//
//   - worker → leader, on the leader's listener: the worker registers once
//     (PathRegister), then sends a heartbeat every few seconds
//     (PathHeartbeat) carrying the models its engine holds, whether the engine
//     sleeps, and an engine load sample. Placement is what heartbeats report,
//     never what a request claimed.
//   - leader → worker, on the worker's own listener: make a model resident
//     (PathModelLoad) and let it go again (PathModelUnload), put the engine to
//     sleep and wake it (PathModelSleep,
//     PathModelResume), attach and detach LoRA adapters (PathAdapters*), and
//     run helper processes — the llama.cpp RPC parts of a sharded model
//     (PathProcess*).
//
// Authentication. Every call carries "X-Opod-Auth: v=1,id=<node>,ts=<unix>,
// sig=<hex>", an HMAC-SHA256 keyed by the node's join token over the string
// "v1\n<METHOD>\n<path without query>\n<ts>". The body is NOT signed — model
// uploads stream gigabytes and are never buffered to be hashed. So the
// signature does not depend on how a body is serialised: key order, and
// whether a struct or a map produced it, change nothing a receiver verifies.
// What a receiver does depend on is the key SET, omitted-versus-present, and
// value types — which is what this package freezes and its golden files hold.
//
// Worker identity, the second form (R9.6, ADR-005's P1). A worker may instead
// be identified by a CLIENT CERTIFICATE on the TLS handshake of its calls to
// the leader, which authenticates the PROCESS rather than proving it holds a
// shared secret. The leader reads the node id out of the certificate's SPIFFE
// URI SAN — "spiffe://<trust domain>/opod/node/<id>", the one SAN shape a
// leader will read an identity from — and a certificate that names a different
// node from the body's "id" is refused rather than believed. Nothing moves in
// this package's types: the identity is in the handshake, and the mode
// (off | allow | require), the CA and the revoked serials travel in the
// manager's auth snapshot (adminapi.AuthSnapshot.NodeMTLS, NodeCertCA,
// RevokedCerts) because they have to be changeable on a serving leader.
//
// HMAC stays the GA path and the only path for the leader→worker direction,
// and a leader in "allow" accepts both, so a fleet can move one endpoint at a
// time.
//
// Errors. A worker answers an error with the status code and a plain-text
// body — not JSON — with three exceptions that are typed here:
// StartProcessError (502), the SleepResponse of an engine with no sleep mode
// (501) and the UnloadModelResponse of an engine that cannot unload (501). The
// leader answers its own errors in the {"error": {"message", "type"}} shape of
// its /admin/v1 surface.
//
// Rules: json tags are the wire and match, key for key, what the opod binary
// wrote before these types existed; additive only; a receiver ignores keys it
// does not know (held by a test), so either side may be the newer one. This
// package imports nothing but the standard library.
package nodeapi

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// Paths on the leader's listener (worker → leader).
const (
	PathRegister  = "/admin/v1/nodes/register"
	PathHeartbeat = "/admin/v1/nodes/heartbeat"
)

// Paths on the worker's listener (leader → worker).
const (
	PathModelLoad      = "/v1/model/load"
	PathModelUnload    = "/v1/model/unload" // POST UnloadModelRequest → UnloadModelResponse (feature "worker_unload")
	PathModelSleep     = "/v1/model/sleep"  // POST, no body → SleepResponse
	PathModelResume    = "/v1/model/resume" // POST, no body → SleepResponse
	PathTokenize       = "/v1/tokenize"     // POST TokenizeRequest → TokenizeResponse | 501 (feature "kv_block_events")
	PathLoad           = "/v1/load"         // GET ?maxAgeMs=N → EngineLoad | 501 (feature "load_pull", ADR-109)
	PathAdapters       = "/v1/adapters"     // GET → []HeldAdapter
	PathAdaptersLoad   = "/v1/adapters/load"
	PathAdaptersUnload = "/v1/adapters/unload"
	PathProcessStart   = "/v1/process/start"  // POST ProcessSpec → 202 ProcessInfo | 502 StartProcessError
	PathProcessStop    = "/v1/process/stop"   // POST StopProcessRequest → 204
	PathProcessList    = "/v1/process/list"   // GET → []ProcessInfo
	PathProcessGet     = "/v1/process/get"    // GET ?id= → ProcessInfo
	PathProcessLogs    = "/v1/process/logs"   // GET ?id=&lines= → text/plain
	PathProcessFile    = "/v1/process/file"   // HEAD ?name=&sha256= → 200 + HeaderFilePath | 404
	PathProcessUpload  = "/v1/process/upload" // POST ?name=&sha256=, raw body → UploadResponse
)

// HeaderFilePath is the response header of PathProcessFile: the absolute
// path of the file on the worker.
const HeaderFilePath = "X-File-Path"

// ---- worker → leader ----

// RegisterRequest is the body of PathRegister. The worker sends every key,
// empty or not.
type RegisterRequest struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	RAMGB    int    `json:"ram_gb"`
	// Address is the host:port of the worker's own listener, as the leader
	// should dial it.
	Address string `json:"address"`
	// HardwareJSON is a Capabilities document encoded as a JSON STRING — JSON
	// inside JSON. The leader stores the string as it arrived and decodes it
	// where it needs a field. EncodeHardware and Hardware do both halves.
	HardwareJSON string `json:"hardware_json"`
	// BootID names the worker PROCESS: minted once per start, sent on register
	// and on every heartbeat, so a leader tells a restarted process from the
	// one it recorded work on (the node id is stable across both). Empty = a
	// worker that predates the field.
	BootID string `json:"boot_id"`
}

// Hardware decodes HardwareJSON. An empty string is the zero Capabilities.
func (r RegisterRequest) Hardware() (Capabilities, error) {
	return DecodeHardware(r.HardwareJSON)
}

// EncodeHardware is the value of RegisterRequest.HardwareJSON for c.
func EncodeHardware(c Capabilities) (string, error) {
	b, err := json.Marshal(c)
	return string(b), err
}

// DecodeHardware reads a hardware_json string (a register body's, or a
// stored node row's). An empty string is the zero Capabilities.
func DecodeHardware(s string) (Capabilities, error) {
	var c Capabilities
	if s == "" {
		return c, nil
	}
	err := json.Unmarshal([]byte(s), &c)
	return c, err
}

// RegisterResponse answers PathRegister.
type RegisterResponse struct {
	Status string `json:"status"` // StatusRegistered
	ID     string `json:"id"`
}

// Capabilities is what a worker knows about its own host and about the
// process it is. The keys are capitalised on the wire: the document predates
// json tags, and node rows that hold it outlive any one binary.
type Capabilities struct {
	Hostname string `json:"Hostname"`
	OS       string `json:"OS"`
	Arch     string `json:"Arch"`
	CPUCores int    `json:"CPUCores"`
	RAMGB    int    `json:"RAMGB"`
	// GPUs is what the worker could detect itself; null when it found none
	// (which includes every card whose tooling the binary cannot probe).
	GPUs []GPU `json:"GPUs"`
	// Role (feature "pd_roles"): "" = a complete server; RolePrefill or
	// RoleDecode = one half of a disaggregated pair.
	Role string `json:"Role,omitempty"`
	// PlanRevision (feature "routing_weights") is the plan revision this
	// worker process was started for; the leader groups workers by it to split
	// traffic between revisions. Zero = not stated.
	PlanRevision int `json:"PlanRevision,omitempty"`
	// Engine (feature "worker_engine") is the canonical id of the engine
	// driver the worker serves through — an id of adminapi's engine list
	// ("vllm", "sglang", "llamacpp", "ollama", "mlx"). It tells the leader what
	// a model load does on that worker: an engine that serves one model per
	// process stops the model it serves to start another. Empty = a worker
	// that predates the field.
	Engine string `json:"Engine,omitempty"`
	// KVTier (feature "kv_peer_hits") is the tier below VRAM the worker's
	// engine keeps its KV cache in: "" = VRAM only, KVTierCPU = host memory
	// of its own, KVTierPeer = host memory its siblings can read from too.
	// The leader scores a prefix a sibling holds as a PEER hit for a worker
	// that states KVTierPeer — weaker than a block of its own, stronger than
	// none. Empty = a worker that predates the field, or one with no tier.
	KVTier string `json:"KVTier,omitempty"`
	// GivenDevices (feature "worker_devices") is the accelerators THIS worker
	// process was given, read from inside its own sandbox — the device nodes
	// it can open and the devices its vendor runtime enumerates — never the
	// host's full list. Under a device plugin that is the subset the plugin
	// handed the container, which is the one fact a manager cannot otherwise
	// learn: it asked for N devices and was never told which.
	//
	// nil = no statement (a worker that predates the field). A pointer to an
	// empty list = the worker looked and was given none (a CPU-only part).
	// The two are different words, which is why this is a pointer.
	GivenDevices *[]GivenDevice `json:"GivenDevices,omitempty"`
	// BindHost (OPOD_BIND_ADDR) is the host this worker's gang processes bind
	// when the address it advertises is not one its host owns — behind NAT or
	// a port forward. The leader binds them there and keeps dialling the
	// advertised address. Empty = bind the advertised host.
	BindHost string `json:"BindHost,omitempty"`
}

// GPU is one detected device.
type GPU struct {
	Name   string `json:"Name"`
	VRAMGB int    `json:"VRAMGB"`
}

// GivenDevice is one accelerator a worker process was given.
//
// Index is the device's position in the worker's OWN enumeration — inside a
// container that is usually 0..N-1 whichever physical cards they are — so it
// names nothing on the host. A reader matches on PCI, or UUID where the
// vendor publishes one; Index is only for a human reading the row.
type GivenDevice struct {
	Index int `json:"Index"`
	// Vendor is one of VendorNVIDIA, VendorAMD, VendorIntel, VendorTT — the
	// vocabulary of adminapi.EnvAccelerator.
	Vendor string `json:"Vendor"`
	// PCI is the device's PCI address in the canonical sysfs form
	// "0000:03:00.0" (NormalizePCI). "" = the worker could not read it.
	PCI string `json:"PCI,omitempty"`
	// UUID is the vendor's stable device id where it has one (NVIDIA's
	// "GPU-…"). "" = the vendor publishes none.
	UUID string `json:"UUID,omitempty"`
}

// Accelerator vendors (GivenDevice.Vendor).
const (
	VendorNVIDIA = "nvidia"
	VendorAMD    = "amd"
	VendorIntel  = "intel"
	VendorTT     = "tt"
)

// NormalizePCI returns a PCI address in the canonical sysfs form: a
// four-digit domain, lower case ("0000:03:00.0"). nvidia-smi prints an
// eight-digit domain in upper case ("00000000:03:00.0"); sysfs prints the
// canonical form already. A value that is not a PCI address comes back
// trimmed and lower-cased, never invented.
func NormalizePCI(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	dom, rest, ok := strings.Cut(s, ":")
	if !ok || strings.Count(rest, ":") != 1 {
		return s // bus:dev.fn with no domain, or not an address at all
	}
	if len(dom) > 4 {
		if strings.TrimLeft(dom[:len(dom)-4], "0") != "" {
			return s // a domain wider than four digits: keep it as written
		}
		dom = dom[len(dom)-4:]
	}
	for len(dom) < 4 {
		dom = "0" + dom
	}
	return dom + ":" + rest
}

// Worker roles (Capabilities.Role).
const (
	RolePrefill = "prefill"
	RoleDecode  = "decode"
)

// KV cache tiers (Capabilities.KVTier).
const (
	KVTierCPU  = "cpu"
	KVTierPeer = "peer"
)

// Heartbeat is the body of PathHeartbeat.
type Heartbeat struct {
	ID string `json:"id"`
	// LoadedModels is what the worker answers a request for, as the ids it was
	// asked to load (an adapter as "<base>:<name>"): what the engine holds in
	// memory, and — for an engine that keeps installed models and loads one on
	// its first request (Ollama) — what it has installed. The leader routes by
	// it. Always sent, and null and [] are different words: null = the engine
	// did not answer the worker in time, so this heartbeat carries NO REPORT —
	// a leader (feature "heartbeat_no_report") advances the node's liveness
	// and changes nothing it recorded about what the node serves; [] = the
	// engine answered and holds nothing. That is why the key is not omitempty,
	// and why a writer must never turn a failed engine query into [].
	LoadedModels []string `json:"loaded_models"`
	BootID       string   `json:"boot_id"`
	// ResidentModels (feature "resident_models") is which of LoadedModels are
	// in the engine's memory right now. Only an engine that can tell the two
	// apart sends it, and then always — a pointer to an empty list means
	// "nothing is in memory". nil = not stated: everything loaded is resident.
	ResidentModels *[]string `json:"resident_models,omitempty"`
	// Sleeping is sent only when true (feature "worker_sleep"): the engine
	// dropped its working set, its models are resident but not routable.
	Sleeping bool `json:"sleeping,omitempty"`
	// Load is sent only when the engine reported a sample (feature
	// "load_signals").
	Load *EngineLoad `json:"load,omitempty"`
	// Slots (feature "slot_admission", ADR-091) is how many requests the
	// worker's engine serves at once: llama.cpp's total_slots, SGLang's
	// effective max running requests, vLLM's --max-num-seqs. The leader
	// dispatches to the worker only while its requests in flight are below
	// it and holds the rest, ranked by class. 0 = the worker cannot say; the
	// leader then treats it as unbounded (UNGOVERNED) and says so.
	Slots int `json:"slots,omitempty"`
	// Engine (feature "engine_liveness") is what the worker's engine process is
	// doing, as the worker sees it. A pod can be Running while the engine
	// inside it crash-loops, restarts on a bad flag, or never finishes loading
	// — and to everything counting workers that pod is capacity. It is not:
	// the leader routes nothing to it, and an autoscaler that counts it scales
	// out too late or not at all.
	//
	// Sent on every heartbeat by a worker that has the feature. nil = an older
	// worker that cannot say, which a reader must treat as "no statement",
	// never as healthy.
	Engine *EngineState `json:"engine,omitempty"`
	// KVBlocks (feature "kv_block_events") is what changed in the engine's
	// prefix cache since the previous heartbeat, as block hashes. Sent only by
	// a worker whose engine publishes cache events; nil = no statement, and
	// the leader keeps routing that worker by the sticky prefix pin.
	KVBlocks *KVBlocks `json:"kv_blocks,omitempty"`
}

// KVBlocks is one heartbeat's worth of prefix-cache changes on one worker.
//
// A block is BlockSize consecutive tokens of a prompt; its hash chains from
// the block before it (BlockHash), so a hash names a whole prefix, not a
// fragment. The hashes are computed by the worker with BlockHash — never the
// engine's own, which are internal to it and have changed between versions —
// and nothing else about the prompt leaves the worker: no token ids, no text.
//
// Seq counts the worker's batches from 1 and never repeats inside one boot.
// A leader that sees a gap, and a worker that lost events from its engine,
// resolve it the same way: the worker sends Cleared, the leader forgets what
// it held for that worker, and the index is rebuilt from what follows. An
// index that is briefly empty routes as the sticky pin does; one that is
// wrong would route worse than no index at all.
type KVBlocks struct {
	Seq       int64    `json:"seq"`
	BlockSize int      `json:"block_size"`
	Stored    []string `json:"stored,omitempty"`  // BlockHash values now in the cache
	Removed   []string `json:"removed,omitempty"` // BlockHash values evicted
	Cleared   bool     `json:"cleared,omitempty"` // forget everything held for this worker, then apply Stored
	// CPUStored and CPURemoved are the same, for the host-memory tier below
	// the card (vLLM's CPU offloading, feature "kv_cpu_tier_hits"): a block
	// the card evicted may still be here, one copy away from a hit. Cleared
	// forgets this tier too. Empty = a worker with no such tier, or one whose
	// engine does not describe it.
	CPUStored  []string `json:"cpu_stored,omitempty"`
	CPURemoved []string `json:"cpu_removed,omitempty"`
}

// TokenizeRequest is the body of PathTokenize: the chat request whose prompt
// the leader wants as token ids, exactly as the engine would tokenize it
// (chat template applied). The leader has no tokenizer; the engine does.
type TokenizeRequest struct {
	Model    string            `json:"model"`
	Messages []TokenizeMessage `json:"messages,omitempty"`
	Prompt   string            `json:"prompt,omitempty"` // completions-style, when there are no messages
	// MaxTokens bounds the answer: only the leading tokens decide a prefix
	// match. 0 = the worker's default.
	MaxTokens int `json:"max_tokens,omitempty"`
}

// TokenizeMessage is one chat turn, role and text.
type TokenizeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// TokenizeResponse answers PathTokenize. 501 instead means the engine cannot
// tokenize for a caller, and the leader falls back to the sticky pin.
type TokenizeResponse struct {
	Tokens []int `json:"tokens"`
}

// blockHashSeed is the parent of a prompt's first block.
const blockHashSeed = "opod-kv-block-v1"

// BlockHash is the hash of one block of tokens given the hash of the block
// before it ("" for the first). 16 hex characters of SHA-256 over the parent
// and the token ids in fixed-width big-endian form: both sides of the node
// protocol call this one function, which is the whole reason it is here.
func BlockHash(parent string, tokens []int) string {
	h := sha256.New()
	if parent == "" {
		parent = blockHashSeed
	}
	h.Write([]byte(parent))
	var b [8]byte
	for _, t := range tokens {
		binary.BigEndian.PutUint64(b[:], uint64(int64(t)))
		h.Write(b[:])
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// BlockHashes is the chain of BlockHash values for the FULL blocks of tokens:
// a trailing partial block is not a block an engine caches. blockSize ≤ 0
// yields nothing.
func BlockHashes(tokens []int, blockSize int) []string {
	if blockSize <= 0 {
		return nil
	}
	out := make([]string, 0, len(tokens)/blockSize)
	parent := ""
	for i := 0; i+blockSize <= len(tokens); i += blockSize {
		parent = BlockHash(parent, tokens[i:i+blockSize])
		out = append(out, parent)
	}
	return out
}

// Engine states (EngineState.State).
const (
	// EngineServing: the process is up and answering the worker's probes.
	EngineServing = "serving"
	// EngineStarting: launched, not answering yet — loading weights, building
	// CUDA graphs, warming up. Normal for minutes on a large model.
	EngineStarting = "starting"
	// EngineCrashLooping: the process has exited and been restarted more than
	// once in this worker's lifetime. The pod stays Running; the engine does
	// not stay up.
	EngineCrashLooping = "crash-looping"
	// EngineRestarting: the process exited and the worker will relaunch it
	// after a back-off (seconds, doubling on each consecutive exit). Nothing
	// answers in the meantime; unlike EngineStopped, it is coming back with no
	// change from anyone. A worker older than this value says EngineStopped
	// here.
	EngineRestarting = "restarting"
	// EngineStopped: exited and not restarting — a fatal flag, a missing file,
	// an unsupported model. It will not come back without a change.
	EngineStopped = "stopped"
)

// EngineState is what the worker can say about the process it launched. The
// worker is the only party that can: the leader sees a heartbeat, Kubernetes
// sees a container that is Running, and neither sees an engine that came up,
// failed its first request and was restarted four times.
type EngineState struct {
	// State is one of the Engine* constants above. An unknown value from a
	// newer worker must be read as "no statement", not as a failure.
	State string `json:"state"`
	// Restarts is how many times the worker has relaunched the engine since
	// the worker itself started. 0 on a healthy one.
	Restarts int `json:"restarts,omitempty"`
	// Since is when the engine entered this state (RFC 3339). A "starting"
	// that has not moved in twenty minutes is a different problem from one
	// that started twenty seconds ago.
	Since string `json:"since,omitempty"`
	// Detail is the worker's own last word on why — an exit status, the first
	// line of the engine's error. Free text, for a human and for an alert.
	Detail string `json:"detail,omitempty"`
	// Version is the engine's own word on what it is (PLAN T29.5), read from
	// the running process once: vLLM's GET /version, SGLang's /server_info
	// `version`, llama-server's /props `build_info` (e.g. "b11028-972d2313b").
	// "" = not read yet, or an engine that does not say.
	Version string `json:"version,omitempty"`
}

// HeartbeatResponse answers PathHeartbeat. 404 instead means "unknown node —
// register first"; 401/403 mean the token was refused.
type HeartbeatResponse struct {
	Status string `json:"status"` // StatusOK
}

// EngineLoad is one engine's load sample. The leader aggregates the samples
// of a model's live workers into adminapi.Load; time-to-first-token is
// measured by the leader itself and is not part of a sample.
type EngineLoad struct {
	KVUsedPct    float64 `json:"kv_used_pct"`    // KV cache in use, 0–100
	QueueDepth   int64   `json:"queue_depth"`    // requests waiting for a slot
	TokensPerSec float64 `json:"tokens_per_s"`   // generated tokens per second since the previous sample
	PrefixHitPct float64 `json:"prefix_hit_pct"` // prefix-cache hit rate, 0–100 (0 when the engine does not report it, or has had no query yet)
	SampledAt    int64   `json:"sampled_at"`     // unix seconds
	// SampledAtMs is when the engine was scraped, in unix milliseconds, on a
	// sample served by PathLoad (ADR-109): a router ranks on samples a few
	// hundred ms old, which whole seconds cannot date. 0 on a heartbeat's.
	SampledAtMs int64 `json:"sampled_at_ms,omitempty"`
	// What the engine itself says it allocated and did (PLAN T23.2, feature
	// "kv_readback"), so a manager can set the KV pool it PLANNED beside the
	// one the engine really built. Each is read from the engine's own report —
	// never derived from the launch line — and nil means the engine does not
	// say: an absent key is never a zero.
	//
	// KVCapacityTokens is the KV pool in tokens (vLLM: num_gpu_blocks ×
	// block_size; SGLang: max_total_num_tokens; llama.cpp: the slots' context
	// summed when each slot has its own partition, the one context when they
	// share a unified pool). KVUsedTokens is the part of it in use, from an
	// engine that counts it (SGLang). PreemptionsTotal counts requests the
	// engine evicted from a running batch to make room — vLLM's preemptions,
	// SGLang's retractions — and EvictionsTotal counts cached KV blocks it
	// dropped; both are counters since the engine started.
	KVCapacityTokens *int64 `json:"kv_capacity_tokens,omitempty"`
	KVUsedTokens     *int64 `json:"kv_used_tokens,omitempty"`
	PreemptionsTotal *int64 `json:"preemptions_total,omitempty"`
	EvictionsTotal   *int64 `json:"evictions_total,omitempty"`
	// The engine's prefix-cache counters (PLAN T28.7), since the engine
	// started: PrefixHitTokensTotal is prompt tokens found in the GPU prefix
	// cache, PrefixQueryTokensTotal the tokens looked up (vLLM:
	// prefix_cache_hits / _queries). The leader divides their sums over the
	// workers, so an endpoint's hit rate is weighted by traffic and a worker
	// that served nothing adds nothing. nil = the engine does not say.
	PrefixHitTokensTotal   *int64 `json:"prefix_hit_tokens_total,omitempty"`
	PrefixQueryTokensTotal *int64 `json:"prefix_query_tokens_total,omitempty"`
	// The engine's host-memory KV tier, when it keeps one (PLAN T26.4), as the
	// engine reports it; nil = the engine does not say, never zero. Counters
	// are since the engine started. TierHitTokensTotal is prompt tokens served
	// from the tier and TierQueryTokensTotal the tokens looked up in it (vLLM:
	// external_prefix_cache_hits / _queries), so their ratio over a window is
	// the tier's hit rate. TierLoadBytesTotal and TierStoreBytesTotal are the
	// bytes moved from and into the tier, TierUsedPct its fill (0–100), and
	// TierAllocFailuresTotal the stores it refused for want of room.
	TierHitTokensTotal     *int64   `json:"tier_hit_tokens_total,omitempty"`
	TierQueryTokensTotal   *int64   `json:"tier_query_tokens_total,omitempty"`
	TierLoadBytesTotal     *int64   `json:"tier_load_bytes_total,omitempty"`
	TierStoreBytesTotal    *int64   `json:"tier_store_bytes_total,omitempty"`
	TierUsedPct            *float64 `json:"tier_used_pct,omitempty"`
	TierAllocFailuresTotal *int64   `json:"tier_alloc_failures_total,omitempty"`
	// EngineTTFTP50Ms is the engine's OWN median time to first token over
	// about the last minute, in milliseconds (PLAN T24.25, feature
	// "engine_ttft"), read from the engine's histogram (vLLM and SGLang:
	// time_to_first_token_seconds) — what the engine spent, without the
	// leader's routing and the network in front of it. nil when the engine
	// publishes no such histogram (llama.cpp) or served nothing in the window:
	// never a zero for "not said".
	EngineTTFTP50Ms *int64 `json:"engine_ttft_p50_ms,omitempty"`
	// Speculative decoding (ADR-120, feature "spec_decode"), counters since the
	// engine started: SpecDraftTokensTotal is the tokens the proposer offered,
	// SpecAcceptedTokensTotal the ones the model kept (vLLM:
	// spec_decode_num_draft_tokens / _num_accepted_tokens). Their ratio over a
	// window is the acceptance rate. nil = the engine does not say (no
	// speculation, or an engine that publishes no such counter).
	SpecDraftTokensTotal    *int64 `json:"spec_draft_tokens_total,omitempty"`
	SpecAcceptedTokensTotal *int64 `json:"spec_accepted_tokens_total,omitempty"`
}

// ---- leader → worker: models ----

// LoadModelRequest is the body of PathModelLoad: the catalog SOURCE of a
// model, not a resolved name — the worker resolves the engine-native name
// against its own engine, which may differ from the leader's.
type LoadModelRequest struct {
	ID         string `json:"id"` // required: the catalog id, and the name the model is served under
	OllamaName string `json:"ollama_name"`
	Repo       string `json:"repo"`
	// File is one file inside Repo (a GGUF in a multi-file repository).
	File string `json:"file,omitempty"`
	Path string `json:"path"`
	Pin  bool   `json:"pin"`
}

// LoadModelResponse answers PathModelLoad. "ready" means the load was
// accepted and the engine launched — an engine that loads weights after it
// binds is still warming; residency is what the next heartbeats report.
type LoadModelResponse struct {
	Status string `json:"status"` // StatusReady
	Model  string `json:"model"`  // the engine-native name
}

// UnloadModelRequest is the body of PathModelUnload (feature
// "worker_unload"): LoadModelRequest's source fields without File and Pin,
// so the worker resolves the engine-native name exactly as the load did.
// Every key is sent, empty or not.
type UnloadModelRequest struct {
	ID         string `json:"id"` // required: the catalog id the model was loaded under
	OllamaName string `json:"ollama_name"`
	Repo       string `json:"repo"`
	Path       string `json:"path"`
}

// UnloadModelResponse answers PathModelUnload. The call is idempotent:
//
//   - 200 StatusUnloaded — the model left the engine: the engine unloaded it,
//     or the worker stopped the engine process it had launched for it. Model
//     is the engine-native name. The model is absent from the next heartbeat.
//   - 200 StatusNoop — it was not resident; Reason says so.
//   - 501 StatusUnsupported — the engine cannot unload a model and the worker
//     did not start the engine; Engine and Reason are set, Model is not.
//
// 409 (plain text) means something else on the worker holds the model — a
// shard part of it runs there, or adapters of that base are loaded — and
// names it; 502 (plain text) means the engine failed.
type UnloadModelResponse struct {
	Status string `json:"status"` // StatusUnloaded | StatusNoop | StatusUnsupported
	Model  string `json:"model,omitempty"`
	Engine string `json:"engine,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// SleepResponse answers PathModelSleep and PathModelResume (both take no
// body). An engine without a sleep mode answers 501 with StatusUnsupported
// and a Reason, never a pretended "sleeping"; a leader passes the worker's
// answer through to whoever asked.
type SleepResponse struct {
	Status string `json:"status"` // StatusSleeping | StatusResumed | StatusUnsupported
	Engine string `json:"engine"`
	Reason string `json:"reason,omitempty"`
}

// ---- leader → worker: LoRA adapters (feature "lora") ----

// Adapter is one LoRA adapter of a worker's start-up set: an element of the
// JSON list in the OPOD_ADAPTERS variable (adminapi.EnvAdapters), which a
// manager writes and the worker reads. It is served as "<base>:<name>".
type Adapter struct {
	Name   string `json:"name"`
	Source string `json:"source"` // a Hugging Face repository or a path on the worker
	// Rank is the adapter's own r, when the writer knows it: the engine's LoRA
	// slots are sized at start for the largest rank in the set. Zero = not
	// stated.
	Rank int `json:"rank,omitempty"`
}

// LoadAdapterRequest is the body of PathAdaptersLoad.
type LoadAdapterRequest struct {
	Base   string `json:"base"` // the resident model the adapter attaches to
	Name   string `json:"name"`
	Source string `json:"source"`
	// Rank is the adapter's own r, when the caller knows it — the same field
	// Adapter carries at start. An engine's LoRA slots are sized when it
	// starts, so a worker told the rank refuses (409) an adapter its running
	// engine cannot take, naming both numbers, instead of relaying the
	// engine's error. Zero = not stated; the key is then omitted.
	Rank int `json:"rank,omitempty"`
}

// UnloadAdapterRequest is the body of PathAdaptersUnload.
type UnloadAdapterRequest struct {
	Base string `json:"base"`
	Name string `json:"name"`
}

// AdapterResponse answers PathAdaptersLoad (StatusReady) and
// PathAdaptersUnload (StatusUnloaded).
type AdapterResponse struct {
	Status string `json:"status"`
	Model  string `json:"model"` // "<base>:<name>"
}

// HeldAdapter is one element of the list GET PathAdapters returns.
type HeldAdapter struct {
	ID     string `json:"id"` // "<base>:<name>"
	Name   string `json:"name"`
	Source string `json:"source"`
}

// ---- leader → worker: helper processes (feature "shards") ----

// ProcessSpec is the body of PathProcessStart: a process the worker
// supervises for the leader. Keys are capitalised on the wire and nothing is
// omitted; the two durations travel as integer nanoseconds.
type ProcessSpec struct {
	ID         string            `json:"ID"`      // caller-assigned, unique on the worker
	Command    string            `json:"Command"` // absolute path or PATH-resolvable binary
	Args       []string          `json:"Args"`
	Env        map[string]string `json:"Env"`        // appended to the worker's own environment
	WorkDir    string            `json:"WorkDir"`    // "" = the worker's
	HealthPort int               `json:"HealthPort"` // > 0: readiness probes this TCP port
	HealthHost string            `json:"HealthHost"` // "" = 127.0.0.1
	LogLines   int               `json:"LogLines"`   // ring-buffer capacity; 0 = the worker's default
	// HealthPath upgrades readiness from "the port is open" to "this path
	// answers 200" — a model server binds before its weights are in.
	HealthPath string `json:"HealthPath"`
	// ReadyTimeout bounds the readiness probe; 0 = the worker's default, which
	// suits a process that listens immediately and not one that loads a model.
	ReadyTimeout time.Duration `json:"ReadyTimeout"`
	// Restart re-launches the process on an abnormal exit, up to MaxRestarts
	// (0 = the worker's default) with RestartBackoff doubling between tries.
	Restart        bool          `json:"Restart"`
	MaxRestarts    int           `json:"MaxRestarts"`
	RestartBackoff time.Duration `json:"RestartBackoff"`
}

// ProcessInfo is the observable state of a supervised process: the 202 of
// PathProcessStart, the answer of PathProcessGet, an element of
// PathProcessList. A start is asynchronous — poll PathProcessGet until Status
// leaves ProcessStarting.
type ProcessInfo struct {
	ID        string    `json:"id"`
	Command   string    `json:"command"`
	Args      []string  `json:"args"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Status    string    `json:"status"` // Process* constants
	ExitErr   string    `json:"exit_err,omitempty"`
	Address   string    `json:"address,omitempty"`
	Restarts  int       `json:"restarts,omitempty"` // automatic restarts so far
}

// StartProcessError is the 502 body of PathProcessStart: the spec was
// refused before anything was spawned (no id, no command, a duplicate id). A
// process that spawns and then fails is a ProcessInfo with ProcessFailed.
type StartProcessError struct {
	Error string       `json:"error"`
	Info  *ProcessInfo `json:"info"` // null when nothing was registered
}

// StopProcessRequest is the body of PathProcessStop.
type StopProcessRequest struct {
	ID string `json:"id"`
}

// UploadResponse answers PathProcessUpload: where the file landed on the
// worker, after its digest matched.
type UploadResponse struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Status words.
const (
	StatusRegistered  = "registered"  // RegisterResponse
	StatusOK          = "ok"          // HeartbeatResponse
	StatusReady       = "ready"       // LoadModelResponse, AdapterResponse (load)
	StatusUnloaded    = "unloaded"    // AdapterResponse (unload), UnloadModelResponse
	StatusNoop        = "noop"        // UnloadModelResponse: nothing to do
	StatusSleeping    = "sleeping"    // SleepResponse
	StatusResumed     = "resumed"     // SleepResponse
	StatusUnsupported = "unsupported" // SleepResponse and UnloadModelResponse, with 501

	ProcessStarting  = "starting"
	ProcessRunning   = "running"
	ProcessStopped   = "stopped"
	ProcessFailed    = "failed"
	ProcessCrashloop = "crashloop" // Restart was on and MaxRestarts was exceeded
)
