package config

// Env is the process-environment contract: every variable a manager of an
// opod process (any manager — a control plane, an operator's systemd unit, a
// script) may set to steer a leader or a worker, parsed ONCE here and carried
// in Config.Env. Library code reads the struct, never os.Getenv —
// TestEnvSurfaceOutsideConfigIsTheAllowlist fails on a new read — so the
// list below is the whole surface, and Vars() is the same list as data for
// the leader's contract (controlplane.EnvContract) and for anyone writing a
// pod template. ROADMAP R12.2.
//
// Tags: `env` is the variable, `side` says who reads it (leader | worker |
// both), `doc` is one line for the table. A bool is set by "1" or "true".

import (
	"os"
	"reflect"
	"strings"
)

type Env struct {
	// leader
	PlanFile         string `env:"OPOD_PLAN_FILE" side:"leader" doc:"the mounted plan the leader serves within (default /etc/opod/plan.json; absent = standalone)"`
	AuthFile         string `env:"OPOD_AUTH_FILE" side:"leader" doc:"the mounted auth snapshot: keys + requireKeys (default /etc/opod-auth/auth.json; \"off\" = no watcher)"`
	PolicyFile       string `env:"OPOD_POLICY_FILE" side:"leader" doc:"the mounted policy snapshot: routing, logging, guardrails (default /etc/opod-auth/policy.json; \"off\" = no watcher)"`
	CoordinatorNode  string `env:"OPOD_COORDINATOR_NODE" side:"leader" doc:"pin the llama.cpp RPC coordinator to a node id (\"local\" = the leader itself)"`
	OTLPLogsEndpoint string `env:"OPOD_OTLP_LOGS_ENDPOINT" side:"leader" doc:"OTLP/HTTP collector for the leader's own log records (URL or host:port); stderr keeps working, the queue is bounded and never blocks. Empty = off"`
	// gateway (T11.1, ADR-063) — a copied FRONT of a leader: it serves /v1 and
	// nothing of /admin/v1, mirrors the leader's worker registry, pushes its
	// usage rows there and enforces from the spend snapshot it polls back.
	Role      string `env:"OPOD_ROLE" side:"leader" doc:"leader (default) | gateway. A gateway serves /v1 only: no join surface, no engine of its own, its worker list mirrored from the leader and its usage pushed there (T11.1)"`
	LeaderURL string `env:"OPOD_LEADER_URL" side:"leader" doc:"the leader a GATEWAY mirrors and pushes to (http://host:8080); required when OPOD_ROLE=gateway"`
	GatewayID string `env:"OPOD_GATEWAY_ID" side:"leader" doc:"this door's name in the leader's door count, which sets each key's 1/N rate share; unset = the hostname"`
	// worker
	Accelerator   string `env:"OPOD_ACCELERATOR" side:"worker" doc:"the vendor the manager placed the worker on (nvidia | amd | intel | tt | none); unset = what the worker detects"`
	EngineFlags   string `env:"OPOD_ENGINE_FLAGS" side:"worker" doc:"JSON map of engine flags from the plan (tp, max_model_len, ctx, ngl, …)"`
	Adapters      string `env:"OPOD_ADAPTERS" side:"worker" doc:"JSON list of LoRA adapters [{name, source, rank}] served as <model>:<name>"`
	RejectBearer  bool   `env:"OPOD_REJECT_BEARER" side:"worker" doc:"1 = the worker's API accepts HMAC only, never a bearer token"`
	SleepMode     bool   `env:"OPOD_SLEEP_MODE" side:"worker" doc:"1 = vLLM starts with sleep mode on (the sleep autoscale tier)"`
	WorkerRole    string `env:"OPOD_WORKER_ROLE" side:"worker" doc:"prefill | decode for disaggregated serving; unset = a whole worker"`
	PlanRevision  string `env:"OPOD_PLAN_REVISION" side:"worker" doc:"the plan revision this worker process was started for; the leader routes a share of traffic per revision (R15.17)"`
	AdvertiseAddr string `env:"OPOD_ADVERTISE_ADDR" side:"worker" doc:"the host:port the leader should dial (overlay / multi-NIC hosts)"`
	NodeID        string `env:"OPOD_NODE_ID" side:"worker" doc:"a stable node id across restarts (else node.yaml, else random)"`
	LeaderCA      string `env:"OPOD_LEADER_CA" side:"worker" doc:"PEM certificate the worker trusts for a TLS leader (exactly that one)"`
	NodeCert      string `env:"OPOD_NODE_CERT" side:"worker" doc:"PEM client certificate this worker presents to its leader; its SPIFFE SAN spiffe://<domain>/opod/node/<id> IS the worker's identity (R9.6). Needs OPOD_NODE_KEY"`
	NodeKey       string `env:"OPOD_NODE_KEY" side:"worker" doc:"the private key for OPOD_NODE_CERT"`
	VRAMBudgetGB  string `env:"OPOD_VRAM_BUDGET_GB" side:"worker" doc:"the slice of the card this worker may use (shared placement); read by the engine launch"`
	GPUIndex      string `env:"OPOD_GPU_INDEX" side:"worker" doc:"the device index the worker was pinned to (informational; the engine sees CUDA_VISIBLE_DEVICES & co)"`
	// T10.7: the model a worker exists to serve. The worker loads it itself,
	// once its engine answers (agent.SelfLoad) — it used to be the container
	// entrypoint POSTing the worker's own API with a bearer token, the one
	// caller that could not sign HMAC.
	LoadModel string `env:"OPOD_LOAD_MODEL" side:"worker" doc:"the catalog id this worker loads once its engine answers; repo and file come from the catalog"`
	LoadRepo  string `env:"OPOD_LOAD_REPO" side:"worker" doc:"override the catalog's repo for that load"`
	LoadFile  string `env:"OPOD_LOAD_FILE" side:"worker" doc:"override the catalog's file (the GGUF within the repo) for that load"`
	// both
	CatalogDir      string `env:"OPOD_CATALOG_DIR" side:"both" doc:"a catalog directory that overrides the bundled entries (beaten only by ~/.opod/catalog)"`
	CatalogPubKey   string `env:"OPOD_CATALOG_PUBKEY" side:"both" doc:"a minisign public key (the base64 line, or a file holding one): a catalog file in a directory that has a <file>.minisig beside it must verify against it, and a signature that does not is always a refusal. The embedded catalog is never checked. Empty = signatures are ignored"`
	CatalogMustSign bool   `env:"OPOD_CATALOG_REQUIRE_SIGNED" side:"both" doc:"1 = a catalog file in a directory with NO signature is refused too (needs OPOD_CATALOG_PUBKEY); default: unsigned files load"`
	SkipSourceCheck bool   `env:"OPOD_SKIP_SOURCE_CHECK" side:"both" doc:"1 = never HEAD-check a model's upstream (air-gapped mirrors)"`
	HFToken         string `env:"HF_TOKEN" side:"both" doc:"Hugging Face token for gated repositories"`
	HFEndpoint      string `env:"HF_ENDPOINT" side:"both" doc:"Hugging Face Hub base URL (a mirror); default https://huggingface.co"`
	ModelRevision   string `env:"OPOD_MODEL_REVISION" side:"both" doc:"pin the model's Hub revision (commit sha, tag or branch); cached under <repo>@<rev> so two revisions coexist. Empty = main, which moves"`
	ModelSHA256     string `env:"OPOD_MODEL_SHA256" side:"both" doc:"the expected sha256 of the model file; a mismatch removes the file and fails the load"`
}

// Var is one row of the environment contract.
type Var struct {
	Name string // the variable
	Side string // leader | worker | both
	Doc  string
}

// FromEnv parses the contract from the process environment.
func FromEnv() Env {
	var e Env
	v := reflect.ValueOf(&e).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		name := t.Field(i).Tag.Get("env")
		raw := strings.TrimSpace(os.Getenv(name))
		switch f := v.Field(i); f.Kind() {
		case reflect.String:
			f.SetString(raw)
		case reflect.Bool:
			f.SetBool(raw == "1" || strings.EqualFold(raw, "true"))
		}
	}
	return e
}

// Vars is the contract as a table, in the struct's order.
func Vars() []Var {
	t := reflect.TypeOf(Env{})
	out := make([]Var, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		out = append(out, Var{Name: f.Tag.Get("env"), Side: f.Tag.Get("side"), Doc: f.Tag.Get("doc")})
	}
	return out
}
