package main

// `opod update` — the explicit self-update (ADR-022: no automatic check);
// download, verify and install live in internal/update.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/opod-io/opod-core/internal/update"
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
	force := fs.Bool("force", false, "install even when the target is this version or older than this build (a deliberate downgrade)")
	yes := fs.Bool("yes", false, "replace this binary without asking (needed without a terminal)")
	help := helpSpec{
		name:    "update",
		summary: "check for and install the latest Opod release",
		usage:   "opod update [--check] [--version <vX.Y.Z>] [--force] [--yes]",
		flags:   fs,
		examples: []string{
			"opod update                    # upgrade to latest",
			"opod update --check            # just check, no install",
			"opod update --version v0.1.1   # pin specific version",
			"opod upgrade                   # alias of `update`",
		},
		notes: []string{
			"--check exits 10 when an update is available, 0 when there is none — a script can branch on it.",
			"A target older than this build is refused unless --force: a downgrade is never an accident.",
			"After installing, restart with `opod down && opod up` if it was running.",
			"If the install path needs sudo (e.g. /usr/local/bin), you'll be told the exact command to run.",
		},
	}
	// Bad flags: print usage to stderr and let ExitOnError exit 2.
	if wantsHelp(args) {
		showHelp(help)
	}
	parseFlags(fs, "update", args)

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

	current := "v" + strings.TrimPrefix(version, "v")
	switch compareBuild(version, target) {
	case buildDev:
		switch {
		case *check && *pinned != "":
			note(os.Stdout, "this is a development build (%s); opod update --version %s would install that release over it", version, target)
			return
		case *check:
			note(os.Stdout, "this is a development build (%s); the latest release is %s", version, target)
			return
		case *pinned == "" && !*force:
			die("this is a development build (%s), not a release — replace it with %s: opod update --force", version, target)
		}
	case buildSame:
		if !*force {
			ok(os.Stdout, "already on %s — nothing to install", target)
			return
		}
		if *check {
			note(os.Stdout, "already on %s; --force would reinstall it", target)
			return
		}
	case buildOlder:
		if !*force {
			die("%s is older than this build (%s) — to downgrade on purpose: opod update --version %s --force", target, current, target)
		}
		if *check {
			note(os.Stdout, "%s is older than this build (%s); --force would downgrade", target, current)
			return
		}
	case buildNewer:
		if *check {
			note(os.Stdout, "update available: %s → %s — install it with: opod update", current, target)
			os.Exit(updateAvailableExit)
		}
	}
	if !*yes {
		verb := "update"
		if *pinned != "" {
			verb += " --version " + *pinned
		}
		confirmOrExit(fmt.Sprintf("Replace %s (%s) with %s? (y/N) ", exe, current, target), verb)
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

// updateAvailableExit is `opod update --check`'s exit code when a newer
// release exists, so a script can tell "update" from "none" (0).
const updateAvailableExit = 10

type buildCompare int

const (
	buildSame  buildCompare = iota
	buildNewer              // the target is newer than this build
	buildOlder              // the target is older: a downgrade
	buildDev                // this build is not a release (a -dev version, or a commit)
)

// compareBuild orders the release target against this binary's version by
// semver, never as text: "v0.21.0-dev" and "v0.4.5" compared as strings read
// the older release as an update and installed it without a word.
func compareBuild(current, target string) buildCompare {
	cur, okCur := update.ParseVersion(current)
	want, okWant := update.ParseVersion(target)
	if !okCur || strings.Contains(cur.Pre, "dev") || !okWant {
		return buildDev
	}
	switch update.Compare(want, cur) {
	case 1:
		return buildNewer
	case -1:
		return buildOlder
	}
	return buildSame
}
