package main

// `opod update` — the explicit self-update (ADR-022: no automatic check);
// download, verify and install live in internal/update.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/opod-io/opod/internal/update"
)

// cmdUpdate checks the latest Opod release on GitHub and, unless --check,
// downloads + verifies + installs it in place of the running binary.
//
// Examples:
//
//	opod update                 # check + install latest
//	opod update --check         # just check, don't install
//	opod update --version v0.2  # pin a specific version
//	opod update --force         # reinstall even if up to date
func cmdUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "only check for an update, don't install")
	pinned := fs.String("version", "", "install a specific version (e.g. v0.1.1) instead of latest")
	force := fs.Bool("force", false, "install even if already on the latest version")
	help := helpSpec{
		name:    "update",
		summary: "check for and install the latest Opod release",
		usage:   "opod update [--check] [--version <vX.Y.Z>] [--force]",
		flags:   fs,
		examples: []string{
			"opod update                    # upgrade to latest",
			"opod update --check            # just check, no install",
			"opod update --version v0.1.1   # pin specific version",
			"opod upgrade                   # alias of `update`",
		},
		notes: []string{
			"After installing, restart with `opod down && opod up` if it was running.",
			"If the install path needs sudo (e.g. /usr/local/bin), you'll be told the exact command to run.",
		},
	}
	// Bad flags: print usage to stderr and let ExitOnError exit 2.
	fs.Usage = func() { showUsageErr(help) }
	if wantsHelp(args) {
		showHelp(help)
	}
	_ = fs.Parse(args)

	// 1. Find the current binary so we know where to install over.
	exe, err := os.Executable()
	if err != nil {
		die("could not locate current binary: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	// else: keep the un-resolved path; better than the empty string we'd get
	// from swallowing the error.

	// 2. Resolve the target version.
	target := *pinned
	if target == "" {
		note(os.Stdout, "checking for updates…")
		latest, err := update.LatestVersion()
		if err != nil {
			die("could not fetch latest version: %v", err)
		}
		target = latest
	}

	current := update.NormalizeVersion("v" + version)
	wanted := update.NormalizeVersion(target)
	upToDate := current == wanted

	// 3. Decide what to do based on --check / --force / version compare.
	switch {
	case upToDate && *check && *force:
		note(os.Stdout, "would force-reinstall %s (already on latest)", target)
		return
	case upToDate && *check:
		ok(os.Stdout, "already on the latest version (%s)", target)
		return
	case upToDate && !*force:
		ok(os.Stdout, "already on the latest version (%s)", target)
		return
	case *check:
		note(os.Stdout, "update available: %s → %s", current, target)
		note(os.Stdout, "run: opod update")
		return
	}

	// 4. Download + verify + install.
	note(os.Stdout, "updating: %s → %s", current, target)
	platform := runtime.GOOS + "-" + runtime.GOARCH
	if err := update.Install(target, platform, exe, func(format string, a ...any) { note(os.Stdout, format, a...) }); err != nil {
		die("update failed: %v", err)
	}

	ok(os.Stdout, "installed %s at %s", target, exe)
	fmt.Println()
	fmt.Println("  To use the new version, restart opod:")
	fmt.Println("    opod down")
	fmt.Println("    opod up")
}
