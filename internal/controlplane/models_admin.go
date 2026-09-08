package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/lifecycle"
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
	var req AddModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	out, err := s.AddModel(r.Context(), req)
	switch {
	case errors.Is(err, ErrNoCatalogEntry):
		writeJSONError(w, http.StatusNotFound, "no catalog entry for "+req.ID+" (try a scheme-prefixed id like hf:owner/repo, ollama:tag, or file:/path)")
	case errors.Is(err, ErrSourceNotFound):
		writeJSONError(w, http.StatusNotFound, "source for "+req.ID+" does not exist: "+err.Error())
	case errors.Is(err, ErrNoOrchestrator):
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, ErrUpstream):
		writeJSONError(w, http.StatusBadGateway, err.Error())
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "id": out.ID, "kind": out.Kind})
	}
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	out, err := s.DeleteModel(r.Context(), id)
	switch {
	case errors.Is(err, ErrNoOrchestrator):
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "id": out.ID, "kind": out.Kind})
	}
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
	err := s.UnloadModel(r.Context(), id, actorFrom(r))
	switch {
	case errors.Is(err, lifecycle.ErrNotInstalled):
		writeJSONError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, engines.ErrUnloadNotSupported):
		writeJSON(w, http.StatusOK, map[string]string{"status": "noop", "id": id, "reason": s.engine.Name() + " does not support online unload"})
	case errors.Is(err, errEngineUnreachable):
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "unloaded", "id": id})
	}
}
