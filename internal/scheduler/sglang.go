package scheduler

// SGLang's own multi-node scheme, as a third gang backend.
//
// The two backends that came before it both put something BETWEEN the engine
// and the machines: llama.cpp gets an rpc-server on each part plus a
// coordinator that dials them, and vLLM gets a Ray cluster that the engine then
// discovers. SGLang needs neither. Its launcher is already distributed: the
// same `sglang.launch_server` command runs on every machine, they find each
// other through one rendezvous address, and rank 0 serves the HTTP API for the
// whole group.
//
//	--dist-init-addr <rank0>:<port>   the rendezvous every rank dials
//	--nnodes <N> --node-rank <i>      which machine this is
//	--tp-size <T>                     the TENSOR width over the WHOLE gang,
//	                                  not per machine
//	--pp-size <P>                     pipeline stages, when asked for
//
// So this file is the simplest of the three: start the same process N times
// with one number different, and register rank 0 as the coordinator the router
// dials.
//
// WHAT MAKES IT A DECISION RATHER THAN A MECHANISM. `--tp-size` spanning
// machines is tensor parallel ACROSS NODES: every all-reduce of every token
// crosses the network. Until 2026-09-22 the product refused that shape outright
// (D8, "a tensor group never crosses a fabric boundary"), which meant SGLang
// had no multi-node path at any width — its native scheme IS the forbidden
// shape. ADR-068 dropped the rule: the shape is allowed and what it costs is
// said out loud, in the plan-time advisory, rather than being refused. That is
// why this backend exists, and why the log line below is emphatic.
//
// Pipeline is the cheap shape on ordinary networking and it is available here
// too (`--pp-size`, a create's `pp`), which is what a caller who cannot put the
// whole tensor group on one machine should reach for first.

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

// Ports the gang starts from. Both go through the create's allocator, so two
// gangs of one model that share a machine never collide (build item 6).
const (
	sglangDistPortBase  = 29500 // torch's own default rendezvous port
	sglangServePortBase = 30000 // SGLang's default HTTP port
)

// createShardedSGLang starts one `sglang.launch_server` per part and registers
// rank 0 as the gang's coordinator.
//
// The parts are started in rank order, rank 0 first: it owns the rendezvous
// address, and a joining rank that dials it before it exists retries rather
// than failing, so starting it first only shortens the wait. Every part that
// starts is recorded before the next one is attempted, so a failure half way
// through has something to roll back.
func (o *Orchestrator) createShardedSGLang(ctx context.Context, entry models.Entry, gangID string, workers []store.Node, par Parallelism, ports *portAllocator) error {
	if len(workers) < 1 {
		return fmt.Errorf("sglang sharding needs at least one worker")
	}
	split, err := par.resolve(len(workers))
	if err != nil {
		return fmt.Errorf("parallelism: %w", err)
	}
	rank0 := workers[0]
	rendezvousHost := hostOf(rank0.Address)
	if rendezvousHost == "" {
		return fmt.Errorf("sglang rank 0 node %s has no advertised address", rank0.ID)
	}
	// The rendezvous port is rank 0's; every other rank only dials it, so it is
	// taken on rank 0's node alone.
	distPort := ports.take(rank0.ID, sglangDistPortBase)

	model := entry.Source.Repo
	if model == "" {
		model = entry.Source.Path
	}
	if model == "" {
		model = entry.ID
	}
	if split.TP > split.DevicesPerRank {
		o.Log.Warn("tensor parallel ACROSS MACHINES (sglang): every all-reduce of every token crosses the network, "+
			"so throughput is bounded by it — pipeline (--pp) pays one hop per token instead. Allowed since ADR-068; the cost is yours to accept",
			"model", entry.ID, "gang", gangID, "tp", split.TP, "pp", split.PP, "nodes", len(workers))
	}

	var created []store.Shard
	for rank, w := range workers {
		host := hostOf(w.Address)
		if host == "" {
			o.rollback(ctx, created)
			return fmt.Errorf("sglang rank %d node %s has no advertised address", rank, w.ID)
		}
		servePort := ports.take(w.ID, sglangServePortBase)
		spec := agent.ProcessSpec{
			// Keyed by RANK, never by node: a gang may put two parts on one host
			// (build item 5), and a node-keyed id makes the second collide with
			// the first.
			ID:      gangShardID(entry.ID, gangID, fmt.Sprintf("sglang-%d", rank)),
			Command: "/bin/sh",
			Args: []string{"-lc", sglangGangCommand(sglangGang{
				Model:     model,
				ServedAs:  entry.ID,
				ServePort: servePort,
				DistAddr:  rendezvousHost + ":" + strconv.Itoa(distPort),
				NNodes:    len(workers),
				NodeRank:  rank,
				TP:        split.TP,
				PP:        split.PP,
			})},
			// No HealthPort: SGLang binds its HTTP port only after the weights are
			// loaded and the group has formed, which is minutes on a model worth
			// splitting. Gating the launch on it would time out — the same call
			// the vLLM backend makes.
			Restart: false,
		}
		if _, err := o.callWorkerStart(ctx, w, spec); err != nil {
			o.rollback(ctx, created)
			return fmt.Errorf("start sglang rank %d on %s: %w", rank, w.ID, err)
		}
		row := store.Shard{
			ID: gangShardID(entry.ID, gangID, fmt.Sprintf("rank-%d", rank)), ModelID: entry.ID, GangID: gangID,
			Role: "rank", NodeID: w.ID, Address: host, ProcessID: spec.ID,
			Status: "ready", CreatedAt: time.Now(), LastSeen: time.Now(),
		}
		if rank == 0 {
			// Rank 0 IS the coordinator here — the same process both joins the
			// group and serves the API — so it gets the coordinator row and no
			// rank row beside it. Two rows over one process id would have the
			// teardown stop it twice and log the second as a failure.
			row.ID = gangShardID(entry.ID, gangID, "sglang-coord")
			row.Role = "coordinator"
			row.Address = host + ":" + strconv.Itoa(servePort)
			row.ConfigJSON = `{"engine":"sglang"}` // the router picks the driver by it
		}
		if err := o.Store.Shards().Create(ctx, row); err != nil {
			_ = o.callWorkerStop(ctx, w, spec.ID)
			o.rollback(ctx, created)
			return fmt.Errorf("persist sglang rank %d: %w", rank, err)
		}
		created = append(created, row)
		o.Log.Info("sglang rank started", "model", entry.ID, "gang", gangID, "rank", rank, "node", w.ID)
	}

	// The router reads a placement, exactly as for the other two backends.
	if err := o.Store.Placements().Upsert(ctx, store.Placement{NodeID: "local", ModelID: entry.ID, Status: "ready", LastSeen: time.Now()}); err != nil {
		o.Log.Warn("placement upsert failed for sglang gang", "model", entry.ID, "err", err)
	}
	o.Log.Info("sglang gang up", "model", entry.ID, "gang", gangID, "nodes", len(workers),
		"tp", split.TP, "pp", split.PP, "coordinator", created[0].Address,
		"note", "SGLang is loading and forming the group — first requests warm it up")
	return nil
}

// sglangGang is one rank's whole command shape, named so the test reads as the
// flags a machine is handed rather than as a format string.
type sglangGang struct {
	Model     string // what the engine loads (a Hub repo, or a path)
	ServedAs  string // the catalog id, so routing needs no name translation
	ServePort int
	DistAddr  string // host:port of rank 0's rendezvous
	NNodes    int
	NodeRank  int
	TP        int
	PP        int
}

// sglangGangCommand is the shell line one rank runs.
//
// `--host 0.0.0.0`, not the loopback the single-node launch uses: the leader
// dials rank 0 from another machine, and a server bound to 127.0.0.1 would
// answer nothing outside its own container.
//
// `--pp-size` is written only when the caller asked for more than one stage.
// Pipeline parallelism is version-bound in SGLang (the engine catalog says so),
// and an older build refuses an unknown flag at startup — so a gang that never
// asked for a pipeline must not be made to depend on the flag existing.
func sglangGangCommand(g sglangGang) string {
	args := []string{
		"exec python3 -m sglang.launch_server",
		"--model-path '" + shellQuote(g.Model) + "'",
		"--served-model-name '" + shellQuote(g.ServedAs) + "'",
		"--host 0.0.0.0",
		"--port " + strconv.Itoa(g.ServePort),
		"--trust-remote-code",
		// The fraction comes from the PART's VRAM budget, computed on the machine
		// by the one rule every start path uses (agent.MemFractionShell). A
		// hard-coded 0.85 here put a part on an 8 GB card at 85% of the CARD
		// rather than the 6 GB its plan gave it, and the engine died allocating
		// its attention workspace with 512 KiB free — a per-worker VRAM budget
		// that one start path ignores is not a budget.
		"--mem-fraction-static " + agent.MemFractionVar,
		"--tp-size " + strconv.Itoa(g.TP),
	}
	if g.PP > 1 {
		args = append(args, "--pp-size "+strconv.Itoa(g.PP))
	}
	args = append(args,
		"--dist-init-addr "+g.DistAddr,
		"--nnodes "+strconv.Itoa(g.NNodes),
		"--node-rank "+strconv.Itoa(g.NodeRank),
	)
	// The budget line runs BEFORE the exec: it writes $U, which the fraction
	// flag above reads.
	return agent.MemFractionShell("0.85") + strings.Join(args, " ")
}

// shellQuote escapes a value for the single quotes it is placed inside.
func shellQuote(s string) string { return strings.ReplaceAll(s, "'", `'\''`) }

// isSGLangBackend reports whether a sharding spec targets SGLang's own
// multi-node launcher rather than llama.cpp's RPC parts or vLLM's Ray cluster.
func isSGLangBackend(engine string) bool {
	e := strings.ToLower(strings.TrimSpace(engine))
	return e == "sglang" || e == "sglang-native" || e == "sglang_native"
}
