// Package vllm is the vLLM driver. vLLM exposes an OpenAI-compatible HTTP
// server when launched with `python -m vllm.entrypoints.openai.api_server`
// or the official Docker image; the driver speaks OpenAI to it through
// openaicompat and adapts to Opod's Engine interface. Registers as "vllm"
// with aliases for Tenstorrent's tt-metal server ("tt-openai", "tenstorrent",
// "tt"), which exposes the same OpenAI API — no TT-specific code needed.
//
// The driver assumes the user (or the worker agent's supervisor) runs vLLM;
// it does not start/stop the process. Example launch (NVIDIA host):
//
//	docker run --gpus all -p 8000:8000 \
//	  vllm/vllm-openai:latest \
//	  --model Qwen/Qwen3-Coder-30B-A3B-Instruct-AWQ \
//	  --tensor-parallel-size 1
package vllm

import (
	"context"
	"net/http"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/engines/openaicompat"
)

const name = "vllm"

func init() {
	engines.Register(engines.Descriptor{
		Name:       name,
		Aliases:    []string{"tt-openai", "tenstorrent", "tt"},
		New:        func(endpoint, apiKey string) engines.Engine { return New(endpoint, apiKey) },
		NativeName: openaicompat.NativeName,
		StartHint:  "start vLLM (see https://docs.vllm.ai/) and ensure OPOD_VLLM_ENDPOINT matches",
	})
}

// Driver is an Engine that proxies to a running vLLM OpenAI-compatible server.
type Driver struct {
	openaicompat.Client
}

// New returns a driver for a vLLM server at endpoint (e.g. http://gpu:8000).
// apiKey is optional — set when vLLM was launched with --api-key.
func New(endpoint, apiKey string) *Driver {
	var auth func(*http.Request)
	if apiKey != "" {
		auth = func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+apiKey) }
	}
	return &Driver{Client: openaicompat.NewClient(name, endpoint, auth)}
}

func (v *Driver) Name() string { return name }

// Pull is a no-op for vLLM: the model is fixed at server-launch time.
// The function returns nil immediately (the configured model is assumed loaded).
// A future version may shell out to `huggingface-cli download` to warm the
// HF cache before the user restarts vLLM with the new model.
func (v *Driver) Pull(ctx context.Context, modelID string, onProgress func(string, int64, int64)) error {
	if onProgress != nil {
		onProgress("vllm: model selection happens at server launch — no pull required", 0, 0)
	}
	return nil
}

// Delete is a no-op (same reasoning as Pull).
func (v *Driver) Delete(ctx context.Context, modelID string) error { return nil }

// Unload is not supported by vLLM — it owns one model per process.
// Restart the vLLM server to free its memory.
func (v *Driver) Unload(ctx context.Context, modelID string) error {
	return engines.ErrUnloadNotSupported
}

var _ engines.Engine = (*Driver)(nil)
