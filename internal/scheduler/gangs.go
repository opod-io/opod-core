package scheduler

// Several gangs of one model (build item 6, "multi-head routing").
//
// A gang is one complete, serving copy of a sharded model: N rpc parts plus the
// one coordinator that fronts them. A model may have several, and they are
// INDEPENDENT — each answers whole requests by itself, so adding a gang adds
// throughput, and the router picks among their coordinators the way it picks
// among workers. A part never spans gangs: a coordinator that dialled another
// gang's rpc part would split one request across two copies of the same layers
// and serve nonsense.
//
// Everything in this file exists because the model id used to BE the gang
// identity. Where that assumption showed up — process ids, ports, teardown,
// the orphan sweep — it had to be widened by one axis, and each of those is a
// place where getting the scope wrong stops a gang that is serving.

import (
	"context"
	"fmt"
	"sort"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// validGangID refuses a gang id that would corrupt the ids built from it.
//
// Gang ids land in process ids, which are matched by prefix — so a '-' inside
// one would make "g1" a prefix of "g1-2" and a teardown of the first would
// take the second with it. Keep them short and flat.
func validGangID(id string) error {
	if id == "" {
		return fmt.Errorf("gang id must not be empty")
	}
	if len(id) > 32 {
		return fmt.Errorf("gang id %q is too long (max 32)", id)
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return fmt.Errorf("gang id %q: use lower-case letters and digits only "+
				"(a '-' would make one gang id a prefix of another, and parts are matched by prefix)", id)
		}
	}
	return nil
}

// gangShardID builds the id of one part. The gang is always in it, so two gangs
// of one model can never collide on the shards table's primary key — which they
// did silently before, the second create overwriting the first gang's row.
func gangShardID(modelID, gangID, role string) string {
	return fmt.Sprintf("s-%s-%s-%s", safeID(modelID), safeID(gangID), role)
}

// stopOrphanGangProcs is stopOrphanShardProcs for ONE gang: it sweeps the fleet
// for processes left by a previous convergence of this gang and stops them,
// while every sibling gang keeps serving.
//
// The model-wide sweep cannot be used here. Its prefix ("s-<model>-") matches
// every gang, so using it to make room for gang g1 would kill g0's parts on
// each node the two share.
func (o *Orchestrator) stopOrphanGangProcs(ctx context.Context, entry models.Entry, gangID string) {
	nodes, err := o.Store.Nodes().List(ctx)
	if err != nil {
		return
	}
	for _, nd := range nodes {
		if nd.ID == "local" || nd.Address == "" {
			continue
		}
		procs, err := o.callWorkerList(ctx, nd)
		if err != nil {
			continue
		}
		for _, p := range procs {
			if !agent.IsGangProcess(p.ID, entry.ID, gangID) {
				continue
			}
			o.Log.Info("stopping orphan process of this gang from a previous leader",
				"node", nd.ID, "gang", gangID, "process", p.ID, "status", p.Status)
			if err := o.callWorkerStop(ctx, nd, p.ID); err != nil {
				o.Log.Warn("orphan gang process stop failed", "node", nd.ID, "process", p.ID, "err", err)
			}
		}
	}
}

// RemoveGang tears down ONE gang of a model: stops its coordinator and its rpc
// parts, deletes its shard rows, and leaves every other gang of the model
// serving.
//
// Unlike RemoveSharded it does not delete the model row, and it removes the
// model's placement only when the gang it took away was the last one — a model
// with a gang still up is still placed.
func (o *Orchestrator) RemoveGang(ctx context.Context, modelID, gangID string) error {
	if gangID == "" {
		gangID = store.DefaultGangID
	}
	shards, err := o.Store.Shards().GetByGang(ctx, modelID, gangID)
	if err != nil {
		return err
	}
	if len(shards) == 0 {
		return fmt.Errorf("model %s has no gang %q", modelID, gangID)
	}
	o.stopGangProcesses(ctx, shards)
	if err := o.Store.Shards().DeleteByGang(ctx, modelID, gangID); err != nil {
		return err
	}
	// Only the LAST gang takes the placement with it. Removing it while another
	// gang serves would hide a servable model from /v1/models and from every
	// caller that reads a placement as "this model can be served".
	left, err := o.Store.Shards().GangsOf(ctx, modelID)
	if err == nil && len(left) == 0 {
		if err := o.Store.Placements().Delete(ctx, "local", modelID); err != nil {
			o.Log.Warn("placement delete failed", "err", err)
		}
	}
	return nil
}

// stopGangProcesses stops the processes behind a set of shard rows. It stops by
// the ProcessID the row RECORDS, never by a prefix — which is what lets a gang
// created before gang ids existed be torn down by exactly the same path.
func (o *Orchestrator) stopGangProcesses(ctx context.Context, shards []store.Shard) {
	for _, s := range shards {
		switch s.Role {
		case "coordinator":
			if s.NodeID == "" || s.NodeID == "local" {
				if err := o.Supervisor.Stop(s.ProcessID); err != nil {
					o.Log.Warn("coordinator stop failed", "id", s.ID, "err", err)
				}
				continue
			}
			fallthrough
		case "rpc", "rank":
			node, err := o.Store.Nodes().Get(ctx, s.NodeID)
			if err != nil || node == nil {
				o.Log.Warn("shard's node not found", "id", s.ID, "node", s.NodeID)
				continue
			}
			if err := o.callWorkerStop(ctx, *node, s.ProcessID); err != nil {
				o.Log.Warn("shard stop failed", "id", s.ID, "err", err)
			}
		}
	}
}

// NextGangID is the next free gang id for a model: g0, then g1, g2, …
//
// It is for a caller that wants "one more gang" without choosing a name. A
// caller that manages gangs itself — the control plane does, one per its own
// gang record — passes its own id and never uses this.
func (o *Orchestrator) NextGangID(ctx context.Context, modelID string) (string, error) {
	have, err := o.Store.Shards().GangsOf(ctx, modelID)
	if err != nil {
		return "", err
	}
	taken := make(map[string]bool, len(have))
	for _, g := range have {
		taken[g] = true
	}
	for i := 0; i < 1024; i++ {
		id := fmt.Sprintf("g%d", i)
		if !taken[id] {
			return id, nil
		}
	}
	return "", fmt.Errorf("model %s already has 1024 gangs", modelID)
}

// sortGangKeys orders gang keys by model then gang, so a caller's log and a
// test see the same order every run.
func sortGangKeys(keys []store.GangKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Model != keys[j].Model {
			return keys[i].Model < keys[j].Model
		}
		return keys[i].Gang < keys[j].Gang
	})
}

// replacePrior tears down what this create is about to rebuild.
//
// What gets replaced depends on who asked. A bare create still replaces the
// whole model, because that is what it has always meant and a caller who names
// no gang is not thinking in gangs. A create that NAMES a gang replaces only
// that gang — the whole point of gangs is that the others keep serving through
// it. Best-effort either way: a clean slate beats a stale one, and a teardown
// that half-failed must not stop the create that replaces it.
func (o *Orchestrator) replacePrior(ctx context.Context, modelID, gangID string, replaceModel bool) {
	if replaceModel {
		existing, _ := o.Store.Shards().GetByModel(ctx, modelID)
		if len(existing) == 0 {
			return
		}
		o.Log.Info("replacing every gang of model", "model", modelID, "prior_rows", len(existing))
		if err := o.RemoveSharded(ctx, modelID); err != nil {
			o.Log.Warn("prior shard teardown had errors (continuing with create)", "model", modelID, "err", err)
		}
		return
	}
	existing, _ := o.Store.Shards().GetByGang(ctx, modelID, gangID)
	if len(existing) == 0 {
		return
	}
	o.Log.Info("replacing one gang", "model", modelID, "gang", gangID, "prior_rows", len(existing))
	if err := o.RemoveGang(ctx, modelID, gangID); err != nil {
		o.Log.Warn("prior gang teardown had errors (continuing with create)", "model", modelID, "gang", gangID, "err", err)
	}
}

// resolveGang turns the caller's gang argument into the gang to build and
// whether this create replaces the model's OTHER gangs too.
//
// "" means the default gang and a whole-model replace: that is what a create
// has always done, and a caller who names no gang is not thinking in gangs.
func resolveGang(gangID string) (gang string, replaceModel bool, err error) {
	replaceModel = gangID == ""
	if replaceModel {
		gangID = store.DefaultGangID
	}
	return gangID, replaceModel, validGangID(gangID)
}
