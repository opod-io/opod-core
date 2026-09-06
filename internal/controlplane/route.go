package controlplane

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/opod-io/opod/internal/api"
)

// routeDefaults is the curated free → cheap → paid ordering used to seed a
// new chain. Only entries whose provider actually has a key configured are
// included, and the local default model is always appended last as the
// always-available floor. Users reorder freely afterwards.
var routeDefaults = []struct{ vendor, model string }{
	// Free tiers first.
	{"groq", "groq/llama-3.3-70b-versatile"},
	{"cerebras", "cerebras/llama-3.3-70b"},
	{"gemini", "gemini/gemini-2.0-flash"},
	// Cheap.
	{"deepseek", "deepseek/deepseek-chat"},
	{"together", "together/meta-llama/Llama-3.3-70B-Instruct-Turbo"},
	{"mistral", "mistral/mistral-large-latest"},
	{"openrouter", "openrouter/meta-llama/llama-3.3-70b-instruct"},
	// Paid escape hatches.
	{"anthropic", "claude-3-5-haiku-latest"},
	{"openai", "gpt-4o-mini"},
}

// defaultRouteChain builds the seed chain from configured providers + the
// local default model (pinned last).
func defaultRouteChain(pool *api.KeyPool, localDefault string) []string {
	chain := make([]string, 0, len(routeDefaults)+1)
	for _, d := range routeDefaults {
		if pool != nil && pool.HasKeys(d.vendor) {
			chain = append(chain, d.model)
		}
	}
	if localDefault != "" {
		chain = append(chain, localDefault)
	}
	return chain
}

// defaultRoute returns this server's computed default chain.
func (s *Server) defaultRoute() []string {
	var pool *api.KeyPool
	if s.egressH != nil {
		pool = s.egressH.Keys
	}
	return defaultRouteChain(pool, s.cfg.Router.DefaultModel)
}

type routeResponse struct {
	Chain     []string `json:"chain"`
	IsDefault bool     `json:"is_default"` // true when no chain is stored yet
}

// getRoute returns the persisted routing chain, or the computed default when
// none has been set.
func (s *Server) getRoute(w http.ResponseWriter, r *http.Request) {
	chain, err := s.store.Route().Get(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(chain) == 0 {
		writeJSON(w, http.StatusOK, routeResponse{Chain: s.defaultRoute(), IsDefault: true})
		return
	}
	writeJSON(w, http.StatusOK, routeResponse{Chain: chain, IsDefault: false})
}

// setRoute replaces the whole chain. Entries are trimmed, blanks dropped, and
// duplicates collapsed (first position wins) so the stored order is clean.
func (s *Server) setRoute(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req struct {
		Chain []string `json:"chain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	seen := make(map[string]bool, len(req.Chain))
	clean := make([]string, 0, len(req.Chain))
	for _, m := range req.Chain {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		clean = append(clean, m)
	}
	if err := s.store.Route().Set(r.Context(), clean); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, routeResponse{Chain: clean, IsDefault: false})
}

// resetRoute clears the stored chain so the computed default applies again.
func (s *Server) resetRoute(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Route().Set(r.Context(), nil); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, routeResponse{Chain: s.defaultRoute(), IsDefault: true})
}
