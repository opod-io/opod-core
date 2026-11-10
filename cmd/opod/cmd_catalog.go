package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/opod-io/opod-core/internal/models"
)

// opod catalog ls | export <dir> — the bundled catalog is embedded in the
// binary (pkg/catalog, R9.8); export writes it out as files for the
// worker entrypoint and for operators who keep drop-in overrides beside it.
func cmdCatalog(args []string) {
	help := helpSpec{
		name:    "catalog",
		summary: "the bundled model catalog (embedded in the binary)",
		usage:   "opod catalog ls\n  opod catalog export <dir>",
		examples: []string{
			"opod catalog ls                    # one catalog id per line",
			"opod catalog export ./catalog      # write every entry out as <id>.yaml",
		},
		notes: []string{
			"Drop-in overrides go beside the exported files (OPOD_CATALOG_DIR).",
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
		rest, asJSON := extractJSONFlag(args[1:])
		if len(rest) > 0 {
			dieUsage(help, "catalog ls takes only --json (got %q)", rest[0])
		}
		entries, err := models.BundledCatalog()
		if err != nil {
			die("catalog ls: %v", err)
		}
		if asJSON {
			ids := make([]string, len(entries))
			for i, e := range entries {
				ids[i] = e.ID
			}
			emitJSON(ids)
			return
		}
		for _, e := range entries {
			fmt.Println(e.ID)
		}
	case "export":
		switch {
		case len(args) < 2:
			dieUsage(help, "catalog export needs a directory: opod catalog export <dir>")
		case len(args) > 2:
			dieUsage(help, "catalog export takes one directory (got %q)", strings.Join(args[1:], " "))
		}
		if err := models.ExportBundled(args[1]); err != nil {
			die("catalog export %s: %v", args[1], err)
		}
		n := 0
		if entries, err := models.BundledCatalog(); err == nil {
			n = len(entries)
		}
		ok(os.Stdout, "wrote %d catalog entries to %s — overrides go beside them (OPOD_CATALOG_DIR=%s)", n, args[1], args[1])
	default:
		dieUnknownSubcommand("catalog", args[0], []string{"ls", "export"})
	}
}
