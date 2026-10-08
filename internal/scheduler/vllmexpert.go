package scheduler

// vLLM's expert parallelism, as a fourth gang backend (PLAN T18.5).
//
// A mixture-of-experts model has many experts per layer and uses a few per
// token. Expert parallelism spreads the experts over the gang's devices and
// runs attention data-parallel beside them: every device is one data-parallel
// rank, and the engine moves each token to the ranks that hold its experts with
// an all-to-all every layer. vLLM does this with its own launcher — no Ray, no
// rpc-server — so this backend is shaped like SGLang's: the same `vllm serve`
// on every part with a few numbers different, and rank 0 serving the API.
//
//	--data-parallel-size <D>           every attention rank in the gang:
//	                                   parts × devices per part ÷ tensor
//	--data-parallel-size-local <L>     the ranks on THIS part (devices ÷ tensor)
//	--data-parallel-address <rank0>    the address every part dials
//	--data-parallel-rpc-port <P>       rank 0's coordination port
//	--data-parallel-start-rank <r×L>   the first global rank on this part
//	--headless                         every part but rank 0: no API server
//	--enable-expert-parallel           experts spread over all D × tensor devices
//
// One part holding every device is the in-machine shape: one process,
// `--data-parallel-size <devices> --enable-expert-parallel`, nothing to dial.
//
// What was run, not read: a two-part gang of one card each across two
// machines over plain ethernet formed with exactly these flags on vLLM 0.27.1
// and answered — each rank holding half of a 32-expert model. The one-process
// in-machine shape is the same engine path and has not been run on a machine
// with several cards yet.
//
// The collective libraries' interface pins (NCCL_SOCKET_IFNAME and its
// kind) are NOT named here: they name an interface of the machine the worker
// runs on, which the leader cannot know. Whoever renders the worker sets them
// in its environment, and every rank inherits it. The one thing the line does
// is make Gloo — the CPU collective vLLM's data-parallel group coordinates
// over — follow NCCL's pin when only NCCL's was given (gangIfnameShell).

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// Ports an expert gang starts from, through the create's allocator so two
// gangs sharing a machine never collide (build item 6).
const (
	vllmExpertServePortBase = 9100  // rank 0's HTTP API, the Ray backend's port
	vllmExpertRPCPortBase   = 13345 // rank 0's data-parallel coordination port
)

// expertSplit is an expert gang's arithmetic: ranks parts of DevicesPerRank
// devices each, tensor-parallel TP inside a part, every tensor group one
// attention data-parallel rank. There is no pipeline in an expert gang.
type expertSplit struct {
	TP    int // devices per data-parallel rank (1 = attention on every device)
	DP    int // data-parallel ranks over the whole gang
	Local int // data-parallel ranks on one part
}

// expertSplit resolves Parallelism for an expert gang of the given parts.
func (p Parallelism) expertSplit(parts int) (expertSplit, error) {
	k := max(1, p.DevicesPerRank)
	tp := max(1, p.TP)
	if p.PP > 1 {
		return expertSplit{}, fmt.Errorf("pp=%d: an expert-parallel gang has no pipeline — its experts are spread over every rank instead", p.PP)
	}
	if k%tp != 0 {
		return expertSplit{}, fmt.Errorf("tp=%d must divide the %d GPUs each part holds: an expert gang's tensor group stays inside one part", tp, k)
	}
	if parts < 1 {
		return expertSplit{}, fmt.Errorf("an expert gang needs at least one part")
	}
	return expertSplit{TP: tp, DP: parts * k / tp, Local: k / tp}, nil
}

// checkExpertCreate refuses an expert create the backend cannot run, before
// anything is torn down or started; a create that asks for no expert
// parallelism passes untouched.
func checkExpertCreate(entry models.Entry, parts int, par Parallelism) error {
	if !par.Expert {
		return nil
	}
	if !isVLLMRayBackend(entry.Sharding.Engine) {
		return fmt.Errorf("%w: expert parallelism is launched by vLLM here (sharding.engine=vllm); %s has no expert-parallel backend in this leader", ErrUnplaceable, nonEmpty(entry.Sharding.Engine, "llama.cpp"))
	}
	if _, err := par.expertSplit(parts); err != nil {
		return fmt.Errorf("%w: %w", ErrUnplaceable, err)
	}
	return nil
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// headFirst puts the part the create named as the head at rank 0, keeping the
// others in order. A manager names the head and lists it first already; a
// caller that only named it is not made to know the list's order too.
func headFirst(workers []store.Node, head string) []store.Node {
	for i, w := range workers {
		if w.ID == head && i > 0 {
			out := append([]store.Node{w}, workers[:i]...)
			return append(out, workers[i+1:]...)
		}
	}
	return workers
}

// createShardedVLLMExpert starts one `vllm serve` per part and registers rank
// 0 as the gang's coordinator, the only process the router dials.
func (o *Orchestrator) createShardedVLLMExpert(ctx context.Context, entry models.Entry, gangID string, workers []store.Node, par Parallelism, ports *portAllocator) error {
	split, err := par.expertSplit(len(workers))
	if err != nil {
		return fmt.Errorf("parallelism: %w", err)
	}
	workers = headFirst(workers, par.Head)
	rank0 := workers[0]
	rank0Host := hostOf(rank0.Address)
	if rank0Host == "" {
		return fmt.Errorf("expert gang rank 0 node %s has no advertised address", rank0.ID)
	}
	rpcPort := ports.take(rank0.ID, vllmExpertRPCPortBase)
	servePort := ports.take(rank0.ID, vllmExpertServePortBase)
	model := entry.Source.Repo
	if model == "" {
		model = entry.Source.Path
	}
	if model == "" {
		model = entry.ID
	}
	overrides, extra := agent.EngineFlags(par.Flags).VLLMShellOverrides()

	var created []store.Shard
	for rank, w := range workers {
		host := hostOf(w.Address)
		if host == "" {
			o.rollback(ctx, created)
			return fmt.Errorf("expert gang rank %d node %s has no advertised address", rank, w.ID)
		}
		spec := agent.ProcessSpec{
			// Keyed by rank, never by node: two parts may share a machine.
			ID:      gangShardID(entry.ID, gangID, fmt.Sprintf("vllm-ep-%d", rank)),
			Command: "/bin/sh",
			Args: []string{"-lc", vllmExpertCommand(vllmExpertRank{
				Model: model, ServedAs: entry.ID, Rank: rank, Parts: len(workers), Split: split,
				Host: host, ServePort: servePort, Rank0Addr: rank0Host, RPCPort: rpcPort,
				Overrides: overrides, Extra: extra,
			})},
			Env: mergeEnv(distEnv(host), map[string]string{
				// Loading, then forming the data-parallel group across machines,
				// takes far longer than vLLM's default before it gives up.
				"VLLM_ENGINE_READY_TIMEOUT_S": "2400",
			}),
			// No HealthPort: rank 0 binds its API only after the group has formed
			// and the weights are loaded, and a headless rank binds none. No
			// restart: a rank restarted alone cannot rejoin a formed group — the
			// gang is re-formed whole, by the manager or the heal loop.
			Restart: false,
		}
		if _, err := o.callWorkerStart(ctx, w, spec); err != nil {
			o.rollback(ctx, created)
			return fmt.Errorf("start expert rank %d on %s: %w", rank, w.ID, err)
		}
		row := store.Shard{
			ID: gangShardID(entry.ID, gangID, fmt.Sprintf("rank-%d", rank)), ModelID: entry.ID, GangID: gangID,
			Role: "rank", NodeID: w.ID, Address: host, ProcessID: spec.ID,
			Status: "ready", CreatedAt: time.Now(), LastSeen: time.Now(),
		}
		if rank == 0 {
			// Rank 0 both holds experts and serves the API: one process, one
			// row — the coordinator the router dials.
			row.ID = gangShardID(entry.ID, gangID, "vllm-ep-coord")
			row.Role = "coordinator"
			row.Address = host + ":" + strconv.Itoa(servePort)
			row.ConfigJSON = `{"engine":"vllm"}` // the router picks the driver by it
		}
		if err := o.Store.Shards().Create(ctx, row); err != nil {
			_ = o.callWorkerStop(ctx, w, spec.ID)
			o.rollback(ctx, created)
			return fmt.Errorf("persist expert rank %d: %w", rank, err)
		}
		created = append(created, row)
		o.Log.Info("expert rank started", "model", entry.ID, "gang", gangID, "rank", rank, "node", w.ID,
			"dp_start", rank*split.Local, "dp_local", split.Local)
	}

	if err := o.Store.Placements().Upsert(ctx, store.Placement{NodeID: "local", ModelID: entry.ID, Status: "ready", LastSeen: time.Now()}); err != nil {
		o.Log.Warn("placement upsert failed for expert gang", "model", entry.ID, "err", err)
	}
	o.Log.Info("expert gang up", "model", entry.ID, "gang", gangID, "parts", len(workers),
		"dp", split.DP, "tp", split.TP, "ep", split.DP*split.TP, "coordinator", created[0].Address,
		"note", "vLLM is loading and forming the data-parallel group — first requests warm it up")
	return nil
}

// gangIfnameShell points Gloo at the interface the worker's environment pinned
// NCCL to, when it pinned NCCL and not Gloo. Both libraries open sockets
// between the ranks; one pinned and the other left to guess is a group whose
// data path works and whose coordination dials the wrong interface. It reads
// the worker's own environment and names no interface itself.
const gangIfnameShell = `[ -n "${NCCL_SOCKET_IFNAME:-}" ] && [ -z "${GLOO_SOCKET_IFNAME:-}" ] && export GLOO_SOCKET_IFNAME="$NCCL_SOCKET_IFNAME"; `

// vllmExpertRank is one rank's whole command shape, named so a test reads the
// flags a machine is handed rather than a format string.
type vllmExpertRank struct {
	Model     string // what the engine loads (a Hub repo, or a path)
	ServedAs  string // the catalog id the leader dispatches under
	Rank      int    // this part's place in the gang; 0 serves the API
	Parts     int
	Split     expertSplit
	Host      string // this part's address (rank 0 binds its API on it)
	ServePort int
	Rank0Addr string // rank 0's address, which every part dials
	RPCPort   int
	Overrides string // the gang's flag shell assignments (agent.EngineFlags)
	Extra     string // the gang's extra `vllm serve` arguments, already quoted
}

// vllmExpertCommand is the shell line one rank runs under a login shell, like
// every vLLM launch (the entry point is on the image's login PATH).
//
// The fraction comes from the part's VRAM budget by the one rule every start
// path uses (agent.MemFractionShell), and the budget line runs before the
// exec because it writes the $U the flag reads.
func vllmExpertCommand(r vllmExpertRank) string {
	args := []string{
		"exec vllm serve '" + shellQuote(r.Model) + "'",
		// Served under the catalog id AND the repo name, like the Ray backend:
		// the leader resolves a catalog id to the engine's native name.
		"--served-model-name '" + shellQuote(r.ServedAs) + "' '" + shellQuote(r.Model) + "'",
		"--trust-remote-code",
		"--gpu-memory-utilization " + agent.MemFractionVar,
	}
	if r.Split.TP > 1 {
		args = append(args, "--tensor-parallel-size "+strconv.Itoa(r.Split.TP))
	}
	args = append(args, "--data-parallel-size "+strconv.Itoa(r.Split.DP))
	if r.Parts > 1 {
		args = append(args,
			"--data-parallel-size-local "+strconv.Itoa(r.Split.Local),
			"--data-parallel-address "+r.Rank0Addr,
			"--data-parallel-rpc-port "+strconv.Itoa(r.RPCPort),
		)
	}
	args = append(args, "--enable-expert-parallel")
	if r.Rank == 0 {
		args = append(args, "--host "+r.Host, "--port "+strconv.Itoa(r.ServePort))
	} else {
		args = append(args, "--headless", "--data-parallel-start-rank "+strconv.Itoa(r.Rank*r.Split.Local))
	}
	if r.Extra != "" {
		args = append(args, r.Extra)
	}
	pre := gangIfnameShell + agent.MemFractionShell("0.85")
	if r.Overrides != "" {
		pre += r.Overrides + " "
	}
	return pre + strings.Join(args, " ")
}
