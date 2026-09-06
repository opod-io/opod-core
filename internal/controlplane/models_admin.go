package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/events"
	"github.com/opod-io/opod/internal/lifecycle"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/scheduler"
	"github.com/opod-io/opod/internal/store"
)

// ---- model admin ----

func (s *Server) listCatalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.cat)
}

// catalogIDForNative maps an engine-native model name a worker reports on
// heartbeat back to the catalog id, so placements key on the id the router
// looks up. Which field of a catalog source is "native" is each driver's
// knowledge (engines.Descriptor.NativeName); the heartbeat does not say which
// engine the worker runs, so every linked driver is consulted. Returns the
// name unchanged when no catalog entry matches (custom/scheme ids).
func (s *Server) catalogIDForNative(name string) string {
	return engines.CatalogID("", name, s.catalogSources())
}

func (s *Server) catalogSources() []engines.Source {
	out := make([]engines.Source, 0, len(s.cat))
	for i := range s.cat {
		e := &s.cat[i]
		out = append(out, engines.Source{ID: e.ID, OllamaName: e.Source.OllamaName, Repo: e.Source.Repo, Path: e.Source.Path})
	}
	return out
}

func (s *Server) addModel(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req struct {
		ID    string   `json:"id"`
		Nodes []string `json:"nodes"` // optional: pin this (non-sharded) model to these exact workers
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	// Scheme-prefixed ids (hf:/ollama:/file:) bypass the catalog so the
	// dashboard "Add custom model" input can install anything the engine
	// supports — same surface as `opod model add hf:owner/repo`.
	var entry *models.Entry
	if e, ok := models.ParseSchemeID(req.ID); ok {
		entry = e
	} else {
		entry = models.FindByID(s.cat, req.ID)
	}
	if entry == nil {
		writeJSONError(w, http.StatusNotFound, "no catalog entry for "+req.ID+" (try a scheme-prefixed id like hf:owner/repo, ollama:tag, or file:/path)")
		return
	}
	// Pre-flight source probe — mirror the CLI: refuse on a certain 404
	// so the dashboard's "Add custom model" input gets a clear error
	// instead of a deferred engine-launch failure; warn-and-proceed when
	// the upstream merely couldn't be verified (network trouble).
	{
		probeCtx, probeCancel := context.WithTimeout(r.Context(), models.ProbeTimeout)
		verdict, reason := models.ProbeSource(probeCtx, nil, entry)
		probeCancel()
		switch verdict {
		case models.ProbeNotFound:
			writeJSONError(w, http.StatusNotFound, "source for "+entry.ID+" does not exist: "+reason)
			return
		case models.ProbeIndeterminate:
			s.log.Warn("could not verify model source — proceeding", "model", entry.ID, "reason", reason)
		}
	}
	// Sharded models delegate to the orchestrator.
	if entry.Sharding.Required {
		if s.orch == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "sharding orchestrator not configured")
			return
		}
		if err := s.orch.CreateSharded(r.Context(), *entry, 0, nil, scheduler.Parallelism{}); err != nil {
			writeJSONError(w, http.StatusBadGateway, err.Error())
			return
		}
		s.router.InvalidateModel(req.ID)
		s.bus.Publish(events.Event{Topic: events.TopicModels, ID: req.ID})
		s.bus.Publish(events.Event{Topic: events.TopicShards, ID: req.ID})
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "id": req.ID, "kind": "sharded"})
		return
	}
	// Node-pinned placement: load this model on specific workers (their engines
	// pull + load it, then report it on heartbeat → the leader reconciles the
	// placement). No leader-local pull happens for this path.
	if len(req.Nodes) > 0 {
		if s.orch == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "placement orchestrator not configured")
			return
		}
		if err := s.orch.PlaceOnNodes(r.Context(), *entry, req.Nodes, false); err != nil {
			writeJSONError(w, http.StatusBadGateway, err.Error())
			return
		}
		// Register the model as installed (for `model ls`); placement rows are
		// written by heartbeat reconciliation, keyed to the worker's own
		// engine-native name.
		_ = s.store.Models().Upsert(r.Context(), store.Model{
			ID: entry.ID, CatalogID: entry.ID,
			Source: "node:" + strings.Join(req.Nodes, ","),
			Status: "ready", SizeBytes: entry.SizeBytes,
			InstalledAt: time.Now(),
		})
		s.router.InvalidateModel(req.ID)
		s.bus.Publish(events.Event{Topic: events.TopicModels, ID: req.ID})
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "id": req.ID, "kind": "node"})
		return
	}
	// Non-sharded: pull via the local engine.
	engineName := ""
	switch s.engine.Name() {
	case "ollama":
		engineName = entry.Source.OllamaName
	case "vllm", "mlx", "mlx-lm":
		engineName = entry.Source.Repo
		if engineName == "" {
			engineName = entry.Source.Path
		}
	default:
		// llamacpp variants accept either an HF repo (-hf) or a local path (-m).
		if entry.Source.Repo != "" {
			engineName = entry.Source.Repo
		} else if entry.Source.Path != "" {
			engineName = entry.Source.Path
		}
	}
	if engineName == "" {
		engineName = entry.ID
	}
	// Synchronous pull — may take minutes. Future: stream progress via SSE.
	if err := s.engine.Pull(r.Context(), engineName, nil); err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	_ = s.store.Models().Upsert(r.Context(), store.Model{
		ID: entry.ID, CatalogID: entry.ID,
		Source: s.engine.Name() + ":" + engineName,
		Status: "ready", SizeBytes: entry.SizeBytes,
		InstalledAt: time.Now(),
	})
	_ = s.store.Placements().Upsert(r.Context(), store.Placement{
		NodeID: "local", ModelID: engineName, Status: "ready", LastSeen: time.Now(),
	})
	s.bus.Publish(events.Event{Topic: events.TopicModels, ID: req.ID})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "id": req.ID, "kind": "local"})
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	// Sharded model? Tear down via the orchestrator.
	shards, _ := s.store.Shards().GetByModel(r.Context(), id)
	if len(shards) > 0 {
		if s.orch == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "sharding orchestrator not configured")
			return
		}
		if err := s.orch.RemoveSharded(r.Context(), id); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.router.InvalidateModel(id)
		s.bus.Publish(events.Event{Topic: events.TopicModels, ID: id})
		s.bus.Publish(events.Event{Topic: events.TopicShards, ID: id})
		writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "id": id, "kind": "sharded"})
		return
	}
	// Non-sharded: delete from store + engine.
	m, _ := s.store.Models().Get(r.Context(), id)
	if m != nil {
		engineName := id
		if idx := indexByte(m.Source, ':'); idx >= 0 && idx < len(m.Source)-1 {
			engineName = m.Source[idx+1:]
		}
		_ = s.engine.Delete(r.Context(), engineName)
		_ = s.store.Placements().Delete(r.Context(), "local", engineName)
		_ = s.store.DesiredPlacements().Delete(r.Context(), "local", id)
	}
	if err := s.store.Models().Delete(r.Context(), id); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.bus.Publish(events.Event{Topic: events.TopicModels, ID: id})
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "id": id, "kind": "local"})
}

// eventsStream serves Server-Sent Events to dashboard subscribers. Each
// connection gets its own buffered channel from the bus; producers
// elsewhere in the server (model add/remove, node heartbeat, usage
// record) publish topic strings that we encode as one-line SSE messages.
// The handler also sends a 25 s heartbeat ping so reverse proxies don't
// idle the connection out.
func (s *Server) eventsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, cancel := s.bus.Subscribe(32)
	defer cancel()

	// Greet so the EventSource client knows the stream is alive even
	// before the first state change.
	_, _ = fmt.Fprintf(w, "event: hello\ndata: {\"ok\":true}\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			// SSE comment line — clients ignore it, but proxies see traffic.
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			payload, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Topic, payload)
			flusher.Flush()
		}
	}
}

// unloadModel asks the engine to drop the model from RAM without
// deleting weights from disk. Mirrors `opod model unload`.
//
// With the lifecycle manager attached (the normal `opod up` path) the
// unload also drains in-flight requests first and clears the model's
// desired-placement row so it stays unloaded across restarts. The
// legacy direct-engine path below remains for embedded uses without a
// manager.
func (s *Server) unloadModel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if s.lifecycle != nil {
		err := s.lifecycle.Unload(r.Context(), id, actorFrom(r))
		switch {
		case errors.Is(err, lifecycle.ErrNotInstalled):
			writeJSONError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, engines.ErrUnloadNotSupported):
			writeJSON(w, http.StatusOK, map[string]string{
				"status": "noop", "id": id,
				"reason": s.engine.Name() + " does not support online unload",
			})
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		default:
			s.bus.Publish(events.Event{Topic: events.TopicModels, ID: id})
			writeJSON(w, http.StatusOK, map[string]string{"status": "unloaded", "id": id})
		}
		return
	}
	m, _ := s.store.Models().Get(r.Context(), id)
	engineName := id
	if m != nil {
		if idx := indexByte(m.Source, ':'); idx >= 0 && idx < len(m.Source)-1 {
			engineName = m.Source[idx+1:]
		}
	}
	// Bounded context: a wedged-but-listening engine should fail fast
	// rather than tying up the admin connection.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.engine.Health(ctx); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable,
			"engine not reachable: "+err.Error())
		return
	}
	if err := s.engine.Unload(ctx, engineName); err != nil {
		if errors.Is(err, engines.ErrUnloadNotSupported) {
			writeJSON(w, http.StatusOK, map[string]string{
				"status": "noop",
				"id":     id,
				"reason": s.engine.Name() + " does not support online unload",
			})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.bus.Publish(events.Event{Topic: events.TopicModels, ID: id})
	writeJSON(w, http.StatusOK, map[string]string{"status": "unloaded", "id": id})
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
