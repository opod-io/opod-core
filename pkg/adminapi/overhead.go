package adminapi

// EngineOverheadMiB is what an engine process holds on its card outside the
// pool its memory fraction bounds (CUDA or HIP context, allocator slack), in
// MiB, per engine and vendor: the one table a manager sizing a budget and a
// worker turning that budget into the engine's fraction both read (PLAN T23.3).
// The figures are measured: vLLM held 837 MiB beyond its pool on an NVIDIA
// card and about 1,103 MiB on an AMD Radeon. llama.cpp has no fraction — its
// figure is the fixed part it holds beyond weights, KV cache AND its compute
// buffer (CUDA context and library workspace), because the compute buffer
// depends on the shape and the manager adds it (PLAN T23.18): flat at 157–160
// MiB on an RTX 3060 Ti for 0.5B to 7B models, and ≈ 570 on an RTX 5090 (628
// MiB beyond weights and KV at 72 slots of 32,768 tokens, less its ≈ 66 MiB
// compute buffer), and ≈ 365 on an AMD Radeon RX 7900 XTX (402 MiB beyond weights
// and KV at 1 slot of 8,192 tokens and 430 at 60 slots of 32,768, less the
// 0.5B compute buffer and mask). The fixed part depends on the card, so CardOverheadMiB
// holds it per card and this table carries the largest. A vendor not measured
// yet takes the engine's largest figure, so a budget is never overrun by a
// guess. 0 = no figure for this engine; the caller keeps its own.
func EngineOverheadMiB(engine, vendor string) int {
	byVendor, ok := engineOverheadMiB[engine]
	if !ok {
		return 0
	}
	if mib, ok := byVendor[vendor]; ok {
		return mib
	}
	largest := 0
	for _, mib := range byVendor {
		largest = max(largest, mib)
	}
	return largest
}

var engineOverheadMiB = map[string]map[string]int{
	"vllm":     {"nvidia": 837, "amd": 1103},
	"llamacpp": {"nvidia": 570},
}

// CardOverheadMiB is EngineOverheadMiB on one card model, named as the vendor's
// tool names it (nvidia-smi's "NVIDIA GeForce RTX 3060 Ti"), where the engine's
// figure was measured on that card; else EngineOverheadMiB(engine, vendor). A
// card not measured, or not known yet, takes the larger figure, so a budget
// sized for it holds on any card.
func CardOverheadMiB(engine, vendor, card string) int {
	if mib, ok := cardOverheadMiB[engine][card]; ok {
		return mib
	}
	return EngineOverheadMiB(engine, vendor)
}

var cardOverheadMiB = map[string]map[string]int{
	"llamacpp": {
		"NVIDIA GeForce RTX 3060 Ti": 160,
		"NVIDIA GeForce RTX 5090":    570,
		"AMD Radeon RX 7900 XTX":     365,
	},
}
