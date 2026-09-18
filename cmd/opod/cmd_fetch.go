package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/fetch"
)

// opod fetch <repo> <file> [--dir <models dir>] — make one GGUF present in
// the models directory: exclusive per file, atomic (temp + rename, size
// checked), skipped when already there. The same code path the worker uses
// before launching llama-server; a control plane runs it as a prefetch Job
// on a node before any pod of a rollout starts.
//
// opod fetch --snapshot <repo>[@rev] [--dir <models dir>] — the same for a
// safetensors model, which is a directory rather than a file: the weight
// shards, configs and tokenizer files an engine loads, under
// <dir>/<repo>@<rev>/. A vLLM or SGLang worker that finds a complete snapshot
// of its model there serves from it instead of pulling at launch.
func cmdFetch(args []string) {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	dir := fs.String("dir", os.Getenv("OPOD_MODELS_DIR"), "models directory (default $OPOD_MODELS_DIR)")
	// R15.16: a version is a pinned revision plus the digest its record carries.
	// Without --revision the Hub serves "main", which is different bytes after
	// the repo owner pushes.
	rev := fs.String("revision", os.Getenv("OPOD_MODEL_REVISION"), "Hub revision to pin: a commit sha, tag or branch (default $OPOD_MODEL_REVISION; empty = main, which moves)")
	sum := fs.String("sha256", os.Getenv("OPOD_MODEL_SHA256"), "the digest the file must hash to; a mismatch removes it and fails (default $OPOD_MODEL_SHA256; not with --snapshot)")
	snap := fs.String("snapshot", "", "fetch the file set a safetensors engine loads from `<hf-repo>[@revision]` instead of one file")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: opod fetch <hf-repo> <file> [--dir <models dir>] [--revision <sha|tag>] [--sha256 <digest>]")
		fmt.Fprintln(os.Stderr, "       opod fetch --snapshot <hf-repo>[@<sha|tag>] [--dir <models dir>]")
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
	pos = append(pos, fs.Args()...)
	if (*snap == "" && len(pos) != 2) || (*snap != "" && len(pos) != 0) {
		fs.Usage()
		os.Exit(2)
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "error: --dir or OPOD_MODELS_DIR required")
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	env := config.FromEnv()
	opt := fetch.Options{Log: log, Token: env.HFToken, Endpoint: env.HFEndpoint, Revision: *rev, SHA256: *sum}
	var path string
	var err error
	if *snap != "" {
		typed := map[string]bool{} // flags given on the command line, as opposed to inherited from the environment
		fs.Visit(func(f *flag.Flag) { typed[f.Name] = true })
		if !typed["sha256"] {
			opt.SHA256 = "" // $OPOD_MODEL_SHA256 describes one file; only a typed --sha256 is a mistake worth refusing
		}
		var repo string
		if repo, opt.Revision, err = snapshotTarget(*snap, *rev, typed["revision"]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(2)
		}
		path, err = fetch.Snapshot(context.Background(), repo, *dir, opt)
	} else {
		path, err = fetch.GGUF(context.Background(), pos[0], pos[1], *dir, opt)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println(path)
}

// snapshotTarget splits `<repo>[@rev]`. A revision written on the argument is
// the explicit one: it wins over $OPOD_MODEL_REVISION, and disagreeing with an
// explicit --revision is an error rather than a guess.
func snapshotTarget(arg, revision string, revisionFlagSet bool) (repo, rev string, err error) {
	repo, at, pinned := strings.Cut(arg, "@")
	switch {
	case repo == "" || (pinned && at == ""):
		return "", "", fmt.Errorf("--snapshot wants <hf-repo>[@revision], got %q", arg)
	case !pinned:
		return repo, revision, nil
	case revisionFlagSet && revision != at:
		return "", "", fmt.Errorf("--snapshot names revision %q and --revision names %q: say it once", at, revision)
	}
	return repo, at, nil
}
