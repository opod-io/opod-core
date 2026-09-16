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
	// R15.16: a version is a pinned revision plus the digest its record carries.
	// Without --revision the Hub serves "main", which is different bytes after
	// the repo owner pushes.
	rev := fs.String("revision", os.Getenv("OPOD_MODEL_REVISION"), "Hub revision to pin: a commit sha, tag or branch (default $OPOD_MODEL_REVISION; empty = main, which moves)")
	sum := fs.String("sha256", os.Getenv("OPOD_MODEL_SHA256"), "the digest the file must hash to; a mismatch removes it and fails (default $OPOD_MODEL_SHA256)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: opod fetch <hf-repo> <file> [--dir <models dir>] [--revision <sha|tag>] [--sha256 <digest>]")
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
	path, err := fetch.GGUF(context.Background(), pos[0], pos[1], *dir, fetch.Options{Log: log, Token: env.HFToken, Endpoint: env.HFEndpoint,
		Revision: *rev, SHA256: *sum})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println(path)
}
