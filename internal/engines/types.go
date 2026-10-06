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

import (
	"context"
	"encoding/json"
)

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
	// ResponseFormat is the caller's `response_format` object, carried
	// verbatim: {"type":"json_object"} or a full
	// {"type":"json_schema","json_schema":{...}}. Empty = the caller asked
	// for nothing, which is plain text.
	//
	// It is raw rather than typed on purpose. The payload is a JSON schema
	// the caller wrote; re-encoding it through a Go struct can only lose
	// fields the next OpenAI revision adds, and every engine behind the
	// OpenAI shape takes the object as it stands. The one driver that does
	// not — Ollama, whose field is `format` — reads the type out of it and
	// translates (PLAN T10.14).
	ResponseFormat json.RawMessage

	// Tools is the caller's `tools` array and ToolChoice their `tool_choice`,
	// both carried verbatim for the same reason as ResponseFormat: the JSON
	// schema of each function is the caller's, and a struct of ours can only
	// lose what it does not model (PLAN T10.8).
	//
	// Opod never EXECUTES a tool. It carries the declaration down and the
	// model's call back up; running the function is the caller's job, as it
	// is with any OpenAI-compatible server.
	Tools      json.RawMessage
	ToolChoice json.RawMessage
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

	// ToolCalls is an ASSISTANT turn's `tool_calls`, carried back in on the
	// next request, and ToolCallID is what a `tool`-role message answers.
	//
	// These are what make tool calling a LOOP rather than one exchange. The
	// standard client appends the assistant message it just received and then
	// a tool result beside it; drop either field and the second request is
	// either rejected by the engine (`tool_call_id` is required on a tool
	// message) or silently missing the context the model needs. Carrying the
	// call out and not back in is single-turn tool calling, which is not what
	// the API means (PLAN T10.8).
	ToolCalls  json.RawMessage
	ToolCallID string
}

// StreamEvent is emitted by Engine.Chat as content arrives.
type StreamEvent struct {
	Delta  string
	Done   bool
	Err    error
	Usage  *Usage
	Reason string // finish reason on the final event

	// ToolCalls is one chunk's `delta.tool_calls` array, verbatim. A model
	// emits a tool call in fragments: the first carries the index, the id and
	// the function name, and the rest carry pieces of the argument string, so
	// a consumer either forwards the fragments as they come (a streaming
	// caller) or merges them by index (ToolCallAccumulator, for a caller that
	// asked for one answer). Empty on an ordinary content event.
	ToolCalls json.RawMessage
}

// Usage is the token accounting for a single completion.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}
