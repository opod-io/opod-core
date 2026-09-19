package scheduler

// The shard-count picker: `opod shard create <model>` with no shape at all.
//
// It is a CLI default and nothing else. A caller that names a shape — a count,
// a node list, a tp/pp split, or any body sent to POST /admin/v1/shards/create
// — is never second-guessed: the admin API does not pick (shardCountFor), so a
// caller that computed its own shape and this picker can never disagree.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/lifecycle"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// WorkerMemory is what the leader knows about one live worker's memory.
type WorkerMemory struct {
	NodeID        string
	CapacityBytes int64 // the budget: GPU memory when the worker reported cards, else RAM, less the reserve
	ResidentBytes int64 // what the leader's placement and shard rows say is already there
	FreeBytes     int64
}

// ShardPick is the picker's answer and the reasoning it prints.
type ShardPick struct {
	Shards    int
	Nodes     []string // the workers the parts go to, most free memory first
	NeedBytes int64
	PartBytes int64 // what each part must hold (parts are taken as equal)
	Why       string
}

// PickShards chooses the smallest number of equal parts that fits:
//
//   - a model that fits the worker with the most free memory is 1 part;
//   - otherwise the smallest N whose N freest workers each hold need/N;
//   - never more parts than live workers, nor than the model's layers when the
//     catalog records them (a layer is the smallest thing either backend cuts);
//   - when nothing fits, a refusal that names the numbers.
//
// Parts are taken as equal. That is exact for pipeline stages and conservative
// for llama.cpp's RPC split, which weights the cut by device memory and so only
// does better than this.
//
// needBytes is the model's footprint (lifecycle.Footprint of its weights);
// layers is 0 when unknown. Pure: no clock, no store, no network.
func PickShards(needBytes int64, layers int, workers []WorkerMemory) (ShardPick, error) {
	if needBytes <= 0 {
		return ShardPick{}, fmt.Errorf("the model's size is unknown, so a shard count cannot be picked — name one")
	}
	if len(workers) == 0 {
		return ShardPick{}, fmt.Errorf("no live workers — join one (`opod join`) or name the count")
	}
	byFree := append([]WorkerMemory(nil), workers...)
	sort.SliceStable(byFree, func(i, j int) bool {
		if byFree[i].FreeBytes != byFree[j].FreeBytes {
			return byFree[i].FreeBytes > byFree[j].FreeBytes
		}
		return byFree[i].NodeID < byFree[j].NodeID
	})
	limit, capped := len(byFree), ""
	if layers > 0 && layers < limit {
		limit, capped = layers, fmt.Sprintf(" (capped at the model's %d layers)", layers)
	}
	for n := 1; n <= limit; n++ {
		part := (needBytes + int64(n) - 1) / int64(n)
		if byFree[n-1].FreeBytes < part {
			continue
		}
		pick := ShardPick{Shards: n, NeedBytes: needBytes, PartBytes: part}
		held := make([]string, 0, n)
		for _, w := range byFree[:n] {
			pick.Nodes = append(pick.Nodes, w.NodeID)
			held = append(held, fmt.Sprintf("%s (%s free)", w.NodeID, gb(w.FreeBytes)))
		}
		if n == 1 {
			pick.Why = fmt.Sprintf("the model needs %s and fits whole on %s", gb(needBytes), held[0])
		} else {
			pick.Why = fmt.Sprintf("the model needs %s; the freest worker has %s, so it does not fit in fewer parts; "+
				"%d parts of %s fit on %s", gb(needBytes), gb(byFree[0].FreeBytes), n, gb(part), strings.Join(held, ", "))
		}
		return pick, nil
	}
	free := make([]string, 0, len(byFree))
	var total int64
	for _, w := range byFree {
		free = append(free, fmt.Sprintf("%s %s", w.NodeID, gb(w.FreeBytes)))
		total += w.FreeBytes
	}
	lastPart := (needBytes + int64(limit) - 1) / int64(limit)
	return ShardPick{}, fmt.Errorf("the model needs %s and no split fits: %d live worker(s) have %s free in total (%s); "+
		"even %d part(s)%s need %s each and worker %s has %s — free memory, join more workers, or name the count yourself",
		gb(needBytes), len(byFree), gb(total), strings.Join(free, ", "),
		limit, capped, gb(lastPart), byFree[limit-1].NodeID, gb(byFree[limit-1].FreeBytes))
}

// ShardNeedBytes is the footprint the picker sizes against: the lifecycle's
// estimate over the catalog's weight size, or its hardware floor when the size
// is not recorded. 0 = unknown.
func ShardNeedBytes(entry models.Entry) int64 {
	if entry.SizeBytes > 0 {
		return lifecycle.Footprint(entry.SizeBytes)
	}
	if floor := max(entry.Hardware.MinVRAMGB, entry.Hardware.MinRAMGB); floor > 0 {
		return int64(floor) << 30
	}
	return 0
}

// WorkerMemoryFacts reads, from the leader's own rows, how much memory every
// live worker has free. Nothing is probed: capacity is what the worker
// registered (its cards' memory, else its RAM) under the lifecycle's reserve,
// and residency is what heartbeats and shard creates already recorded, sized
// by the lifecycle's footprint estimate. A model whose size nobody recorded
// counts as 0, the same optimism admission applies.
//
// replacing names a model whose present gang is ignored, because `shard
// create` tears it down before building the new one. The leader's own "local"
// row is not a worker and is never a candidate.
func WorkerMemoryFacts(ctx context.Context, st store.Store, cat []models.Entry, replacing string,
	reservePercent int, maxAge time.Duration, now time.Time) ([]WorkerMemory, error) {
	nodes, err := st.Nodes().List(ctx)
	if err != nil {
		return nil, err
	}
	installed, err := st.Models().List(ctx)
	if err != nil {
		return nil, err
	}
	shards, err := st.Shards().List(ctx)
	if err != nil {
		return nil, err
	}

	// A placement row carries the engine's own name for a model; the models
	// table says which catalog id and size that is.
	need := map[string]int64{}
	for _, e := range cat {
		need[e.ID] = ShardNeedBytes(e)
	}
	for _, m := range installed {
		size := lifecycle.Footprint(m.SizeBytes)
		if size == 0 {
			size = need[m.CatalogID]
		}
		need[m.ID] = size
		if _, native, ok := strings.Cut(m.Source, ":"); ok && native != "" {
			need[native] = size
		}
	}

	// Gangs: every part holds an equal share; a gang of one is the whole model
	// on its coordinator's node.
	parts, sharded := map[string][]string{}, map[string]bool{}
	coordinator := map[string]string{}
	for _, sh := range shards {
		sharded[sh.ModelID] = true
		switch sh.Role {
		case "coordinator":
			coordinator[sh.ModelID] = sh.NodeID
		default: // rpc | rank
			parts[sh.ModelID] = append(parts[sh.ModelID], sh.NodeID)
		}
	}
	resident := map[string]int64{}
	for model := range sharded {
		if model == replacing {
			continue
		}
		holders := parts[model]
		if len(holders) == 0 {
			holders = []string{coordinator[model]}
		}
		for _, node := range holders {
			resident[node] += need[model] / int64(len(holders))
		}
	}

	out := make([]WorkerMemory, 0, len(nodes))
	for _, n := range nodes {
		if n.ID == "local" || n.State != "ready" || n.Address == "" || now.Sub(n.LastHeartbeat) > maxAge {
			continue
		}
		w := WorkerMemory{NodeID: n.ID, CapacityBytes: lifecycle.Budget(workerMemoryBytes(n), reservePercent), ResidentBytes: resident[n.ID]}
		placed, err := st.Placements().GetByNode(ctx, n.ID)
		if err != nil {
			return nil, err
		}
		for _, p := range placed {
			if !sharded[p.ModelID] { // a gang is already counted by its parts
				w.ResidentBytes += need[p.ModelID]
			}
		}
		w.FreeBytes = max(0, w.CapacityBytes-w.ResidentBytes)
		out = append(out, w)
	}
	return out, nil
}

// workerMemoryBytes is the memory a worker serves from: its cards when it
// reported any with a size, else its RAM (CPU hosts, unified memory).
func workerMemoryBytes(n store.Node) int64 {
	var caps agent.Capabilities
	if n.HardwareJSON != "" && json.Unmarshal([]byte(n.HardwareJSON), &caps) == nil {
		vram := 0
		for _, g := range caps.GPUs {
			vram += g.VRAMGB
		}
		if vram > 0 {
			return int64(vram) << 30
		}
	}
	return int64(n.RAMGB) << 30
}

func gb(b int64) string { return fmt.Sprintf("%.1f GB", float64(b)/float64(1<<30)) }
