package main

import (
	"fmt"
	"os"

	"github.com/opod-io/opod/internal/models"
)

// opod catalog ls | export <dir> — the bundled catalog is embedded in the
// binary (opod-sdk/catalog, R9.8); export writes it out as files for the
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
		entries, err := models.BundledCatalog()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		for _, e := range entries {
			fmt.Println(e.ID)
		}
	case "export":
		if len(args) < 2 {
			dieHelp(help)
		}
		if err := models.ExportBundled(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	default:
		dieUnknownSubcommand("catalog", args[0], []string{"ls", "export"})
	}
}
