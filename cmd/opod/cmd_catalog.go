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
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: opod catalog ls | export <dir>")
		os.Exit(2)
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
			fmt.Fprintln(os.Stderr, "usage: opod catalog export <dir>")
			os.Exit(2)
		}
		if err := models.ExportBundled(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: opod catalog ls | export <dir>")
		os.Exit(2)
	}
}
