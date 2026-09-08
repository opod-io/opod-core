package controlplane

// ModelService + ShardService (E1): install / remove / unload a model and
// create / remove a llama.cpp-RPC gang as plain methods with typed errors
// and a typed outcome; the HTTP handlers only decode, call, and encode.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/scheduler"
	"github.com/opod-io/opod/internal/store"
)

var (
	// ErrNoCatalogEntry: the id is neither a catalog entry nor a scheme id (hf:/ollama:/file:).
	ErrNoCatalogEntry = errors.New("no catalog entry")
	// ErrSourceNotFound: the model's upstream source certainly does not exist.
	ErrSourceNotFound = errors.New("model source does not exist")
	// ErrNoOrchestrator: a sharded or node-pinned placement needs the orchestrator.
	ErrNoOrchestrator = errors.New("sharding orchestrator not configured")
	// ErrUpstream: the engine or a worker refused (mapped to 502).
	ErrUpstream = errors.New("upstream")
)

// AddModelRequest installs a model: by catalog id or scheme id, optionally
// pinned to named workers.
type AddModelRequest struct {
	ID    string   `json:"id"`
	Nodes []string `json:"nodes"`
}

// ModelOutcome says how the model landed.
type ModelOutcome struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // sharded | node | local
}

// AddModel resolves the entry, probes its source (a certain 404 is refused;
// an unverifiable source proceeds with a warning), then installs it:
// sharded through the orchestrator, node-pinned through the workers, or
// pulled by the local engine.
func (s *Server) AddModel(ctx context.Context, req AddModelRequest) (ModelOutcome, error) {
	var entry *models.Entry
	if e, ok := models.ParseSchemeID(req.ID); ok {
		entry = e
	} else {
		entry = models.FindByID(s.cat, req.ID)
	}
	if entry == nil {
		return ModelOutcome{}, ErrNoCatalogEntry
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, models.ProbeTimeout)
	verdict, reason := models.ProbeSource(probeCtx, nil, entry)
	probeCancel()
	switch verdict {
	case models.ProbeNotFound:
		return ModelOutcome{}, errors.Join(ErrSourceNotFound, errors.New(reason))
	case models.ProbeIndeterminate:
		s.log.Warn("could not verify model source — proceeding", "model", entry.ID, "reason", reason)
	}
	if entry.Sharding.Required {
		if s.orch == nil {
			return ModelOutcome{}, ErrNoOrchestrator
		}
		if err := s.orch.CreateSharded(ctx, *entry, 0, nil, scheduler.Parallelism{}); err != nil {
			return ModelOutcome{}, errors.Join(ErrUpstream, err)
		}
		s.router.InvalidateModel(req.ID)
		s.record("model.added", req.ID, map[string]any{"kind": "sharded"})
		s.record("shard.created", req.ID, map[string]any{"default_shards": entry.Sharding.DefaultShards})
		return ModelOutcome{ID: req.ID, Kind: "sharded"}, nil
	}
	if len(req.Nodes) > 0 {
		if s.orch == nil {
			return ModelOutcome{}, ErrNoOrchestrator
		}
		if err := s.orch.PlaceOnNodes(ctx, *entry, req.Nodes, false); err != nil {
			return ModelOutcome{}, errors.Join(ErrUpstream, err)
		}
		// Registered as installed (for `model ls`); placement rows arrive
		// with the workers' heartbeats under their engine-native names.
		_ = s.store.Models().Upsert(ctx, store.Model{ID: entry.ID, CatalogID: entry.ID, Source: "node:" + strings.Join(req.Nodes, ","),
			Status: "ready", SizeBytes: entry.SizeBytes, InstalledAt: time.Now()})
		s.router.InvalidateModel(req.ID)
		s.record("model.added", req.ID, map[string]any{"kind": "node", "nodes": req.Nodes})
		return ModelOutcome{ID: req.ID, Kind: "node"}, nil
	}
	engineName := engineNameFor(s.engine.Name(), entry)
	if err := s.engine.Pull(ctx, engineName, nil); err != nil { // synchronous: may take minutes
		return ModelOutcome{}, errors.Join(ErrUpstream, err)
	}
	_ = s.store.Models().Upsert(ctx, store.Model{ID: entry.ID, CatalogID: entry.ID, Source: s.engine.Name() + ":" + engineName,
		Status: "ready", SizeBytes: entry.SizeBytes, InstalledAt: time.Now()})
	_ = s.store.Placements().Upsert(ctx, store.Placement{NodeID: "local", ModelID: engineName, Status: "ready", LastSeen: time.Now()})
	s.record("model.added", req.ID, map[string]any{"kind": "local"})
	return ModelOutcome{ID: req.ID, Kind: "local"}, nil
}

// engineNameFor is the name the local engine pulls a catalog entry under.
func engineNameFor(engine string, entry *models.Entry) string {
	name := ""
	switch engine {
	case "ollama":
		name = entry.Source.OllamaName
	case "vllm", "mlx", "mlx-lm":
		name = entry.Source.Repo
		if name == "" {
			name = entry.Source.Path
		}
	default: // llama.cpp variants: an HF repo (-hf) or a local path (-m)
		if entry.Source.Repo != "" {
			name = entry.Source.Repo
		} else if entry.Source.Path != "" {
			name = entry.Source.Path
		}
	}
	if name == "" {
		name = entry.ID
	}
	return name
}

// DeleteModel tears a sharded model down through the orchestrator, or
// removes a local model from the engine, its placements and the store.
func (s *Server) DeleteModel(ctx context.Context, id string) (ModelOutcome, error) {
	shards, _ := s.store.Shards().GetByModel(ctx, id)
	if len(shards) > 0 {
		if s.orch == nil {
			return ModelOutcome{}, ErrNoOrchestrator
		}
		if err := s.orch.RemoveSharded(ctx, id); err != nil {
			return ModelOutcome{}, err
		}
		s.router.InvalidateModel(id)
		s.record("model.removed", id, map[string]any{"kind": "sharded"})
		s.record("shard.removed", id, nil)
		return ModelOutcome{ID: id, Kind: "sharded"}, nil
	}
	if m, _ := s.store.Models().Get(ctx, id); m != nil {
		engineName := sourceName(m.Source, id)
		_ = s.engine.Delete(ctx, engineName)
		_ = s.store.Placements().Delete(ctx, "local", engineName)
		_ = s.store.DesiredPlacements().Delete(ctx, "local", id)
	}
	if err := s.store.Models().Delete(ctx, id); err != nil {
		return ModelOutcome{}, err
	}
	s.record("model.removed", id, map[string]any{"kind": "local"})
	return ModelOutcome{ID: id, Kind: "local"}, nil
}

// sourceName strips the "<engine>:" prefix of a stored model source.
func sourceName(source, fallback string) string {
	if idx := strings.IndexByte(source, ':'); idx >= 0 && idx < len(source)-1 {
		return source[idx+1:]
	}
	return fallback
}

// UnloadModel drops a model from engine memory without uninstalling it
// (engines.ErrUnloadNotSupported when the engine cannot — a soft no-op).
// The lifecycle manager owns it when attached.
func (s *Server) UnloadModel(ctx context.Context, id, actor string) error {
	if s.lifecycle != nil {
		if err := s.lifecycle.Unload(ctx, id, actor); err != nil {
			return err
		}
		s.record("model.unloaded", id, map[string]any{"node": "local", "by": actor})
		return nil
	}
	engineName := id
	if m, _ := s.store.Models().Get(ctx, id); m != nil {
		engineName = sourceName(m.Source, id)
	}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second) // a wedged engine fails fast
	defer cancel()
	if err := s.engine.Health(hctx); err != nil {
		return errors.Join(errEngineUnreachable, err)
	}
	if err := s.engine.Unload(hctx, engineName); err != nil {
		return err
	}
	s.record("model.unloaded", id, map[string]any{"node": "local", "by": actor})
	return nil
}

var errEngineUnreachable = errors.New("engine not reachable")

// CreateShardsRequest builds a gang for a catalog model on named workers.
type CreateShardsRequest struct {
	ModelID string   `json:"model_id"`
	Shards  int      `json:"shards"`
	Nodes   []string `json:"nodes"` // optional: pin shards to these exact workers
	TP      int      `json:"tp"`    // optional: tensor-parallel size (vLLM only)
	PP      int      `json:"pp"`    // optional: pipeline-parallel size (vLLM only)
}

// CreateShards builds the gang through the orchestrator. A failed create is
// torn down on a fresh context so no half-shard lingers.
func (s *Server) CreateShards(ctx context.Context, req CreateShardsRequest) error {
	entry := models.FindByID(s.cat, req.ModelID)
	if entry == nil {
		return ErrNoCatalogEntry
	}
	if s.orch == nil {
		return ErrNoOrchestrator
	}
	if err := s.orch.CreateSharded(ctx, *entry, req.Shards, req.Nodes, scheduler.Parallelism{TP: req.TP, PP: req.PP}); err != nil {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		if rmErr := s.orch.RemoveSharded(cleanCtx, req.ModelID); rmErr != nil {
			s.log.Error("shards/create cleanup failed", "model", req.ModelID, "err", rmErr)
		}
		cancel()
		s.router.InvalidateModel(req.ModelID)
		return errors.Join(ErrUpstream, err)
	}
	s.router.InvalidateModel(req.ModelID)
	s.record("shard.created", req.ModelID, map[string]any{"nodes": req.Nodes, "count": req.Shards})
	return nil
}

// RemoveShards tears the gang down.
func (s *Server) RemoveShards(ctx context.Context, modelID string) error {
	if s.orch == nil {
		return ErrNoOrchestrator
	}
	if err := s.orch.RemoveSharded(ctx, modelID); err != nil {
		return err
	}
	s.router.InvalidateModel(modelID)
	s.record("shard.removed", modelID, nil)
	return nil
}

var _ = engines.ErrUnloadNotSupported
