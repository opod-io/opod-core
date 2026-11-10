package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/opod-io/opod-core/internal/config"
	"github.com/opod-io/opod-core/internal/fetch"
	"github.com/opod-io/opod-core/pkg/adminapi"
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
	logFormat := fs.String("log-format", "text", "text: one human line per event on stderr; json: structured slog lines for a log collector")
	if wantsHelp(args) {
		showHelp(helpSpec{
			name:    "fetch",
			summary: "make one GGUF file, or a safetensors model's file set, present in a models directory",
			usage:   "opod fetch <hf-repo> <file> [--dir D] [--revision R] [--sha256 S]  |  opod fetch --snapshot <hf-repo>[@<rev>] [--dir D]",
			flags:   fs,
			examples: []string{
				"opod fetch bartowski/Llama-3.2-3B-Instruct-GGUF Llama-3.2-3B-Instruct-Q4_K_M.gguf --dir /data/models",
				"opod fetch --snapshot Qwen/Qwen2.5-1.5B-Instruct@main --dir /data/models",
				"HF_TOKEN=hf_… opod fetch meta-llama/Llama-3.1-8B-Instruct --snapshot … # a gated repository",
			},
			notes: []string{
				"The path is the only line on stdout; progress, `already cached` and every warning are on stderr.",
				"HF_ENDPOINT points at a Hub mirror (default https://huggingface.co); HF_TOKEN reads gated or private repositories.",
			},
		})
	}
	// flags may follow the positionals
	var pos []string
	for len(args) > 0 {
		if len(args[0]) > 0 && args[0][0] == '-' {
			break
		}
		pos, args = append(pos, args[0]), args[1:]
	}
	parseFlags(fs, "fetch", args)
	pos = append(pos, fs.Args()...)
	usage := helpSpec{name: "fetch", usage: "opod fetch <hf-repo> <file> [--dir D] [--revision R] [--sha256 S]  |  opod fetch --snapshot <hf-repo>[@<rev>] [--dir D]"}
	switch {
	case *snap == "" && len(pos) != 2:
		dieUsage(usage, "fetch takes a repo and a file (got %d argument(s)), or --snapshot <repo>", len(pos))
	case *snap != "" && len(pos) != 0:
		dieUsage(usage, "--snapshot takes no other argument (got %q)", strings.Join(pos, " "))
	}
	if *dir == "" {
		dieUsage(usage, "no models directory: pass --dir or set OPOD_MODELS_DIR")
	}
	var log *slog.Logger
	switch *logFormat {
	case "text":
		log = slog.New(newHumanHandler(os.Stderr))
	case "json":
		log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	default:
		dieUsage(usage, "--log-format %q: text or json", *logFormat)
	}
	env := config.FromEnv()
	opt := fetch.Options{Log: log, Token: env.HFToken, Endpoint: env.HFEndpoint, Revision: *rev, SHA256: *sum,
		Progress: func(done, total int64) {
			if total > 0 {
				log.Info("still fetching", "done", fmt.Sprintf("%.1f of %.1f GiB (%d%%)", float64(done)/(1<<30), float64(total)/(1<<30), done*100/total))
				return
			}
			log.Info("still fetching", "done", fmt.Sprintf("%.1f GiB", float64(done)/(1<<30)))
		}}
	// Ctrl-C, or the SIGTERM Kubernetes sends a prefetch Job's pod it deletes,
	// cancels the pull instead of killing the process mid-write: the lock is
	// released and the partial is kept, so the next attempt resumes at once.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
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
			dieUsageOf("fetch", "%v", err)
		}
		path, err = fetch.Snapshot(ctx, repo, *dir, opt)
	} else {
		path, err = fetch.GGUF(ctx, pos[0], pos[1], *dir, opt)
	}
	if err != nil {
		// No answer at all is the Hub's address, not the model: say which
		// address and where it came from.
		var ue *url.Error
		var he *fetch.HubError
		if errors.As(err, &ue) && !errors.As(err, &he) && ctx.Err() == nil {
			hub, from := env.HFEndpoint, "HF_ENDPOINT"
			if hub == "" {
				hub, from = fetch.DefaultHFEndpoint, "the default; HF_ENDPOINT names a mirror"
			}
			die("fetch: cannot reach the Hub at %s (%s): %v — check the address and the network, or unset HF_ENDPOINT to use huggingface.co", hub, from, ue.Err)
		}
		msg := err.Error()
		if !strings.HasPrefix(msg, "fetch:") {
			msg = "fetch: " + msg // the package names itself on most of its errors; say it once
		}
		die("%s", msg)
	}
	if env.FetchResult != "" {
		var repo string
		if *snap != "" {
			repo, _, _ = strings.Cut(*snap, "@")
		} else {
			repo = pos[0]
		}
		writeFetchResult(env.FetchResult, path, fetch.SignatureOf(ctx, repo, opt), log)
	}
	fmt.Println(path)
}

// writeFetchResult leaves the fetch's result where its manager asked
// ($OPOD_FETCH_RESULT, T32.23): one JSON line, written only once the weights
// are in place. Failing to write it does not fail the fetch — the weights are
// there — and the manager, finding no result, reports the signature unknown.
func writeFetchResult(dst, path string, sig adminapi.FetchSignature, log *slog.Logger) {
	line, err := json.Marshal(adminapi.FetchResult{Path: path, Signature: sig})
	if err == nil {
		err = os.WriteFile(dst, append(line, '\n'), 0o644)
	}
	if err != nil {
		log.Warn(fmt.Sprintf("could not write the result file %s: %v — the weights are at %s", dst, err, path))
	}
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
