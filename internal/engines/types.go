// Package engines is the engine contract. It holds the Engine interface every
// inference backend driver implements, the engine-agnostic request/stream
// types, the error classes callers branch on (errors.go), the tracing helpers
// drivers share (tracing.go), and the registry drivers register into
// (registry.go).
//
// Drivers live one package each under internal/engines/<name> and register
// themselves in init. internal/engines/all links every driver and is what the
// opod binary imports; a slim build imports only the drivers it ships. Nothing
// outside those packages knows a concrete driver type — construct through
// engines.New / engines.NewWithAuth, exactly like database/sql drivers.
package engines

import "context"

// Engine is implemented by every inference backend driver.
type Engine interface {
	Name() string
	Endpoint() string
	Health(ctx context.Context) error

	List(ctx context.Context) ([]string, error)
	Pull(ctx context.Context, modelID string, onProgress func(status string, completed, total int64)) error
	Delete(ctx context.Context, modelID string) error

	// Unload asks the engine to drop the named model from memory without
	// uninstalling its weights. Engines that can't (vLLM, MLX-LM,
	// llama-server) return ErrUnloadNotSupported and the caller treats
	// it as a soft warning. Used by `opod model unload` and by
	// `opod up --unload-on-exit`.
	Unload(ctx context.Context, modelID string) error

	Chat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error)
}

// ResidentModel is one model currently occupying engine memory, with its
// actual byte footprint as reported by the engine. Distinct from List()
// (which returns *installed* models): residency is what memory-admission
// decisions are made against.
type ResidentModel struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	VRAMBytes int64  `json:"vram_bytes"`
}

// ResidentLister is implemented by engines that can report which models
// are resident in RAM/VRAM right now (Ollama via /api/ps). Engines
// without it fall back to catalog-size estimates in the lifecycle
// manager.
type ResidentLister interface {
	Resident(ctx context.Context) ([]ResidentModel, error)
}

// Loader is implemented by engines that can warm-load a model into
// memory ahead of the first request (Ollama via an empty generate).
// pin asks the engine to exempt the model from its idle eviction TTL
// (Ollama keep_alive=-1). Best-effort: an engine under memory pressure
// may still shuffle pinned models.
type Loader interface {
	Load(ctx context.Context, modelID string, pin bool) error
}

// ChatRequest is the engine-agnostic chat input.
type ChatRequest struct {
	Model       string
	Messages    []Message
	System      string
	Temperature *float32
	TopP        *float32
	MaxTokens   *int
	Stop        []string
	Stream      bool
}

// Message is a single chat turn.
//
// Images, when non-empty, are passed to vision-capable engines alongside
// Content. Each entry is either a base64-encoded image (without the
// "data:image/...;base64," prefix) or an absolute https URL — engines
// negotiate which they prefer.
type Message struct {
	Role    string // system | user | assistant | tool
	Content string
	Images  []string // optional, for vision-capable models (Ollama, vLLM, MLX-LM)
}

// StreamEvent is emitted by Engine.Chat as content arrives.
type StreamEvent struct {
	Delta  string
	Done   bool
	Err    error
	Usage  *Usage
	Reason string // finish reason on the final event
}

// Usage is the token accounting for a single completion.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}
