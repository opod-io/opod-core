package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/fetch"
)

// opod fetch <repo> <file> [--dir <models dir>] — make one GGUF present in
// the models directory: exclusive per file, atomic (temp + rename, size
// checked), skipped when already there. The same code path the worker uses
// before launching llama-server; a control plane runs it as a prefetch Job
// on a node before any pod of a rollout starts.
func cmdFetch(args []string) {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	dir := fs.String("dir", os.Getenv("OPOD_MODELS_DIR"), "models directory (default $OPOD_MODELS_DIR)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: opod fetch <hf-repo> <file> [--dir <models dir>]")
		fs.PrintDefaults()
	}
	// flags may follow the positionals
	var pos []string
	for len(args) > 0 {
		if len(args[0]) > 0 && args[0][0] == '-' {
			break
		}
		pos, args = append(pos, args[0]), args[1:]
	}
	_ = fs.Parse(args)
	if len(pos) != 2 {
		fs.Usage()
		os.Exit(2)
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "error: --dir or OPOD_MODELS_DIR required")
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	env := config.FromEnv()
	path, err := fetch.GGUF(context.Background(), pos[0], pos[1], *dir, fetch.Options{Log: log, Token: env.HFToken, Endpoint: env.HFEndpoint})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println(path)
}
