package controlplane

// The leader's runtime adapter surface (ROADMAP R15.15).
//
// A manager (the control plane) edits its plan and then wants the adapter set
// on the workers to match, without rolling a pod: an adapter goes into the
// engine's LoRA slots on a worker that already holds the base model, so nothing
// needs restarting (ADR-044 hold mode). The manager cannot do the fan-out
// itself — worker addresses and their HMAC identities are the leader's — so it
// asks here and the leader drives its own workers.
//
// These routes change what the engines hold. They do NOT change the plan file:
// the plan is the manager's document, and a leader that edited it would be
// writing to the thing it is supposed to obey. The manager writes the plan
// first and calls this second; a leader restart re-reads the plan and reloads
// from OPOD_ADAPTERS, so the two agree without the leader ever being the author.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"github.com/opod-io/opod/internal/scheduler"
)

var adminAdapterName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// adaptersLoad — POST /admin/v1/adapters {name, source}. The base is this
// leader's one model identity (D5); asking for another is meaningless here.
func (s *Server) adaptersLoad(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if !adminAdapterName.MatchString(req.Name) {
		writeJSONError(w, http.StatusBadRequest, "adapter name must be lower-case letters, digits, dot, dash or underscore (it becomes part of the model id served as <model>:<name>)")
		return
	}
	if req.Source == "" {
		writeJSONError(w, http.StatusBadRequest, "source is required: a Hugging Face repo id or a path under the node model cache")
		return
	}
	base, ok := s.adapterBase(w)
	if !ok {
		return
	}
	res, err := s.orch.LoadAdapter(r.Context(), base, req.Name, req.Source)
	if err != nil {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	s.writeAdapterResults(w, base, req.Name, res, "loaded")
}

// adaptersUnload — DELETE /admin/v1/adapters/{name}.
func (s *Server) adaptersUnload(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !adminAdapterName.MatchString(name) {
		writeJSONError(w, http.StatusBadRequest, "not an adapter name")
		return
	}
	base, ok := s.adapterBase(w)
	if !ok {
		return
	}
	res, err := s.orch.UnloadAdapter(r.Context(), base, name)
	if err != nil {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	s.writeAdapterResults(w, base, name, res, "unloaded")
}

// adapterBase is the leader's one model identity. Without a plan file there is
// no single identity to attach an adapter to, and guessing one from whatever
// happens to be resident is how an adapter lands on the wrong base.
func (s *Server) adapterBase(w http.ResponseWriter) (string, bool) {
	_, base := s.plan.get()
	if base == "" {
		writeJSONError(w, http.StatusConflict, "this leader has no plan file, so it has no single model identity to attach an adapter to: load it on the worker directly (POST /v1/adapters/load)")
		return "", false
	}
	if s.orch == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no orchestrator: this leader drives no workers")
		return "", false
	}
	return base, true
}

// writeAdapterResults answers per worker. A partial result is a 207, not a 200:
// the model id would answer on some workers and 404 on others, and the caller
// has to see that rather than read "ok" and move on.
func (s *Server) writeAdapterResults(w http.ResponseWriter, base, name string, res []scheduler.AdapterResult, verb string) {
	failed := 0
	for _, r := range res {
		if !r.OK {
			failed++
		}
	}
	code := http.StatusOK
	switch {
	case failed == len(res) && failed > 0:
		code = http.StatusBadGateway
	case failed > 0:
		code = http.StatusMultiStatus
	}
	writeJSON(w, code, map[string]any{
		"model":   base + ":" + name,
		"status":  verb,
		"workers": res,
		"failed":  failed,
	})
}
