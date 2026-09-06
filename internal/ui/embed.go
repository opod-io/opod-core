// Package ui embeds the web UI assets so the binary ships them statically.
// The UI is a single self-contained HTML file using Tailwind via CDN and
// vanilla JavaScript — no build step required.
package ui

import _ "embed"

//go:embed index.html
var IndexHTML []byte
