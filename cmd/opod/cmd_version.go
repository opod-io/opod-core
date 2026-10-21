package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/opod-io/opod/internal/leader"
)

func cmdVersion(args []string) {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	features := fs.Bool("features", false, "print the leader's feature keys, comma-separated and sorted (the list /admin/v1/capabilities advertises)")
	if wantsHelp(args) {
		showHelp(helpSpec{
			name:    "version",
			summary: "print the opod binary version + build info",
			usage:   "opod version [--features]",
			flags:   fs,
			notes: []string{
				"The image build stamps the --features list on the leader image as the",
				"io.opod.leader.features label, so a manager can read what a leader speaks from its image.",
			},
		})
	}
	_ = fs.Parse(args)
	if *features {
		printFeatures(os.Stdout)
		return
	}
	fmt.Printf("opod %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// printFeatures writes the leader's advertised feature keys as one line: the
// value of the leader image's io.opod.leader.features label.
func printFeatures(w io.Writer) {
	fmt.Fprintln(w, strings.Join(leader.FeatureKeys(), ","))
}
