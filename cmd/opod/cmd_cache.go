package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
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
// anything does.
func cmdCache(args []string) {
	if len(args) == 0 {
		cacheUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "ls":
		cacheLs(args[1:])
	case "prune":
		cachePrune(args[1:])
	default:
		cacheUsage()
		os.Exit(2)
	}
}

func cacheUsage() {
	fmt.Fprintln(os.Stderr, "usage: opod cache ls [--dir <models dir>] [--json]")
	fmt.Fprintln(os.Stderr, "       opod cache prune [--dir <models dir>] [--keep a.gguf,b.gguf] [--min-age 24h] [--target-free 50] [--apply] [--json]")
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
		fmt.Printf("%-40s %8.1f GB  %-8s  last used %s\n", e.File, float64(e.Size)/(1<<30), owner, since(e.LastUsedAt))
	}
}

func cachePrune(args []string) {
	fs := flag.NewFlagSet("cache prune", flag.ExitOnError)
	dir := fs.String("dir", os.Getenv("OPOD_MODELS_DIR"), "models directory (default $OPOD_MODELS_DIR)")
	keep := fs.String("keep", "", "comma-separated files that must survive (the control plane's keep set)")
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
		fmt.Printf("%s %-40s %8.1f GB (last used %s)\n", verb, e.File, float64(e.Size)/(1<<30), since(e.LastUsedAt))
	}
	fmt.Printf("%s %d file(s), %.1f GB; kept %d referenced or recent", verb, len(res.Removed), float64(res.FreedBytes)/(1<<30), res.Kept)
	if len(res.Skipped) > 0 {
		fmt.Printf("; left %d file(s) this cache did not write (%s)", len(res.Skipped), strings.Join(res.Skipped, ", "))
	}
	fmt.Println()
	if !*apply {
		fmt.Println("dry run: pass --apply to delete")
	}
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
