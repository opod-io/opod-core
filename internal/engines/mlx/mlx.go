// Package mlx is the MLX-LM driver. `mlx_lm.server` exposes an
// OpenAI-compatible HTTP API on Apple Silicon; like vLLM, the driver assumes
// the user runs the server externally and acts as a thin OpenAI client via
// openaicompat. Registers as "mlx" with alias "mlx-lm".
//
// Example launch (Apple Silicon host):
//
//	pip install mlx-lm
//	mlx_lm.server --model mlx-community/Qwen2.5-Coder-14B-Instruct-4bit \
//	              --port 8080
package mlx

import (
	"context"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/engines/openaicompat"
)

const name = "mlx"

func init() {
	engines.Register(engines.Descriptor{
		Name:       name,
		Aliases:    []string{"mlx-lm"},
		New:        func(endpoint, _ string) engines.Engine { return New(endpoint) },
		NativeName: openaicompat.NativeName,
		StartHint:  "start MLX-LM: mlx_lm.server --port 8080",
	})
}

// Driver is an Engine that proxies to a running mlx_lm.server.
type Driver struct {
	openaicompat.Client
}

// New returns a driver for an MLX-LM server at endpoint (e.g. http://localhost:8080).
func New(endpoint string) *Driver {
	return &Driver{Client: openaicompat.NewClient(name, endpoint, nil)}
}

func (m *Driver) Name() string { return name }

// Pull is a no-op (model is selected at mlx_lm.server launch time).
func (m *Driver) Pull(ctx context.Context, modelID string, onProgress func(string, int64, int64)) error {
	if onProgress != nil {
		onProgress("mlx: model selection happens at server launch — no pull required", 0, 0)
	}
	return nil
}

// Delete is a no-op (same reasoning as Pull).
func (m *Driver) Delete(ctx context.Context, modelID string) error { return nil }

// Unload is not supported by mlx_lm.server — restart the daemon to free RAM.
func (m *Driver) Unload(ctx context.Context, modelID string) error {
	return engines.ErrUnloadNotSupported
}

var _ engines.Engine = (*Driver)(nil)
