package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// humanHandler is the slog handler a person reads (CONTRIBUTING.md "CLI
// output", rule 6): one line per event, `▶ message (key=value, …)`, a
// warning marked ⚠, no timestamp or level column. A log collector asks for
// structured lines instead (`--log-format json`).
type humanHandler struct {
	mu    *sync.Mutex
	w     io.Writer
	attrs []slog.Attr
}

func newHumanHandler(w io.Writer) *humanHandler { return &humanHandler{mu: &sync.Mutex{}, w: w} }

func (h *humanHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }

func (h *humanHandler) Handle(_ context.Context, r slog.Record) error {
	var kv []string
	add := func(a slog.Attr) bool {
		kv = append(kv, fmt.Sprintf("%s=%v", a.Key, a.Value.Any()))
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(add)
	mark := ansiFor(h.w, "1;34", "▶")
	if r.Level >= slog.LevelWarn {
		mark = ansiFor(h.w, "1;33", "⚠")
	}
	line := mark + " " + r.Message
	if len(kv) > 0 {
		line += " (" + strings.Join(kv, ", ") + ")"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := fmt.Fprintln(h.w, line)
	return err
}

func (h *humanHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &humanHandler{mu: h.mu, w: h.w, attrs: append(append([]slog.Attr{}, h.attrs...), as...)}
}

func (h *humanHandler) WithGroup(string) slog.Handler { return h }
