package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/opod-io/opod/internal/fetch"
)

// opod cache ls|prune — the node's weight cache (ADR-046).
//
// The control plane runs `prune` as a Job on a node, passing the files its
// plans, jobs, prefetches and staged versions still reference. This command
// deletes nothing it did not fetch (a marker beside each file says so), takes
// each file's own download lock so a pull in flight is never pruned
// underneath, and defaults to a dry run: an operator sees what would go before
// anything does. Both commands reach into the revision directories
// (<repo>@<rev>/): a pinned GGUF is a file like any other, a safetensors
// snapshot is one entry that goes whole or stays whole.
func cmdCache(args []string) {
	help := helpSpec{
		name:    "cache",
		summary: "the node's weight cache: list it, or remove what no plan references",
		usage:   "opod cache ls [--dir <models dir>] [--json]\n  opod cache prune [--dir <models dir>] [--keep a.gguf,org-model@rev] [--min-age 24h] [--target-free 50] [--sweep 24h] [--apply] [--json]\n  opod cache kv-prune --dir <disk KV tier dir> --max-gb N [--keep a,b] [--every 10s] [--apply] [--json]",
		examples: []string{
			"opod cache ls --dir /data/models",
			"opod cache prune --dir /data/models                       # dry run: what would go",
			"opod cache prune --dir /data/models --keep org-model@rev --apply",
			"opod cache kv-prune --dir /data/kvtier/ep --max-gb 100 --every 10s --apply   # keep a disk KV tier under 100 GB",
		},
		notes: []string{
			"--dir defaults to $OPOD_MODELS_DIR. prune never removes a file opod did not fetch, and is a dry run without --apply.",
			"kv-prune bounds a disk KV tier (an engine's kv_tier_fs directory), which the engine never deletes from: oldest blocks first, down to 90% of --max-gb, re-read every second while the tier grows; --max-gb 0 empties it but for the --keep entries. It touches only the tier's own files (*.bin, *.tmp, config.json).",
		},
	}
	if len(args) == 0 {
		dieHelp(help)
	}
	if wantsHelp(args) {
		showHelp(help)
	}
	switch args[0] {
	case "ls":
		cacheLs(args[1:])
	case "prune":
		cachePrune(args[1:])
	case "kv-prune":
		cacheKVPrune(args[1:])
	default:
		dieUnknownSubcommand("cache", args[0], []string{"ls", "prune", "kv-prune"})
	}
}

func cacheLs(args []string) {
	fs := flag.NewFlagSet("cache ls", flag.ExitOnError)
	dir := fs.String("dir", os.Getenv("OPOD_MODELS_DIR"), "models directory (default $OPOD_MODELS_DIR)")
	asJSON := fs.Bool("json", false, "JSON output")
	_ = fs.Parse(args)
	entries, err := fetch.List(needDir(*dir))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(entries)
		return
	}
	for _, e := range entries {
		owner := "not ours"
		if e.Ours {
			owner = "opod"
		}
		fmt.Printf("%-40s %8.1f GB  %-8s  last used %s\n", cacheName(e), float64(e.Size)/(1<<30), owner, since(e.LastUsedAt))
	}
}

// cacheName is an entry's name in the tables: a snapshot directory says that
// it is one, and how many files stand behind its one line.
func cacheName(e fetch.Entry) string {
	if e.Snapshot {
		return fmt.Sprintf("%s/ (snapshot, %d files)", e.File, e.Files)
	}
	return e.File
}

func cachePrune(args []string) {
	fs := flag.NewFlagSet("cache prune", flag.ExitOnError)
	dir := fs.String("dir", os.Getenv("OPOD_MODELS_DIR"), "models directory (default $OPOD_MODELS_DIR)")
	keep := fs.String("keep", "", "comma-separated entries that must survive: a file name (kept wherever it is cached), a path under --dir, or a snapshot directory <repo>@<rev>")
	minAge := fs.Duration("min-age", 24*time.Hour, "leave files used more recently than this")
	targetFree := fs.Int("target-free", 0, "stop once this many GB are free (0 = remove every prunable file)")
	apply := fs.Bool("apply", false, "actually delete; without it this is a dry run")
	asJSON := fs.Bool("json", false, "JSON output")
	sweep := fs.Duration("sweep", 24*time.Hour, "also clear crashed pulls older than this (0 = off)")
	_ = fs.Parse(args)

	d := needDir(*dir)
	var keepList []string
	if *keep != "" {
		keepList = strings.Split(*keep, ",")
	}
	res, err := fetch.Prune(fetch.PruneRequest{Dir: d, Keep: keepList, MinAge: *minAge, TargetFreeGb: *targetFree, DryRun: !*apply})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if *sweep > 0 && *apply {
		if n, err := fetch.SweepPartials(d, *sweep); err == nil && n > 0 {
			res.Swept = n
			fmt.Fprintf(os.Stderr, "swept %d leftover(s) of crashed pulls\n", n)
		}
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(res)
		return
	}
	verb := "would remove"
	if *apply {
		verb = "removed"
	}
	for _, e := range res.Removed {
		fmt.Printf("%s %-40s %8.1f GB (last used %s)\n", verb, cacheName(e), float64(e.Size)/(1<<30), since(e.LastUsedAt))
	}
	fmt.Printf("%s %d item(s), %.1f GB; kept %d referenced or recent", verb, len(res.Removed), float64(res.FreedBytes)/(1<<30), res.Kept)
	if len(res.Skipped) > 0 {
		fmt.Printf("; left %d file(s) this cache did not write (%s)", len(res.Skipped), strings.Join(res.Skipped, ", "))
	}
	fmt.Println()
	if !*apply {
		fmt.Println("dry run: pass --apply to delete")
	}
}

// cacheKVPrune bounds a disk KV tier (PLAN T26.9): one pass, or one every
// --every until the process is stopped — the shape a manager runs beside the
// engine that writes the directory, for as long as it writes.
func cacheKVPrune(args []string) {
	fs := flag.NewFlagSet("cache kv-prune", flag.ExitOnError)
	dir := fs.String("dir", "", "the disk KV tier directory (the engine's kv_tier_fs)")
	maxGb := fs.Float64("max-gb", -1, "cap on the tier's files, in GiB; 0 removes all of them but the --keep entries")
	keep := fs.String("keep", "", "comma-separated top-level entries of --dir that are never touched")
	every := fs.Duration("every", 0, "repeat the pass at this idle interval until stopped (0 = once); while the tier grows or is trimmed the next pass follows within 1s")
	apply := fs.Bool("apply", false, "actually delete; without it this is a dry run")
	asJSON := fs.Bool("json", false, "JSON output, one object per pass")
	_ = fs.Parse(args)
	if *dir == "" || *maxGb < 0 {
		fmt.Fprintln(os.Stderr, "error: --dir and --max-gb are required")
		os.Exit(2)
	}
	var keepList []string
	if *keep != "" {
		keepList = strings.Split(*keep, ",")
	}
	req := fetch.KVTierPruneRequest{Dir: *dir, MaxBytes: int64(*maxGb * (1 << 30)), Keep: keepList, DryRun: !*apply}
	show := func(res fetch.KVTierPruneResult) {
		switch {
		case *asJSON:
			_ = json.NewEncoder(os.Stdout).Encode(res)
		case res.Removed > 0 || *every == 0:
			verb := "would remove"
			if *apply {
				verb = "removed"
			}
			fmt.Printf("%s: %.2f GiB in the tier; %s %d file(s), %.2f GiB (walk %d ms, remove %d ms)\n", res.Dir, float64(res.UsedBytes)/(1<<30), verb, res.Removed, float64(res.FreedBytes)/(1<<30), res.WalkMs, res.RemoveMs)
		}
	}
	if *every <= 0 {
		res, err := fetch.PruneKVTier(req)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		show(res)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// A failed pass is said and the next one tries again; a pass that found
	// the tier growing is followed within a second, not a whole --every.
	fetch.RunKVTierPruner(ctx, req, *every, fetch.KVTierHotDelay, func(res fetch.KVTierPruneResult, err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return
		}
		show(res)
	})
}

func needDir(dir string) string {
	if dir == "" {
		fmt.Fprintln(os.Stderr, "error: --dir or OPOD_MODELS_DIR required")
		os.Exit(2)
	}
	return dir
}

func since(t time.Time) string {
	if t.IsZero() {
		return "never recorded"
	}
	return time.Since(t).Round(time.Minute).String() + " ago"
}
