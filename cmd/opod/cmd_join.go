package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/opod-io/opod/internal/kvevents"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/mesh"

	"gopkg.in/yaml.v3"
)

// cmdJoin connects this machine to an existing Opod cluster as a worker.
//
// Usage:
//
//	opod join "http://leader:8080?token=sk-orc-..."
//
// workerBaseEnv is what every engine process this worker launches sees in
// addition to its own environment: the device it may use and the VRAM budget
// (from the flags, else from the environment contract). Set on the
// supervisor, never os.Setenv on the worker process itself — the worker's
// own code reads config.Env; only the engines read these.
func workerBaseEnv(gpuIndex, vramBudget string) map[string]string {
	env := map[string]string{}
	if gpuIndex != "" {
		env["OPOD_GPU_INDEX"] = gpuIndex
		for _, k := range []string{"CUDA_VISIBLE_DEVICES", "HIP_VISIBLE_DEVICES", "ONEAPI_DEVICE_SELECTOR"} {
			if os.Getenv(k) != "" {
				continue // an operator's own pin wins
			}
			if k == "ONEAPI_DEVICE_SELECTOR" {
				env[k] = "level_zero:" + gpuIndex
			} else {
				env[k] = gpuIndex
			}
		}
	}
	if vramBudget != "" {
		env["OPOD_VRAM_BUDGET_GB"] = vramBudget
	}
	return env
}

func cmdJoin(args []string) {
	if wantsHelp(args) {
		showHelp(helpSpec{
			name:    "join",
			summary: "join this machine to an existing opod cluster as a worker",
			usage:   "opod join \"<leader-url>?token=<token>\" [--gpu <index>] [--vram-budget <GB>]",
			examples: []string{
				"opod join \"http://leader.local:8080?token=sk-orc-...\"",
				"opod join \"https://opod.example.com?token=sk-orc-...\"",
				"opod join \"http://leader:8080?token=sk-orc-...\" --gpu 1 --vram-budget 20",
			},
			notes: []string{
				"Generate the token on the leader: `opod token create --node`",
				"--gpu <i> pins this worker's engine to one device (CUDA_VISIBLE_DEVICES / HIP_VISIBLE_DEVICES).",
				"--vram-budget <GB> caps the engine's VRAM (vLLM --gpu-memory-utilization = budget / device) so",
				"  several workers can share a device under a capacity ledger. Env: OPOD_VRAM_BUDGET_GB.",
				"⚠️  Tokens grant access. Only join on a trusted network (LAN or Tailscale).",
			},
		})
	}
	if len(args) == 0 {
		die("usage: opod join \"<leader-url>?token=<token>\"  (run `opod join --help` for details)")
	}
	leader, token, err := parseJoinTarget(args[0])
	if err != nil {
		die("%v", err)
	}
	certJoin := token == "" // decided below, once the environment is read
	// Placement hints for the engines this worker will supervise. They reach
	// every engine launcher (vLLM, llama.cpp, …) through the supervisor's base
	// environment — one contract; a manager sets the same variables on the
	// pod instead, and the flags win over them.
	var gpuIndex, vramBudget string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--gpu":
			if i+1 >= len(args) {
				die("--gpu needs a device index")
			}
			i++
			gpuIndex = args[i]
		case "--vram-budget":
			if i+1 >= len(args) {
				die("--vram-budget needs a size in GB")
			}
			i++
			if _, err := strconv.Atoi(args[i]); err != nil {
				die("--vram-budget must be an integer number of GB, got %q", args[i])
			}
			vramBudget = args[i]
		default:
			die("unknown join argument %q (see `opod join --help`)", args[i])
		}
	}

	cfg := loadConfigOrExit()
	log := newLogger(cfg)
	env := cfg.Env // the environment contract, parsed once (config.Env)
	if certJoin {
		// No `?token=`: the worker's certificate (OPOD_NODE_CERT/KEY) is the
		// credential the leader sees, and its mTLS gate needs no shared secret
		// (nodeMtls=require is exactly a leader that refuses one). The token
		// has a second job, though: it is the secret the LEADER signs its calls
		// to THIS worker's API with (the register request hands it over as the
		// worker token). That job is per process and needs no operator, so a
		// certificate join mints its own and sends it to the leader over the
		// mTLS-protected register — a pod under `require` then carries no
		// join token at all (PLAN T7.3).
		if env.NodeCert == "" || env.NodeKey == "" {
			die("join URL has no ?token= and no worker certificate is set (OPOD_NODE_CERT/OPOD_NODE_KEY): one or the other identifies this worker")
		}
		token = generateCallbackSecret()
	}
	if gpuIndex == "" {
		gpuIndex = env.GPUIndex
	}
	if vramBudget == "" {
		vramBudget = env.VRAMBudgetGB
	}

	caps := agent.Detect()
	switch role := strings.TrimSpace(env.WorkerRole); role {
	case "", "prefill", "decode":
		caps.Role = role
	default:
		die("OPOD_WORKER_ROLE %q: use prefill or decode, or leave it unset", role)
	}
	// R15.17: the plan revision this process serves. A manager that splits
	// traffic between revisions sets it; a bad value is ignored rather than
	// fatal, because a worker that will not start is worse than one the leader
	// simply cannot weight.
	if rev := strings.TrimSpace(env.PlanRevision); rev != "" {
		if n, err := strconv.Atoi(rev); err == nil && n > 0 {
			caps.PlanRevision = n
		} else {
			warn(os.Stdout, "OPOD_PLAN_REVISION %q is not a positive number — this worker joins the unversioned group", rev)
		}
	}
	addr, err := mesh.NewLAN().Address(8081) // workers default to :8081
	if err != nil {
		warn(os.Stdout, "could not determine local address: %v", err)
		addr = "0.0.0.0:8081"
	}
	// Honor an explicit advertise address. The auto-detected LAN IP is the
	// container/host default-route address (e.g. docker's 172.17.0.x), which the
	// leader can't reach for NAT'd/overlay workers. When the worker is on an
	// overlay or has multiple NICs, OPOD_ADVERTISE_ADDR pins the address the
	// leader should dial (e.g. the tailnet IP:8081).
	if adv := strings.TrimSpace(env.AdvertiseAddr); adv != "" {
		addr = adv
		log.Info("using advertised address from OPOD_ADVERTISE_ADDR", "addr", addr)
	}
	// Keep a STABLE identity across container recreates/restarts so we don't
	// orphan a "stale · no heartbeat" registration on the leader every time.
	// Precedence: OPOD_NODE_ID env (deterministic — a manager sets one per
	// slot) > a persisted node.yaml from a prior run > the pod's name (POD_NAME,
	// which any Kubernetes manifest can inject and which is stable for a
	// StatefulSet-like slot and at worst per pod) > a fresh random id. The
	// POD_NAME rung exists because in a pod DataDir is not a volume, so the
	// node.yaml rung never fires and a random id per restart is what a
	// hand-rolled or third-party worker would otherwise get (PLAN T15.15).
	nodeID := nodeIDFrom(env.NodeID, readPersistedNodeID(filepath.Join(cfg.DataDir, "node.yaml")), env.PodName)

	// Record this join. Only node_id is read back (readPersistedNodeID, the
	// next `opod join`); the other fields are a record for the operator.
	nodeCfg := NodeConfig{
		NodeID:    nodeID,
		LeaderURL: leader,
		Token:     token,
		Address:   addr,
		JoinedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if err := nodeCfg.Save(filepath.Join(cfg.DataDir, "node.yaml")); err != nil {
		warn(os.Stdout, "could not persist node config: %v", err)
	}

	// Local engine — the worker proxies its inference requests to this.
	eng := newEngineFromConfig(cfg)
	caps.Engine = engines.Canonical(eng.Name()) // registered with the hardware: the leader learns what a load does here
	// One alias table, written by the load handler and read by the heartbeat.
	aliases := &agent.Aliases{}

	// The leader may speak TLS with a certificate the control plane minted
	// (OPOD_LEADER_CA names it); the client trusts exactly that. And with
	// OPOD_NODE_CERT/KEY this worker presents its OWN certificate, which is what
	// identifies it to a leader running node mTLS (R9.6) — the join token stays
	// the credential everywhere else.
	leaderClient, err := agent.NewLeaderClientWithIdentity(env.LeaderCA, env.NodeCert, env.NodeKey, 10*time.Second)
	if err != nil {
		die("%v", err)
	}
	// Prefix-cache block events (feature "kv_block_events"): one translator,
	// fed by the server's subscriber and drained by the agent's heartbeat.
	var blocks *kvevents.Translator
	if env.KVEvents {
		blocks = kvevents.NewTranslator()
	}
	a := &agent.Agent{
		Blocks:            blocks,
		NodeID:            nodeID,
		LeaderURL:         leader,
		Token:             token,
		Address:           addr,
		Capabilities:      caps,
		Engine:            eng,
		Aliases:           aliases,
		HTTP:              leaderClient,
		HeartbeatInterval: 5 * time.Second,
		Log:               log,
	}

	// Worker HTTP server — leader will call into here for inference AND
	// for launching/stopping shard processes (rpc-server etc).
	sup := agent.NewSupervisor(log)
	sup.BaseEnv = workerBaseEnv(gpuIndex, vramBudget)
	// The heartbeat's engine state (feature "engine_liveness") is read off THIS
	// supervisor — it is the only thing that knows the engine process crashed,
	// because the pod stays Running and the container reports nothing.
	//
	// It was never wired. The Agent above was built before the supervisor
	// existed, so `Procs` stayed nil, `engineState` returned nil on every
	// heartbeat and the field was never sent — while the leader advertised the
	// feature, ingested it, and counted zero. Found on the design-partner cell
	// 2026-09-27 by crash-looping an engine on purpose (a flag llama.cpp does
	// not take) and watching the endpoint sit at "converging" with an empty
	// lastError for ten minutes, which is the exact defect the feature exists
	// to end.
	a.Procs = sup
	adapters, adaptersErr := agent.ParseAdapters(env.Adapters)
	srv := &agent.Server{
		Engine:     eng,
		Token:      token,
		Supervisor: sup,
		// Models dir is shared with local engine storage — leader's GGUF
		// distribution writes here under sha256-verified filenames so
		// sharded models don't require manually scp'd weights.
		ModelsDir: cfg.Storage.ModelsDir,
		// llama.cpp runs on the CPU unless told to offload; the worker has to
		// know whether it is holding a card to make that call.
		Accelerated:  agent.AcceleratorPresent(caps, env.Accelerator),
		Aliases:      aliases,
		EngineFlags:  agent.ParseEngineFlags(env.EngineFlags),
		Adapters:     adapters,
		AdaptersErr:  adaptersErr,
		RejectBearer: env.RejectBearer,
		SleepMode:    env.SleepMode,
		KVEvents:     env.KVEvents,
		Blocks:       blocks,
		HFToken:      env.HFToken,
		HFEndpoint:   env.HFEndpoint,
		// R15.16: the version this worker serves, pinned by the manager.
		ModelRevision: env.ModelRevision,
		ModelSHA256:   env.ModelSHA256,
		Log:           log,
	}
	defer sup.StopAll()

	ok(os.Stdout, "joining cluster at %s as %s", leader, nodeID)
	note(os.Stdout, "address: %s", addr)
	note(os.Stdout, "hardware: %s/%s · %d GB RAM", caps.OS, caps.Arch, caps.RAMGB)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)

	// T10.7: the model this worker exists to serve (OPOD_LOAD_MODEL), loaded
	// by the worker itself once its engine answers. The container entrypoint
	// used to do it with `curl -H "Authorization: Bearer $OPOD_JOIN_TOKEN"`
	// against this process's own API — the one caller of /v1/model/load that
	// could not sign HMAC, because a shell cannot, which is what made
	// OPOD_REJECT_BEARER=1 unusable. Nothing about the load needed HTTP.
	if req, ok := selfLoadRequest(cfg, srv); ok {
		go srv.SelfLoad(ctx, req)
	}

	// Heartbeat loop. On the way out it says goodbye (T11.2): the engine dies
	// with this process, and a leader that only learns it by silence goes on
	// routing into a dead engine for the heartbeat bound — which is a 502 to a
	// caller, the one error class a client cannot act on.
	go func() {
		defer wg.Done()
		defer a.Goodbye(ctx)
		if err := a.Loop(ctx); err != nil && err != context.Canceled {
			log.Error("agent loop exited", "err", err)
			cancel()
		}
	}()

	// Worker HTTP server (binds to the tailnet/LAN address from mesh)
	go func() {
		defer wg.Done()
		if err := srv.Start(ctx, addr); err != nil && err != context.Canceled {
			log.Error("worker server exited", "err", err)
			cancel()
		}
	}()

	wg.Wait()
	ok(os.Stdout, "worker shutdown complete")
}

// selfLoadRequest is the load OPOD_LOAD_MODEL asks for: the catalog entry's
// repo and file unless the manager overrode them (OPOD_LOAD_REPO /
// OPOD_LOAD_FILE), and by path when the whole file is already cached and the
// version is not pinned (Server.LoadBody). A catalog the worker cannot read is
// a warning, not a refusal: with a repo and a file given it does not need one,
// and without them the leader can still place a model on this worker.
func selfLoadRequest(cfg *config.Config, srv *agent.Server) (agent.LoadRequest, bool) {
	id := strings.TrimSpace(cfg.Env.LoadModel)
	if id == "" {
		return agent.LoadRequest{}, false
	}
	repo, file := strings.TrimSpace(cfg.Env.LoadRepo), strings.TrimSpace(cfg.Env.LoadFile)
	if repo == "" {
		entries, err := loadCatalog(cfg)
		if err != nil {
			warn(os.Stdout, "OPOD_LOAD_MODEL=%s: catalog unreadable (%v) — loading by id alone", id, err)
		}
		for _, e := range entries {
			if e.ID == id {
				repo, file = e.Source.Repo, e.Source.File
				break
			}
		}
	}
	return srv.LoadBody(id, repo, file), true
}

func parseJoinTarget(raw string) (leader, token string, err error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("invalid URL: %w", err)
	}
	q := u.Query()
	// The token is optional here and required by the caller unless the worker
	// has a certificate (R9.6): under node mTLS the certificate is the
	// credential and there is no shared secret to put in the URL.
	token = q.Get("token")
	// A bare host was upgraded to http:// above. Warn when the join token
	// would travel in cleartext to a non-local leader — it should go over
	// https (or a private tailnet) for anything but localhost.
	if host := u.Hostname(); token != "" && u.Scheme == "http" &&
		host != "localhost" && host != "127.0.0.1" && host != "::1" {
		warn(os.Stderr, "joining %s over cleartext http — the join token will be sent unencrypted; prefer https for non-local leaders", host)
	}
	u.RawQuery = ""
	leader = strings.TrimRight(u.String(), "/")
	return leader, token, nil
}

// generateCallbackSecret is the per-process secret a certificate join gives the
// leader for its calls back into this worker (see cmdJoin). 32 random bytes:
// it never leaves the register body and nobody types it.
func generateCallbackSecret() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return "sk-orc-cb-" + base64.RawURLEncoding.EncodeToString(buf)
}

func generateNodeID() string {
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	return "n_" + base64.RawURLEncoding.EncodeToString(buf)
}

// nodeIDFrom is the identity precedence: an explicit id, a persisted one, the
// pod's name, and only then a random one.
func nodeIDFrom(explicit, persisted, podName string) string {
	if v := strings.TrimSpace(explicit); v != "" {
		return v
	}
	if v := strings.TrimSpace(persisted); v != "" {
		return v
	}
	if v := strings.TrimSpace(podName); v != "" {
		return "n_" + v
	}
	return generateNodeID()
}

// readPersistedNodeID is the node id a prior run wrote, or "".
func readPersistedNodeID(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var prev NodeConfig
	if yaml.Unmarshal(b, &prev) != nil {
		return ""
	}
	return prev.NodeID
}

// NodeConfig is the per-worker config saved at ~/.opod/node.yaml.
type NodeConfig struct {
	NodeID    string `yaml:"node_id"`
	LeaderURL string `yaml:"leader_url"`
	Token     string `yaml:"token"`
	Address   string `yaml:"address"`
	JoinedAt  string `yaml:"joined_at"`
}

func (n *NodeConfig) Save(path string) error {
	data, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// loadNodeConfig is intentionally unexported and currently unused; it's the
// piece a future "worker resumes from saved state on `opod up`" feature
// would call. Marked with `var _` so go vet / unused linters don't object.
//
//nolint:unused
func loadNodeConfig(path string) (*NodeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var n NodeConfig
	if err := yaml.Unmarshal(data, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

var _ = loadNodeConfig // reserve for v0.5 worker resume
