package agent

// Engine flags (P12/P13 scenarios): the control plane's plan carries
// `workers[].flags`, a small JSON map the worker pod receives as
// OPOD_ENGINE_FLAGS. Only the keys below are honoured — the rest of an engine's
// command line stays the worker's own decision — so an operator can pin the
// knobs that change capacity or quality without owning the whole launch line.
//
//	vLLM:      tp (tensor-parallel-size; auto = largest power of 2 ≤ GPUs),
//	           gpu_memory_utilization (0.05–0.95; overrides the budget-derived value),
//	           max_model_len, max_num_seqs, kv_cache_dtype, extra (raw args)
//	llama.cpp: ctx (-c), ngl (--n-gpu-layers), parallel (-np), kv_cache_type (-ctk/-ctv), extra (raw args)

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// EngineFlags is the parsed OPOD_ENGINE_FLAGS map; values are strings as the plan writes them.
type EngineFlags map[string]string

// engineFlagsFromEnv parses OPOD_ENGINE_FLAGS; unset or invalid → empty.
func engineFlagsFromEnv() EngineFlags {
	raw := strings.TrimSpace(os.Getenv("OPOD_ENGINE_FLAGS"))
	if raw == "" || raw == "null" {
		return EngineFlags{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return EngineFlags{}
	}
	out := EngineFlags{}
	for k, v := range m {
		switch x := v.(type) {
		case string:
			out[k] = x
		case float64:
			out[k] = strconv.FormatFloat(x, 'f', -1, 64)
		case bool:
			out[k] = strconv.FormatBool(x)
		}
	}
	return out
}

func (f EngineFlags) get(k string) (string, bool) {
	v, ok := f[k]
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

func (f EngineFlags) posInt(k string, min, max int) (int, bool) {
	v, ok := f.get(k)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min || n > max {
		return 0, false
	}
	return n, true
}

// vllmShellOverrides returns shell assignments that override the auto-detected
// TP and utilisation inside the vLLM launch line ("" when nothing is pinned),
// plus the extra arguments appended to `vllm serve`.
func (f EngineFlags) vllmShellOverrides() (overrides string, extraArgs string) {
	var sh []string
	if tp, ok := f.posInt("tp", 1, 64); ok {
		sh = append(sh, fmt.Sprintf("TP=%d;", tp))
	}
	if v, ok := f.get("gpu_memory_utilization"); ok {
		if u, err := strconv.ParseFloat(v, 64); err == nil && u >= 0.05 && u <= 0.95 {
			sh = append(sh, fmt.Sprintf("U=%.2f;", u))
		}
	}
	var args []string
	if n, ok := f.posInt("max_model_len", 512, 1<<22); ok {
		args = append(args, "--max-model-len", strconv.Itoa(n))
	}
	if n, ok := f.posInt("max_num_seqs", 1, 4096); ok {
		args = append(args, "--max-num-seqs", strconv.Itoa(n))
	}
	if v, ok := f.get("kv_cache_dtype"); ok && isToken(v) {
		args = append(args, "--kv-cache-dtype", v)
	}
	if v, ok := f.get("extra"); ok {
		args = append(args, splitExtra(v)...)
	}
	return strings.Join(sh, " "), shellQuoteAll(args)
}

// llamaArgs returns the extra `llama-server` arguments for the pinned knobs.
func (f EngineFlags) llamaArgs() []string {
	var args []string
	if n, ok := f.posInt("ctx", 512, 1<<22); ok {
		args = append(args, "-c", strconv.Itoa(n))
	}
	if n, ok := f.posInt("ngl", 0, 1000); ok {
		args = append(args, "--n-gpu-layers", strconv.Itoa(n))
	}
	if n, ok := f.posInt("parallel", 1, 256); ok {
		args = append(args, "-np", strconv.Itoa(n))
	}
	if v, ok := f.get("kv_cache_type"); ok && isToken(v) {
		args = append(args, "-ctk", v, "-ctv", v)
	}
	if v, ok := f.get("extra"); ok {
		args = append(args, splitExtra(v)...)
	}
	return args
}

// isToken accepts a bare CLI token (no shell metacharacters).
func isToken(s string) bool {
	for _, r := range s {
		if !(r == '-' || r == '_' || r == '.' || r == ':' || r == '/' || r == '=' || r == ',' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return s != ""
}

// splitExtra splits a raw "extra" string into tokens, dropping anything that
// is not a plain CLI token — the flags map never becomes a shell injection.
func splitExtra(s string) []string {
	var out []string
	for _, t := range strings.Fields(s) {
		if isToken(t) {
			out = append(out, t)
		}
	}
	return out
}

func shellQuoteAll(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = "'" + strings.ReplaceAll(a, "'", "'\\''") + "'"
	}
	return strings.Join(q, " ")
}
