package agent

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
)

// Sampling defaults (PLAN T16.9, feature pd_handoff): the model's own
// generation_config.json, which vLLM applies to every request that does not
// set a field itself. The leader asks before it splits a request across a
// prefill/decode pair, because vLLM's decode answers differently after a
// remote prefill whenever a repetition penalty is on (vLLM #63693) — and for
// a request that names none, the penalty in force is this file's.
//
// Read from the directory the engine loads (modelSource, the same lookup its
// launcher uses), so it is the file of the bytes being served. A model the
// engine pulls by repo name has no directory here, and the answer is "not
// known": the leader then does not split, which is always correct.

// SamplingDefaults is the worker's answer. Known is false when the worker
// cannot read the served model's configuration; RepetitionPenalty is then 0.
type SamplingDefaults struct {
	Known             bool    `json:"known"`
	RepetitionPenalty float64 `json:"repetition_penalty,omitempty"`
}

// samplingDefaultsOf reads the served model's defaults. A directory with no
// generation_config.json is known: the engine's own default applies, which
// is no penalty.
func (s *Server) samplingDefaultsOf(model string) SamplingDefaults {
	native := model
	if s.Aliases != nil {
		native = s.Aliases.Native(model)
	}
	dir := s.modelSource(native)
	if !filepath.IsAbs(dir) {
		return SamplingDefaults{}
	}
	b, err := os.ReadFile(filepath.Join(dir, "generation_config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return SamplingDefaults{Known: true, RepetitionPenalty: 1}
	}
	if err != nil {
		return SamplingDefaults{}
	}
	var gc struct {
		RepetitionPenalty *float64 `json:"repetition_penalty"`
	}
	if json.Unmarshal(b, &gc) != nil {
		return SamplingDefaults{}
	}
	if gc.RepetitionPenalty == nil {
		return SamplingDefaults{Known: true, RepetitionPenalty: 1}
	}
	return SamplingDefaults{Known: true, RepetitionPenalty: *gc.RepetitionPenalty}
}

// samplingDefaults serves GET /v1/sampling-defaults?model=<id>.
func (s *Server) samplingDefaults(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	model := r.URL.Query().Get("model")
	if model == "" {
		http.Error(w, "model required", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.samplingDefaultsOf(model))
}
