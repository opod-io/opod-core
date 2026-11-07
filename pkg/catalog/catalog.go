// Package catalog is the model catalog opod ships: one YAML per model (the
// files beside this package, embedded) and the schema they follow. The
// leader (opod-core) and the control plane both read this one set — a model
// added here is known to both, and neither carries a second copy (ADR-040
// gap G12, ROADMAP R9.8). Stdlib only: the files are exposed as an embed.FS
// and parsed by the importer with the YAML library it already has.
package catalog

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
)

//go:embed *.yaml
var files embed.FS

// FS is the embedded catalog: every entry as "<id>.yaml" at the root.
var FS fs.FS = files

// Names lists the embedded files, sorted.
func Names() []string {
	des, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil
	}
	var out []string
	for _, de := range des {
		if !de.IsDir() && strings.HasSuffix(de.Name(), ".yaml") {
			out = append(out, de.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Read returns one embedded file by name.
func Read(name string) ([]byte, error) { return fs.ReadFile(files, name) }

// Entry is the schema of one catalog file. Tags are the wire: yaml for the
// files, json for the APIs that echo an entry.
type Entry struct {
	ID            string `yaml:"id"                  json:"id"`
	DisplayName   string `yaml:"display_name"        json:"display_name"`
	Source        Source `yaml:"source"              json:"source"`
	SizeBytes     int64  `yaml:"size_bytes"          json:"size_bytes"`
	Quant         string `yaml:"quant"               json:"quant"`
	ContextWindow int    `yaml:"context_window"      json:"context_window"`
	// TrainedContext is the window the weights were trained for, when
	// ContextWindow is reached only with rope scaling (YaRN) the engine must
	// be told to apply; an engine started without it serves at most this many
	// tokens (llama.cpp caps a slot at the GGUF's n_ctx_train, vLLM at the
	// config's max_position_embeddings). 0 = ContextWindow is the trained one.
	TrainedContext     int      `yaml:"trained_context,omitempty" json:"trained_context,omitempty"`
	Capabilities       []string `yaml:"capabilities"        json:"capabilities"`
	RecommendedEngines []string `yaml:"recommended_engines" json:"recommended_engines"`
	Hardware           Hardware `yaml:"hardware"            json:"hardware"`
	Tags               []string `yaml:"tags"                json:"tags"`
	Sharding           Sharding `yaml:"sharding,omitempty"  json:"sharding,omitempty"`
	// Architecture is what a planner validates a split against (tensor
	// parallel divides the heads, pipeline parallel the layers); zero fields
	// = unknown, which warns and never refuses.
	Architecture Architecture `yaml:"architecture,omitempty" json:"architecture,omitempty"`
	// ToolParser names, per engine id, the parser that engine needs to turn
	// this model's tool-call text into `tool_calls` ({vllm: hermes, sglang:
	// qwen25, trtllm: qwen3}): vLLM, SGLang and TensorRT-LLM return a tool
	// call as plain text unless they are started with one, and each spells
	// the same family differently.
	// Only meaningful on a model with the `tools` capability. An engine with
	// no key = the parser is not known for it, and the engine is started with
	// none; engines that parse tool calls on their own (llama.cpp, Ollama)
	// need no key.
	ToolParser map[string]string `yaml:"tool_parser,omitempty" json:"tool_parser,omitempty"`

	License    string `yaml:"license,omitempty"     json:"license,omitempty"`
	LicenseURL string `yaml:"license_url,omitempty" json:"license_url,omitempty"`
	Released   string `yaml:"released,omitempty"    json:"released,omitempty"`

	// Fallback chains the leader walks when this model fails: generic, and
	// per error class.
	Fallback                []string `yaml:"fallback,omitempty"                   json:"fallback,omitempty"`
	FallbackOnContextLength []string `yaml:"fallback_on_context_length,omitempty" json:"fallback_on_context_length,omitempty"`
	FallbackOnContentPolicy []string `yaml:"fallback_on_content_policy,omitempty" json:"fallback_on_content_policy,omitempty"`
}

// Source says where the weights come from.
type Source struct {
	Type       string `yaml:"type"                  json:"type"` // ollama | huggingface | file
	Repo       string `yaml:"repo,omitempty"        json:"repo,omitempty"`
	File       string `yaml:"file,omitempty"        json:"file,omitempty"` // one file within an HF repo (GGUF)
	OllamaName string `yaml:"ollama_name,omitempty" json:"ollama_name,omitempty"`
	Path       string `yaml:"path,omitempty"        json:"path,omitempty"` // a local path (GGUF / safetensors)
}

// Sharding is the llama.cpp-RPC gang default for an entry that needs one.
type Sharding struct {
	Required        bool   `yaml:"required"         json:"required"`
	DefaultShards   int    `yaml:"default_shards"   json:"default_shards"`
	Engine          string `yaml:"engine"           json:"engine"`
	RPCPortBase     int    `yaml:"rpc_port_base"    json:"rpc_port_base"`
	CoordinatorPort int    `yaml:"coordinator_port" json:"coordinator_port"`
}

// Architecture is the model's shape as far as placement needs it.
type Architecture struct {
	Params  int64 `yaml:"params,omitempty"   json:"params,omitempty"`
	Layers  int   `yaml:"layers,omitempty"   json:"layers,omitempty"`
	Heads   int   `yaml:"heads,omitempty"    json:"heads,omitempty"`
	KVHeads int   `yaml:"kv_heads,omitempty" json:"kv_heads,omitempty"`
	Experts int   `yaml:"experts,omitempty"  json:"experts,omitempty"`
	// HeadDim is the attention head dimension (hidden size ÷ heads). One
	// token of KV cache is 2 × layers × kv_heads × head_dim × bytes per
	// element, so a reader sizing a context window needs it; 0 = not
	// recorded, and a reader assumes 128 (most architectures; Qwen2.5-0.5B's
	// is 64, which made that assumption 1.9× too high).
	HeadDim int `yaml:"head_dim,omitempty" json:"head_dim,omitempty"`

	// The attention layout, when it is not every layer keeping K and V for every
	// token (PLAN T23.9). A reader sizing a KV cache with the formula above
	// would be wrong for each of these, and too high: 0 = not this kind.
	//
	// KVLoRARank and QKRopeHeadDim: multi-head latent attention (MLA,
	// DeepSeek-V2/V3 style). A layer keeps ONE compressed latent per token,
	// kv_lora_rank + qk_rope_head_dim elements, for all of its heads.
	KVLoRARank    int `yaml:"kv_lora_rank,omitempty"     json:"kv_lora_rank,omitempty"`
	QKRopeHeadDim int `yaml:"qk_rope_head_dim,omitempty" json:"qk_rope_head_dim,omitempty"`
	// QKNopeHeadDim and VHeadDim: the per-head K and V an MLA model's attention
	// expands to. Whether an engine keeps the latent or these is the engine's
	// choice: llama.cpp (build 11028) kept K (qk_nope + qk_rope) and V per head
	// for DeepSeek-V2-Lite — 8,640 MiB at 32,768 tokens, where the latent is 972.
	QKNopeHeadDim int `yaml:"qk_nope_head_dim,omitempty" json:"qk_nope_head_dim,omitempty"`
	VHeadDim      int `yaml:"v_head_dim,omitempty"       json:"v_head_dim,omitempty"`
	// SlidingWindow and SlidingLayers: this many layers attend to the last
	// sliding_window tokens only, and keep no more (gpt-oss: 12 of 24 at 128);
	// the other layers keep the whole context.
	SlidingWindow int `yaml:"sliding_window,omitempty" json:"sliding_window,omitempty"`
	SlidingLayers int `yaml:"sliding_layers,omitempty" json:"sliding_layers,omitempty"`
	// StateLayers and StateBytes: hybrid models whose other layers carry a
	// fixed state per sequence instead of a cache per token — SSM, linear
	// attention or short convolution (LFM2: 18 of 24). StateBytes is that state
	// for all of them, one sequence, as the engine keeps it (llama.cpp: f32);
	// 0 = not recorded (small, and not counted).
	StateLayers int   `yaml:"state_layers,omitempty" json:"state_layers,omitempty"`
	StateBytes  int64 `yaml:"state_bytes,omitempty"  json:"state_bytes,omitempty"`
}

// Hardware is the entry's stated minimum.
type Hardware struct {
	MinRAMGB  int `yaml:"min_ram_gb"            json:"min_ram_gb"`
	MinVRAMGB int `yaml:"min_vram_gb,omitempty" json:"min_vram_gb,omitempty"`
}
