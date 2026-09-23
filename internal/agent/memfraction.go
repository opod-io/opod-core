package agent

// MemFractionShell is the one rule for turning a worker's VRAM BUDGET into the
// memory fraction an engine is started with, as the shell line that computes it.
//
// Every engine here (vLLM's `--gpu-memory-utilization`, SGLang's
// `--mem-fraction-static`) takes a FRACTION of the device, and the ledger hands
// a worker a budget in GiB. The conversion needs the device's size, which only
// the machine knows — so it is computed on the machine, at start, from
// nvidia-smi, and falls back to the default when nvidia-smi cannot answer. It
// is clamped to 0.05–0.95: a fraction outside that is a budget that was never
// going to hold a model, and the engine's own error is worse than ours.
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
// The caller writes the result before its `exec` and passes "$U" wherever the
// engine wants the fraction.
func MemFractionShell(defaultFraction string) string {
	return "U=" + defaultFraction + "; B=\"${OPOD_VRAM_BUDGET_GB:-0}\"; " +
		"if [ \"$B\" -gt 0 ] 2>/dev/null; then T=$(nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits 2>/dev/null | head -1 | tr -d ' '); " +
		"[ -n \"$T\" ] && U=$(awk -v b=\"$B\" -v t=\"$T\" 'BEGIN{u=b*1024/t; if(u>0.95)u=0.95; if(u<0.05)u=0.05; printf \"%.2f\", u}'); fi; "
}

// MemFractionVar is what a command line writes where the fraction goes.
const MemFractionVar = `"$U"`

// gpuCountShell counts the devices this worker can see and derives the largest
// power of two at or below that count as $TP — an engine's tensor degree must
// divide the attention head count, which is almost always a power of two, so 5
// cards mean TP=4 rather than a start that dies on "heads (32) must be
// divisible by 5". nvidia-smi first, then /dev/dri, then one.
const gpuCountShell = "N=$(nvidia-smi -L 2>/dev/null | grep -c GPU); " +
	"[ \"$N\" -ge 1 ] || N=$(ls /dev/dri/renderD* 2>/dev/null | wc -l); " +
	"[ \"$N\" -ge 1 ] || N=1; " +
	"TP=1; while [ $((TP*2)) -le \"$N\" ]; do TP=$((TP*2)); done; "
