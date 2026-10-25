package agent

import (
	"strconv"

	"github.com/opod-io/opod-sdk/adminapi"
)

// MemFractionShell is the one rule for turning a worker's VRAM BUDGET into the
// memory fraction an engine is started with, as the shell line that computes it.
//
// Every engine here (vLLM's `--gpu-memory-utilization`, SGLang's
// `--mem-fraction-static`) takes a FRACTION of the device, and the ledger hands
// a worker a budget in GiB. The conversion needs the device's size. A manager
// that measured the card passes it as OPOD_CARD_MIB (feature card_mib), and that
// is what the line reads first; without it the line asks nvidia-smi, and with no
// answer from either the engine's default stands — on a host with no nvidia-smi
// (AMD, Intel) that used to mean the budget was silently never applied. The
// result is clamped to 0.05–0.95: a fraction outside that is a budget that was
// never going to hold a model, and the engine's own error is worse than ours.
//
// ONE TRANSLATION PER ENGINE (memKnobs below), because the engines do not mean
// the same thing by their fraction: vLLM's bounds weights + activations + KV
// cache, SGLang's bounds weights + KV cache only. llama.cpp takes no fraction
// at all (its pool is `-c` tokens, sized by the manager), and TensorRT-LLM's
// `kv_cache_free_gpu_memory_fraction` is a share of what is left AFTER the
// weights — translating a budget into it needs the weights' size, which the
// worker does not have at launch, so it is not translated here and stays the
// plan's own flag.
//
// It lives here, exported, because TWO callers build such a command line and
// they used to carry their own copy of the arithmetic:
//   - the worker's own single-model launches (vllmCmdline, sglangCmdline), and
//   - the LEADER's gang backends (internal/scheduler), which start an engine on
//     a worker over the process contract.
//
// The gang backend was written without it and hard-coded 0.85, which put a gang
// part on an 8 GB card at 85% of the CARD instead of the 6 GB its plan gave it;
// the engine died allocating its attention workspace with 512 KiB free
// (design-partner cell, 2026-09-22). A per-worker VRAM budget that one start
// path ignores is not a budget.
//
// THE BUDGET IS WHAT THE CARD LOSES, not what the engine is told. The fraction
// bounds the engine's own pool; the process also holds a CUDA context and its
// allocator's slack, which the fraction does not count. Measured on a cluster
// (2026-09-29, a 48 GB card, a 10 GB budget): the fraction was 10 GB of the
// card rounded UP to 0.21, the pool took 10.3 GB and the card lost 11.08 GB —
// 8% more than the ledger had promised anyone. So the engine's share is the
// budget LESS that overhead, and the two decimals are cut, never rounded up.
//
// The caller writes the result before its `exec` and passes "$U" wherever the
// engine wants the fraction. An engine with no row gets the line with no
// default and no overhead, which writes nothing a command line reads.
func MemFractionShell(engine string) string {
	k := memKnobs[engine]
	return "U=" + k.defaultFraction + "; B=\"${OPOD_VRAM_BUDGET_GB:-0}\"; " +
		"if [ \"$B\" -gt 0 ] 2>/dev/null; then T=\"${OPOD_CARD_MIB:-}\"; " +
		"case \"$T\" in ''|*[!0-9]*) T=$(nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits 2>/dev/null | head -1 | tr -d ' ');; esac; " +
		"O=\"${" + adminapi.EnvOverheadMiB + ":-}\"; case \"$O\" in ''|*[!0-9]*) O=" + strconv.Itoa(overheadMiB(engine)) + ";; esac; " +
		"[ -n \"$T\" ] && U=$(awk -v b=\"$B\" -v t=\"$T\" -v o=\"$O\" 'BEGIN{u=(b*1024-o)/t; u=int(u*100)/100; if(u>0.95)u=0.95; if(u<0.05)u=0.05; printf \"%.2f\", u}'); fi; "
}

// overheadMiB is the engine's overhead when the manager names none
// (OPOD_ENGINE_OVERHEAD_MIB unset): the SDK table's figure for a vendor not
// measured, which is the engine's largest, else the engine's own row here.
func overheadMiB(engine string) int {
	if mib := adminapi.EngineOverheadMiB(engine, ""); mib > 0 {
		return mib
	}
	return memKnobs[engine].overheadMiB
}

// memKnob is one engine's translation from a budget to its memory fraction.
type memKnob struct {
	// defaultFraction is the fraction with no budget; "" = none, and the flag
	// that carries it disappears (SGLangMemFractionFlag) so the engine sizes
	// its own pool.
	defaultFraction string
	// overheadMiB is what the engine process holds on its card OUTSIDE the
	// pool its fraction bounds, taken off the budget before dividing, for an
	// engine the SDK's EngineOverheadMiB has no figure for.
	overheadMiB int
}

// memKnobs is every engine's translation. vLLM's overhead is measured per
// vendor and lives in the SDK (adminapi.EngineOverheadMiB, which the manager
// passes as OPOD_ENGINE_OVERHEAD_MIB for the card's vendor); SGLang keeps
// 768 MiB, an estimate beside vLLM's pool that no measurement of its own has
// replaced yet (its fraction also leaves out its activations and CUDA-graph
// buffers).
var memKnobs = map[string]memKnob{
	"vllm":   {defaultFraction: "0.85"},
	"sglang": {defaultFraction: "", overheadMiB: 768},
}

// MemFractionVar is what a command line writes where the fraction goes.
const MemFractionVar = `"$U"`

// SGLangMemFractionFlag is where an SGLang command line puts its fraction, and
// it is written only when a budget set one: SGLang's row in memKnobs has no
// default, so with no budget $U is empty and the flag disappears.
//
// SGLang sizes its own pool when it is not told: it reserves room for its
// activations and CUDA graph buffers by card size and parallel width (about
// 3 GB on a card under 20 GB, v0.5.2 `server_args.py`). vLLM has no such rule
// — its default is a constant — so it keeps one. A fixed 0.85 on a WHOLE-CARD
// part replaced SGLang's arithmetic with ours: on an 8 GB card the pool took
// all but 0.6 GB and both ranks of a two-machine gang died allocating the
// attention workspace with 448 KiB free (design-partner cell, 2026-10-16),
// where SGLang alone would have chosen about 0.61.
const SGLangMemFractionFlag = `${U:+--mem-fraction-static "$U"}`

// gpuCountShell counts the devices this worker can see and derives the largest
// power of two at or below that count as $TP — an engine's tensor degree must
// divide the attention head count, which is almost always a power of two, so 5
// cards mean TP=4 rather than a start that dies on "heads (32) must be
// divisible by 5". nvidia-smi first, then /dev/dri, then one.
const gpuCountShell = "N=$(nvidia-smi -L 2>/dev/null | grep -c GPU); " +
	"[ \"$N\" -ge 1 ] || N=$(ls /dev/dri/renderD* 2>/dev/null | wc -l); " +
	"[ \"$N\" -ge 1 ] || N=1; " +
	"TP=1; while [ $((TP*2)) -le \"$N\" ]; do TP=$((TP*2)); done; "
