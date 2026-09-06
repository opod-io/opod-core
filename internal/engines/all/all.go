// Package all links every in-tree engine driver. The opod binary imports it
// for side effects; a slim build imports only the driver packages it ships:
//
//	import _ "github.com/opod-io/opod/internal/engines/all"
//
// Adding a driver = one new package that calls engines.Register in init +
// one line here. Nothing else in the tree changes.
package all

import (
	_ "github.com/opod-io/opod/internal/engines/llamacpp"
	_ "github.com/opod-io/opod/internal/engines/mlx"
	_ "github.com/opod-io/opod/internal/engines/ollama"
	_ "github.com/opod-io/opod/internal/engines/vllm"
)
