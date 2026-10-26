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
//	llama.cpp: ctx (-c), ngl (--n-gpu-layers), parallel (-np), kv_cache_type (-ctk/-ctv),
//	           kv_unified (true → --kv-unified, false → --no-kv-unified),
//	           tensor_split (--tensor-split: uneven shares per device), extra (raw args)
//	SGLang:    tp, mem_fraction_static, max_model_len (--context-length), max_running_requests,
//	           kv_cache_dtype (--kv-cache-dtype), extra (raw args)
//	TensorRT-LLM: tp (--tp_size), max_num_seqs (--max_batch_size, always set), max_model_len
//	           (--max_seq_len), kv_cache_free_gpu_memory_fraction, kv_cache_dtype (merged into the
//	           --config file as kv_cache_config.dtype: no flag exists), extra (raw args)
//	vLLM/SGLang: pp_layer_partition (VLLM_/SGLANG_PP_LAYER_PARTITION: layers per pipeline stage —
//	           the only way either engine holds a model across cards of different sizes)
//	vLLM:      kv_tier_fs (an absolute directory: a disk tier below the host
//	           tier, PLAN T26.9). With the host tier on the line (`extra` carries
//	           --kv-offloading-size N, backend native), the launch adds ONE
//	           --kv-transfer-config JSON — OffloadingConnector, spec
//	           TieringOffloadingSpec, the host tier as its CPU primary
//	           (cpu_bytes_to_use = N GiB) and secondary_tiers [{type: fs,
//	           root_dir: <dir>}] — and exports PYTHONHASHSEED (0 unless the
//	           environment sets one), so every process sharing the directory
//	           names one prefix with one file. Ignored without a host tier (the
//	           fs tier is fed only from it), with another backend, or when
//	           `extra` already carries a --kv-transfer-config of its own: a list
//	           of objects cannot ride `extra`, which keeps plain tokens only.
//	           Nothing in the engine deletes from the directory; whoever names
//	           it bounds it (`opod cache kv-prune`).
//	vLLM/SGLang/TensorRT-LLM: no flag for the tool-call parser — the model's catalog entry names it
//	           (tool_parser) and the launch adds it; an `extra` that already carries the engine's
//	           parser flag (--tool-call-parser, TensorRT-LLM --tool_parser) wins and the catalog's is
//	           not added (toolparser.go)

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// EngineFlags is the parsed OPOD_ENGINE_FLAGS map; values are strings as the plan writes them.
type EngineFlags map[string]string

// ParseEngineFlags parses the OPOD_ENGINE_FLAGS value (config.Env.EngineFlags);
// unset or invalid → empty.
func ParseEngineFlags(raw string) EngineFlags {
	raw = strings.TrimSpace(raw)
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

// allGPULayers is llama.cpp's idiom for "put every layer you can on the GPU".
// The engine places what fits and leaves the rest on the CPU.
const allGPULayers = 999

// llamaOffloadArgs decides how much of the model llama.cpp puts on the GPU.
//
// llama.cpp keeps every layer on the CPU unless it is told otherwise, so a
// worker holding a card has to ask for the offload or the card it reserved does
// nothing. A plan that pins `ngl` always wins: an operator asking for a partial
// offload knows something we do not. Otherwise an accelerated worker offloads
// everything and lets the engine place what fits, and a CPU worker says nothing
// at all so it keeps running on the CPU.
func (f EngineFlags) llamaOffloadArgs(accelerated bool) []string {
	if _, pinned := f.get("ngl"); pinned || !accelerated {
		return nil
	}
	return []string{"--n-gpu-layers", strconv.Itoa(allGPULayers)}
}

func (f EngineFlags) get(k string) (string, bool) {
	v, ok := f[k]
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

func (f EngineFlags) posInt(k string, lo, hi int) (int, bool) {
	v, ok := f.get(k)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
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
	if v, ok := f.get("pp_layer_partition"); ok && isSplitList(v) {
		// vLLM reads the per-stage layer counts from the environment, not from
		// a flag (VLLM_PP_LAYER_PARTITION="8,12,12,8"). Pipeline stages are the
		// only way it can hold a model across cards of different sizes: tensor
		// parallel gives every rank the same share, so the smallest card bounds
		// the rest.
		sh = append(sh, fmt.Sprintf("export VLLM_PP_LAYER_PARTITION=%s;", v))
	}
	var args []string
	if n, ok := f.posInt("max_model_len", 512, 1<<22); ok {
		args = append(args, "--max-model-len", strconv.Itoa(n))
	}
	if v, ok := f.get("kv_cache_dtype"); ok && isToken(v) {
		args = append(args, "--kv-cache-dtype", v)
	}
	// --max-num-seqs is ALWAYS on the line (ADR-091): it is the slot count the
	// worker reports and the leader governs dispatch by, so the engine and the
	// leader must agree on it by construction. vLLM's own default depends on
	// the release and the card; ours is stated and fixed.
	if _, inExtra := f.extraInt("--max-num-seqs"); !inExtra {
		args = append(args, "--max-num-seqs", strconv.Itoa(f.vllmMaxNumSeqs()))
	}
	if v, ok := f.get("extra"); ok {
		args = append(args, splitExtra(v)...)
	}
	if cfg, ok := f.vllmKVTierFS(); ok {
		sh = append(sh, kvTierFSSeed)
		args = append(args, "--kv-transfer-config", cfg)
	}
	return strings.Join(sh, " "), shellQuoteAll(args)
}

// kvTierFSSeed fixes the root of vLLM's block-hash chain (NONE_HASH): unseeded,
// every process starts it from random bytes, so two processes sharing one fs
// tier directory — replicas, or a worker restarted onto its own files — name
// one prefix with two file names and never hit each other's blocks. An
// environment that sets one keeps it.
const kvTierFSSeed = `export PYTHONHASHSEED="${PYTHONHASHSEED:-0}";`

// vllmKVTierFS is the --kv-transfer-config value of a disk tier (kv_tier_fs),
// or ok=false when the key is unset or cannot apply. vLLM's
// TieringOffloadingSpec (read on the pinned 0.27.1) is the CPU tier plus
// secondary tiers; --kv-offloading-size still names the CPU tier's size, and
// vLLM merges it into this config (config/vllm.py
// _post_init_kv_transfer_config updates kv_connector_extra_config), so this is
// the same one connector the host tier already is, never a second. The size is
// written here as well so the JSON says the whole tier on its own.
func (f EngineFlags) vllmKVTierFS() (string, bool) {
	dir, ok := f.get("kv_tier_fs")
	if !ok || !isTierDir(dir) {
		return "", false
	}
	gib, sized := f.extraInt("--kv-offloading-size")
	if !sized || gib <= 0 {
		return "", false // no CPU primary: the fs tier is fed only from it
	}
	if b := f.extraValue("--kv-offloading-backend"); b != "" && b != "native" {
		return "", false
	}
	if f.extraHasPrefix("--kv-transfer-config") {
		return "", false // a connector of the plan's own: one connector, never two
	}
	type tier struct {
		Type    string `json:"type"`
		RootDir string `json:"root_dir"`
	}
	type extra struct {
		SpecName       string `json:"spec_name"`
		CPUBytesToUse  int64  `json:"cpu_bytes_to_use"`
		SecondaryTiers []tier `json:"secondary_tiers"`
	}
	type config struct {
		Connector string `json:"kv_connector"`
		Role      string `json:"kv_role"`
		Extra     extra  `json:"kv_connector_extra_config"`
	}
	b, err := json.Marshal(config{Connector: "OffloadingConnector", Role: "kv_both", Extra: extra{
		SpecName: "TieringOffloadingSpec", CPUBytesToUse: int64(gib) << 30,
		SecondaryTiers: []tier{{Type: "fs", RootDir: dir}},
	}})
	if err != nil {
		return "", false
	}
	return string(b), true
}

// isTierDir accepts an absolute directory made of plain path characters, with
// no `..` element: it lands in a JSON argument and names where the engine
// writes, so nothing relative or escaping is taken.
func isTierDir(s string) bool {
	if !strings.HasPrefix(s, "/") || len(s) > 512 || !isToken(s) || strings.ContainsAny(s, "=,:") {
		return false
	}
	for _, el := range strings.Split(s, "/") {
		if el == ".." {
			return false
		}
	}
	return true
}

// extraValue is the value of a flag in the plan's raw `extra` ("--flag v" or
// "--flag=v"), the last one written; "" when absent.
func (f EngineFlags) extraValue(name string) string {
	v, _ := f.get("extra")
	toks := splitExtra(v)
	out := ""
	for i, t := range toks {
		switch {
		case t == name && i+1 < len(toks):
			out = toks[i+1]
		case strings.HasPrefix(t, name+"="):
			out = strings.TrimPrefix(t, name+"=")
		}
	}
	return out
}

// extraHasPrefix reports whether any token of the plan's raw `extra` starts
// with prefix (a flag in any of its spellings, dotted keys included).
func (f EngineFlags) extraHasPrefix(prefix string) bool {
	v, _ := f.get("extra")
	for _, t := range splitExtra(v) {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}

// VLLMShellOverrides is vllmShellOverrides for a caller outside the agent: the
// scheduler's expert-parallel gang (PLAN T18.5), whose ranks are `vllm serve`
// processes the leader launches — so a gang's max_model_len, utilisation or
// extra args reach every rank the way a worker's do. The `TP=` assignment it
// may carry is inert there: the gang writes its tensor width itself.
func (f EngineFlags) VLLMShellOverrides() (overrides string, extraArgs string) {
	return f.vllmShellOverrides()
}

// LlamaServerArgs is llamaArgs for a caller outside the agent: the
// scheduler's gang coordinator, which is a llama-server too and took no flags
// at all until PLAN T14.26 — so `-c` and `--tensor-split` could never reach a
// gang, a small card could not join one, and the refusal it got was an OOM
// crash loop rather than a plan-time error.
func (f EngineFlags) LlamaServerArgs() []string { return f.llamaArgs() }

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
	// kv_unified: one KV pool every slot shares, or a fixed partition per slot
	// (-c ÷ -np each). A build picks one when it is not told — unified only
	// when the slot count is automatic, as of the build we ship — so the
	// manager says which, and the layout no longer changes with the build.
	// With the slot count left automatic the build shares one pool whatever
	// it is told (llamaKVUnified).
	switch v, _ := f.get("kv_unified"); strings.ToLower(v) {
	case "true":
		args = append(args, "--kv-unified")
	case "false":
		args = append(args, "--no-kv-unified")
	}
	// tensor_split: how much of the model each device holds, as llama.cpp's own
	// proportions ("12,8" = three fifths on the first card, two on the second).
	// Without it the split is equal, which wastes every card that is larger
	// than the smallest — the reason a 16 GB and an 8 GB card cannot hold a
	// model that would fit across them.
	if v, ok := f.get("tensor_split"); ok && isSplitList(v) {
		args = append(args, "--tensor-split", v)
	}
	if v, ok := f.get("extra"); ok {
		args = append(args, splitExtra(v)...)
	}
	return args
}

// llamaKVUnified is the KV layout the launch line gives llama-server: true =
// one pool every slot shares, false = a partition per slot. The engine does
// not report it back, and its pool cannot be read without it (the llama.cpp
// driver's SetKVUnified, PLAN T23.2).
//
// A slot count left automatic decides it alone: the build then sets four
// slots and one shared pool together, whatever the line says — on the
// design-partner cell (2026-10-22) a gang coordinator started with
// --no-kv-unified and no -np logged `kv_unified = 'true'`. With a slot count
// stated, the last word wins, as on llama-server's own command line: `extra`
// (written last), then the kv_unified flag, else a partition per slot.
// known = false when the line caps each slot with --kv-unified-per-slot: the
// slot context /props reports is then that cap, not the pool, and neither
// layout's arithmetic reads the pool from it.
func (f EngineFlags) llamaKVUnified() (unified, known bool) {
	v, hasExtra := f.get("extra")
	toks := strings.Fields(v)
	for _, t := range toks {
		if t == "--kv-unified-per-slot" || strings.HasPrefix(t, "--kv-unified-per-slot=") {
			return false, false
		}
	}
	if f.LaunchSlots("llamacpp") == 0 {
		return true, true
	}
	if hasExtra {
		for i := len(toks) - 1; i >= 0; i-- {
			switch toks[i] {
			case "--kv-unified", "-kvu":
				return true, true
			case "--no-kv-unified", "-no-kvu":
				return false, true
			}
		}
	}
	switch v, _ := f.get("kv_unified"); strings.ToLower(v) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, true
}

// LlamaKVUnified is llamaKVUnified for a caller outside the agent: the
// scheduler records a gang coordinator's layout on its row, so the leader that
// samples the coordinator can read its pool (PLAN T23.13).
func (f EngineFlags) LlamaKVUnified() (unified, known bool) { return f.llamaKVUnified() }

// isSplitList accepts a comma-separated list of non-negative numbers and
// nothing else — "8,12,12,8", "12,8", "0.6,0.4". It is stricter than isToken
// on purpose: these values go into a shell export and a CLI argument, and a
// split is never anything but numbers and commas.
func isSplitList(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	digits := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits = true
		case r == ',' || r == '.':
		default:
			return false
		}
	}
	return digits
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

// llamaModelSource picks how llama-server finds the weights: a cached file
// path wins; a Hugging Face repo needs the GGUF file name when the repo holds
// several quantisations (--hf-file); a bare name only when nothing else is
// known (llama-server then expects <user>/<model>[:quant]).
func llamaModelSource(nativeName, repo, file, path string) []string {
	switch {
	case path != "":
		return []string{"-m", path}
	case repo != "" && file != "":
		return []string{"-hf", repo, "--hf-file", file}
	case repo != "":
		return []string{"-hf", repo}
	default:
		return []string{"-hf", nativeName}
	}
}

// sglangShellOverrides is vllmShellOverrides' counterpart for SGLang: the same
// three questions — how many cards, how much of each, how long a context — in
// SGLang's own spelling. TP and the memory fraction override the auto-detected
// values inside the launch line; the rest are appended as arguments.
func (f EngineFlags) sglangShellOverrides() (overrides string, extraArgs string) {
	var sh []string
	if tp, ok := f.posInt("tp", 1, 64); ok {
		sh = append(sh, fmt.Sprintf("TP=%d;", tp))
	}
	if v, ok := f.get("mem_fraction_static"); ok {
		if u, err := strconv.ParseFloat(v, 64); err == nil && u >= 0.05 && u <= 0.95 {
			sh = append(sh, fmt.Sprintf("U=%.2f;", u))
		}
	}
	if v, ok := f.get("pp_layer_partition"); ok && isSplitList(v) {
		// SGLANG_PP_LAYER_PARTITION, the same idea as vLLM's: upstream advises
		// putting the larger share on the LATER stages, which reduces the time
		// a stage spends waiting for the one before it.
		sh = append(sh, fmt.Sprintf("export SGLANG_PP_LAYER_PARTITION=%s;", v))
	}
	var args []string
	if n, ok := f.posInt("max_model_len", 512, 1<<22); ok {
		args = append(args, "--context-length", strconv.Itoa(n))
	}
	if n, ok := f.posInt("max_running_requests", 1, 4096); ok {
		args = append(args, "--max-running-requests", strconv.Itoa(n))
	}
	// kv_cache_dtype: the KV cache's element type (fp8_e5m2, fp8_e4m3 …), the
	// same key vLLM takes — an 8-bit cache holds twice the tokens. SGLang had
	// no way to take it from a plan (PLAN T23.10).
	if v, ok := f.get("kv_cache_dtype"); ok && isToken(v) {
		args = append(args, "--kv-cache-dtype", v)
	}
	if v, ok := f.get("extra"); ok {
		args = append(args, splitExtra(v)...)
	}
	return strings.Join(sh, " "), shellQuoteAll(args)
}

// trtllmShellOverrides is sglangShellOverrides for trtllm-serve (T19.15): tp
// into $TP, and the rest as trtllm-serve's own flag names. --max_batch_size is
// ALWAYS on the line, as --max-num-seqs is for vLLM (ADR-091): it is the slot
// count the worker reports, and TensorRT-LLM's own default is far above what
// the leader should dispatch to one card. config, when set, is the one
// --config file the engine reads: it replaces any the plan's extra names
// (trtllm-serve takes one, and the worker's merges the plan's into it).
func (f EngineFlags) trtllmShellOverrides(config string) (overrides string, extraArgs string) {
	var sh []string
	if tp, ok := f.posInt("tp", 1, 64); ok {
		sh = append(sh, fmt.Sprintf("TP=%d;", tp))
	}
	var args []string
	if _, inExtra := f.extraInt("--max_batch_size"); !inExtra {
		args = append(args, "--max_batch_size", strconv.Itoa(f.trtllmMaxBatch()))
	}
	if n, ok := f.posInt("max_model_len", 512, 1<<22); ok {
		args = append(args, "--max_seq_len", strconv.Itoa(n))
	}
	if v, ok := f.get("kv_cache_free_gpu_memory_fraction"); ok {
		if u, err := strconv.ParseFloat(v, 64); err == nil && u >= 0.05 && u <= 0.95 {
			args = append(args, "--kv_cache_free_gpu_memory_fraction", strconv.FormatFloat(u, 'f', 2, 64))
		}
	}
	if v, ok := f.get("extra"); ok {
		extra := splitExtra(v)
		for i := 0; i < len(extra); i++ {
			if t := extra[i]; config != "" && isTRTLLMConfigFlag(t) {
				if !strings.Contains(t, "=") {
					i++ // and its value
				}
				continue
			}
			args = append(args, extra[i])
		}
	}
	if config != "" {
		args = append(args, "--config", config)
	}
	return strings.Join(sh, " "), shellQuoteAll(args)
}

// isTRTLLMConfigFlag reports whether t names trtllm-serve's options file, in
// either of its spellings, as `--flag v` or `--flag=v`.
func isTRTLLMConfigFlag(t string) bool {
	name, _, _ := strings.Cut(t, "=")
	return name == "--config" || name == "--extra_llm_api_options"
}

// trtllmConfigPath is the options file the plan's extra names, or "".
func trtllmConfigPath(extra string) string {
	f := splitExtra(extra)
	for i, t := range f {
		if !isTRTLLMConfigFlag(t) {
			continue
		}
		if _, v, ok := strings.Cut(t, "="); ok {
			return v
		}
		if i+1 < len(f) {
			return f[i+1]
		}
	}
	return ""
}

// DefaultTRTLLMMaxBatch is the --max_batch_size a TensorRT-LLM worker starts
// with when the plan pins none: the same slot count a small card's vLLM worker
// is usually run at, stated rather than left to the engine's 2048.
const DefaultTRTLLMMaxBatch = 64

func (f EngineFlags) trtllmMaxBatch() int {
	if n, ok := f.posInt("max_num_seqs", 1, 4096); ok {
		return n
	}
	return DefaultTRTLLMMaxBatch
}

// DefaultVLLMMaxNumSeqs is the --max-num-seqs a vLLM worker starts with when
// the plan pins none (ADR-091): vLLM's own default on cards below 70 GiB, the
// cards most fleets run. vLLM picks 1024 by itself on larger cards; a plan that
// wants that says max_num_seqs: 1024. Pinned rather than left to vLLM so the
// slot count the worker reports is the one the engine enforces.
const DefaultVLLMMaxNumSeqs = 256

// vllmMaxNumSeqs is the plan's max_num_seqs, else the default.
func (f EngineFlags) vllmMaxNumSeqs() int {
	if n, ok := f.posInt("max_num_seqs", 1, 4096); ok {
		return n
	}
	return DefaultVLLMMaxNumSeqs
}

// extraInt reads a numeric flag out of the plan's raw `extra` arguments, in
// either spelling ("--flag N" or "--flag=N"). ok = the flag is there at all,
// even with a value that is not a number — the engine will refuse that itself.
func (f EngineFlags) extraInt(names ...string) (n int, ok bool) {
	v, has := f.get("extra")
	if !has {
		return 0, false
	}
	toks := strings.Fields(v)
	for i, t := range toks {
		for _, name := range names {
			val := ""
			switch {
			case t == name:
				ok = true
				if i+1 < len(toks) {
					val = toks[i+1]
				}
			case strings.HasPrefix(t, name+"="):
				ok = true
				val = strings.TrimPrefix(t, name+"=")
			default:
				continue
			}
			if x, err := strconv.Atoi(val); err == nil && x > 0 {
				n = x
			}
		}
	}
	return n, ok
}

// LaunchSlots is how many requests an engine this worker launches is told to
// serve at once (ADR-091), by engine id: the flag on the launch line, or 0
// when the line carries none and only the engine itself can say (llama.cpp's
// -np default, SGLang's KV-derived limit — both drivers read the engine's own
// number, engines.SlotReporter, which wins over this one).
func (f EngineFlags) LaunchSlots(engine string) int {
	switch engine {
	case "vllm":
		if n, ok := f.extraInt("--max-num-seqs"); ok {
			return n
		}
		return f.vllmMaxNumSeqs()
	case "sglang":
		if n, ok := f.extraInt("--max-running-requests"); ok {
			return n
		}
		n, _ := f.posInt("max_running_requests", 1, 4096)
		return n
	case "trtllm":
		if n, ok := f.extraInt("--max_batch_size"); ok {
			return n
		}
		return f.trtllmMaxBatch()
	case "llamacpp":
		if n, ok := f.extraInt("-np", "--parallel"); ok {
			return n
		}
		n, _ := f.posInt("parallel", 1, 256)
		return n
	}
	return 0
}
