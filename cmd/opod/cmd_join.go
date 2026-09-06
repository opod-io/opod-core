package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/mesh"

	"gopkg.in/yaml.v3"
)

// cmdJoin connects this machine to an existing Opod cluster as a worker.
//
// Usage:
//
//	opod join http://leader:8080?token=sk-orc-...
//
// mustSetenv sets a process env var or dies: a failed Setenv here would
// silently hand the engine the wrong GPU, which is worse than exiting.
func mustSetenv(k, v string) {
	if err := os.Setenv(k, v); err != nil {
		die("setenv %s: %v", k, err)
	}
}

func cmdJoin(args []string) {
	if wantsHelp(args) {
		showHelp(helpSpec{
			name:    "join",
			summary: "join this machine to an existing opod cluster as a worker",
			usage:   "opod join <leader-url>?token=<token> [--gpu <index>] [--vram-budget <GB>]",
			examples: []string{
				"opod join http://leader.local:8080?token=sk-orc-...",
				"opod join https://opod.example.com?token=sk-orc-...",
				"opod join http://leader:8080?token=sk-orc-... --gpu 1 --vram-budget 20",
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
		die("usage: opod join <leader-url>?token=<token>  (run `opod join --help` for details)")
	}
	leader, token, err := parseJoinTarget(args[0])
	if err != nil {
		die("%v", err)
	}
	// Placement hints for the engine this worker will supervise. They are
	// exported as env so every engine launcher (vLLM, llama.cpp, …) reads one
	// contract; a control plane sets the same env on the pod instead.
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--gpu":
			if i+1 >= len(args) {
				die("--gpu needs a device index")
			}
			i++
			mustSetenv("OPOD_GPU_INDEX", args[i])
			for _, k := range []string{"CUDA_VISIBLE_DEVICES", "HIP_VISIBLE_DEVICES", "ONEAPI_DEVICE_SELECTOR"} {
				if os.Getenv(k) == "" {
					if k == "ONEAPI_DEVICE_SELECTOR" {
						mustSetenv(k, "level_zero:"+args[i])
					} else {
						mustSetenv(k, args[i])
					}
				}
			}
		case "--vram-budget":
			if i+1 >= len(args) {
				die("--vram-budget needs a size in GB")
			}
			i++
			if _, err := strconv.Atoi(args[i]); err != nil {
				die("--vram-budget must be an integer number of GB, got %q", args[i])
			}
			mustSetenv("OPOD_VRAM_BUDGET_GB", args[i])
		default:
			die("unknown join argument %q (see `opod join --help`)", args[i])
		}
	}

	cfg := loadConfigOrExit()
	log := newLogger(cfg)

	caps := agent.Detect()
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
	if adv := strings.TrimSpace(os.Getenv("OPOD_ADVERTISE_ADDR")); adv != "" {
		addr = adv
		log.Info("using advertised address from OPOD_ADVERTISE_ADDR", "addr", addr)
	}
	// Keep a STABLE identity across container recreates/restarts so we don't
	// orphan a "stale · no heartbeat" registration on the leader every time.
	// Precedence: OPOD_NODE_ID env (deterministic — Fleet sets one per machine) >
	// a persisted node.yaml from a prior run > a fresh random id.
	nodeID := generateNodeID()
	if v := strings.TrimSpace(os.Getenv("OPOD_NODE_ID")); v != "" {
		nodeID = v
	} else if b, err := os.ReadFile(filepath.Join(cfg.DataDir, "node.yaml")); err == nil {
		var prev NodeConfig
		if yaml.Unmarshal(b, &prev) == nil && prev.NodeID != "" {
			nodeID = prev.NodeID
		}
	}

	// Persist node config so a subsequent `opod up` enters worker mode.
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

	a := &agent.Agent{
		NodeID:            nodeID,
		LeaderURL:         leader,
		Token:             token,
		Address:           addr,
		Capabilities:      caps,
		Engine:            eng,
		HTTP:              &http.Client{Timeout: 10 * time.Second},
		HeartbeatInterval: 5 * time.Second,
		Log:               log,
	}

	// Worker HTTP server — leader will call into here for inference AND
	// for launching/stopping shard processes (rpc-server etc).
	sup := agent.NewSupervisor(log)
	srv := &agent.Server{
		Engine:     eng,
		Token:      token,
		Supervisor: sup,
		// Models dir is shared with local engine storage — leader's GGUF
		// distribution writes here under sha256-verified filenames so
		// sharded models don't require manually scp'd weights.
		ModelsDir: cfg.Storage.ModelsDir,
	}
	defer sup.StopAll()

	ok(os.Stdout, "joining cluster at %s as %s", leader, nodeID)
	note(os.Stdout, "address: %s", addr)
	note(os.Stdout, "hardware: %s/%s · %d GB RAM", caps.OS, caps.Arch, caps.RAMGB)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)

	// Heartbeat loop
	go func() {
		defer wg.Done()
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

func parseJoinTarget(raw string) (leader, token string, err error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("invalid URL: %w", err)
	}
	q := u.Query()
	token = q.Get("token")
	if token == "" {
		return "", "", fmt.Errorf("token query param required")
	}
	// A bare host was upgraded to http:// above. Warn when the join token
	// would travel in cleartext to a non-local leader — it should go over
	// https (or a private tailnet) for anything but localhost.
	if host := u.Hostname(); u.Scheme == "http" &&
		host != "localhost" && host != "127.0.0.1" && host != "::1" {
		warn(os.Stderr, "joining %s over cleartext http — the join token will be sent unencrypted; prefer https for non-local leaders", host)
	}
	u.RawQuery = ""
	leader = strings.TrimRight(u.String(), "/")
	return leader, token, nil
}

func generateNodeID() string {
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	return "n_" + base64.RawURLEncoding.EncodeToString(buf)
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
