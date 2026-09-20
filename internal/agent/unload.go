package agent

// POST /v1/model/unload — the counterpart of /v1/model/load: make a model
// stop being resident on THIS worker, so that it leaves the next heartbeat
// and the leader's placement row goes with it (feature "worker_unload").
//
// "Unload" is a different act on each engine, which is why the worker owns
// it and the leader only asks:
//
//   - An engine the worker launched for the model (`vllm serve`,
//     `sglang.launch_server`, `llama-server` — one model per worker) is
//     stopped through the supervisor. The process is the model.
//   - An engine that outlives its models and can drop one (Ollama) is asked
//     to: the driver's Unload. The weights stay installed.
//   - An engine that can do neither — a vLLM somebody else started, which
//     this worker only proxies to — is answered 501 "unsupported", in the
//     shape /v1/model/sleep uses. Never a pretended "unloaded".
//
// The call is idempotent: a model that is not resident answers 200 "noop"
// with the reason. It refuses (409) rather than break something it does not
// own: a shard part of the model running here belongs to a gang, and an
// adapter held on the model is a served variant of it — both are removed
// through their own routes first.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/engines"
)

// Unload statuses. "unloaded" and "unsupported" are the words the adapter and
// sleep routes already use.
const (
	statusUnloaded    = "unloaded"
	statusNoop        = "noop"
	statusUnsupported = "unsupported"
)

// unloadModelRequest is the body of /v1/model/unload: the load request's
// source fields without `file` and `pin`, so the worker resolves the
// engine-native name exactly as the load did. Key names are the wire; the
// type becomes the SDK's nodeapi.UnloadModelRequest when core adopts that
// package.
type unloadModelRequest struct {
	ID         string `json:"id"`
	OllamaName string `json:"ollama_name"`
	Repo       string `json:"repo"`
	Path       string `json:"path"`
}

// unloadModelResponse answers it (nodeapi.UnloadModelResponse): "unloaded"
// and "noop" with 200 and the engine-native name, "unsupported" with 501 and
// the engine. Reason says why nothing was done.
type unloadModelResponse struct {
	Status string `json:"status"`
	Model  string `json:"model,omitempty"`
	Engine string `json:"engine,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// launchedEngine is the engine process this worker started for a model: the
// supervisor id it runs under, the id it was loaded as and the engine's own
// name for it. These engines serve exactly one model, so there is at most one.
type launchedEngine struct {
	process string
	id      string
	native  string
}

// noteLaunch records the launch modelLoad just made (the previous one, if
// any, was stopped by that launch).
func (s *Server) noteLaunch(process, id, native string) {
	s.launched.Store(&launchedEngine{process: process, id: id, native: native})
}

func (s *Server) modelUnload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var req unloadModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	name := engines.NativeName(s.Engine.Name(), engines.Source{
		ID: req.ID, OllamaName: req.OllamaName, Repo: req.Repo, Path: req.Path,
	})

	if parts := s.shardParts(req.ID); len(parts) > 0 {
		http.Error(w, fmt.Sprintf("model %q runs here as part of a sharded placement (process %s); a part is not unloaded on its own — remove the sharded placement on the leader first",
			req.ID, strings.Join(parts, ", ")), http.StatusConflict)
		return
	}
	if held := s.heldAdaptersOf(req.ID); len(held) > 0 {
		http.Error(w, fmt.Sprintf("model %q still holds adapter(s) %s on this worker, each served as a variant of it; unload them first (/v1/adapters/unload)",
			req.ID, strings.Join(held, ", ")), http.StatusConflict)
		return
	}

	if l := s.launched.Load(); l != nil {
		s.unloadLaunched(w, l, req.ID, name)
		return
	}
	s.unloadFromEngine(w, r, req.ID, name)
}

// unloadLaunched: this worker started its engine, for exactly one model.
// Unloading that model is stopping the process; any other model is not here.
func (s *Server) unloadLaunched(w http.ResponseWriter, l *launchedEngine, id, name string) {
	if l.id != id {
		writeUnload(w, http.StatusOK, unloadModelResponse{Status: statusNoop, Model: name,
			Reason: fmt.Sprintf("not resident: this worker's engine serves %q", l.id)})
		return
	}
	// Stop only fails when the supervisor no longer knows the process — the
	// state the caller asked for.
	stopErr := s.Supervisor.Stop(l.process)
	s.launched.CompareAndSwap(l, nil)
	s.launchGen.Add(1) // a start-up adapter loader still waiting for this launch gives up
	s.Aliases.Forget(l.native)
	s.forgetAdaptersOf(id)
	s.loraLaunched.Store(nil)
	if stopErr != nil {
		writeUnload(w, http.StatusOK, unloadModelResponse{Status: statusNoop, Model: name,
			Reason: "not resident: the engine process was already gone"})
		return
	}
	writeUnload(w, http.StatusOK, unloadModelResponse{Status: statusUnloaded, Model: name})
}

// unloadFromEngine: the engine is not this worker's process (an Ollama
// daemon, an engine endpoint the worker proxies to). The driver either can
// drop one model or it cannot.
//
// The alias is kept on purpose: the weights stay installed, so "native is
// this worker's copy of id" is still true, and an engine that loads the model
// again must be reported under the id.
func (s *Server) unloadFromEngine(w http.ResponseWriter, r *http.Request, id, name string) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	resident, known := s.residentNow(ctx, id, name)
	notResident := unloadModelResponse{Status: statusNoop, Model: name, Reason: "not resident in this worker's engine"}

	err := s.Engine.Unload(ctx, name)
	switch {
	case errors.Is(err, engines.ErrUnloadNotSupported):
		if known && !resident {
			writeUnload(w, http.StatusOK, notResident)
			return
		}
		writeUnload(w, http.StatusNotImplemented, unloadModelResponse{Status: statusUnsupported, Engine: s.Engine.Name(),
			Reason: "the engine cannot unload a model and this worker did not start it; stop the engine, or the worker, instead"})
	case err != nil:
		http.Error(w, "unload: "+err.Error(), http.StatusBadGateway)
	case known && !resident:
		writeUnload(w, http.StatusOK, notResident)
	default:
		writeUnload(w, http.StatusOK, unloadModelResponse{Status: statusUnloaded, Model: name})
	}
}

// residentNow asks the engine whether it holds the model in memory: its
// resident list where the driver has one, else what it lists. known is false
// when the engine did not answer, and then nothing is concluded from it.
func (s *Server) residentNow(ctx context.Context, id, name string) (resident, known bool) {
	var names []string
	if rl, ok := s.Engine.(engines.ResidentLister); ok {
		models, err := rl.Resident(ctx)
		if err != nil {
			return false, false
		}
		for _, m := range models {
			names = append(names, m.Name)
		}
	} else {
		models, err := s.Engine.List(ctx)
		if err != nil {
			return false, false
		}
		names = models
	}
	for _, n := range names {
		if n == name || n == id || s.Aliases.ID(n) == id {
			return true, true
		}
	}
	return false, true
}

// shardParts lists the supervised processes that are parts of a sharded
// placement of the model (ShardProcessPrefix), sorted.
func (s *Server) shardParts(id string) []string {
	prefix := ShardProcessPrefix(id)
	var out []string
	for _, p := range s.Supervisor.List() {
		if strings.HasPrefix(p.ID, prefix) {
			out = append(out, p.ID)
		}
	}
	sort.Strings(out)
	return out
}

// heldAdaptersOf lists the adapter ids this worker holds on a base, sorted.
func (s *Server) heldAdaptersOf(base string) []string {
	st := s.adapters()
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for id := range st.held {
		if strings.HasPrefix(id, base+":") {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// forgetAdaptersOf drops the records of a base whose engine is gone. An
// unload refuses while any is held, so this only clears what a start-up
// loader added between that check and the stop.
func (s *Server) forgetAdaptersOf(base string) {
	st := s.adapters()
	st.mu.Lock()
	defer st.mu.Unlock()
	for id, a := range st.held {
		if strings.HasPrefix(id, base+":") {
			delete(st.held, id)
			s.Aliases.Forget(a.Name)
		}
	}
}

func writeUnload(w http.ResponseWriter, code int, body unloadModelResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// ShardProcessPrefix is the id prefix of every helper process a leader starts
// on a worker for a sharded placement of the model ("s-<id>-rpc-0",
// "s-<id>-coord", …). The leader builds the ids and the worker recognises
// them, so the rule lives in one place.
//
// It matches EVERY gang of the model. A caller that means one gang wants
// GangProcessPrefix — stopping by this prefix while another gang of the same
// model runs on the node would take that gang's parts down with it.
func ShardProcessPrefix(modelID string) string { return "s-" + SafeProcessID(modelID) + "-" }

// GangProcessPrefix is the id prefix of one gang's helper processes:
// "s-<model>-<gang>-rpc-0", "s-<model>-<gang>-coord", … Several gangs of one
// model may share a worker, so every id a leader mints carries its gang.
func GangProcessPrefix(modelID, gangID string) string {
	return ShardProcessPrefix(modelID) + SafeProcessID(gangID) + "-"
}

// IsGangProcess reports whether procID is a part of this model's gang. Every
// id a leader mints carries its gang, so this is a plain prefix test — and
// validGangID keeps gang ids free of '-' so one gang's prefix can never be a
// prefix of another's.
func IsGangProcess(procID, modelID, gangID string) bool {
	return strings.HasPrefix(procID, GangProcessPrefix(modelID, gangID))
}

// DefaultGangID mirrors store.DefaultGangID. The agent package must not import
// the leader's store, and the two are pinned together by a test.
const DefaultGangID = "g0"

// SafeProcessID folds a model id into what a process id may contain: lower
// case letters, digits and '-'; everything else becomes '-'.
func SafeProcessID(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}
