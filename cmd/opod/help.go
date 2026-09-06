package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

// printCmdHelp prints a consistent help block for any subcommand. Each command
// builds one of these and calls printCmdHelp(fs, ...) from its --help branch.
//
// Layout:
//
//	opod <name> — <summary>
//
//	Usage:
//	  <usage>
//
//	Flags:
//	  <auto-printed by fs.PrintDefaults if fs is non-nil>
//
//	Examples:
//	  <each example on its own line>
type helpSpec struct {
	name     string
	summary  string
	usage    string
	flags    *flag.FlagSet
	examples []string
	notes    []string
}

func (h helpSpec) print(w io.Writer) {
	fmt.Fprintf(w, "opod %s — %s\n\n", h.name, h.summary)
	if h.usage != "" {
		fmt.Fprintln(w, "Usage:")
		fmt.Fprintf(w, "  %s\n\n", h.usage)
	}
	if h.flags != nil {
		// Only print if the FlagSet has at least one defined flag.
		hasFlags := false
		h.flags.VisitAll(func(*flag.Flag) { hasFlags = true })
		if hasFlags {
			fmt.Fprintln(w, "Flags:")
			h.flags.SetOutput(w)
			h.flags.PrintDefaults()
			fmt.Fprintln(w)
		}
	}
	if len(h.examples) > 0 {
		fmt.Fprintln(w, "Examples:")
		for _, e := range h.examples {
			fmt.Fprintf(w, "  %s\n", e)
		}
		fmt.Fprintln(w)
	}
	for _, n := range h.notes {
		fmt.Fprintln(w, n)
	}
}

// wantsHelp returns true if args contain -h or --help anywhere, or the
// bare word "help" as the FIRST arg only — `opod model search help`
// must search for "help", not print the help screen.
func wantsHelp(args []string) bool {
	for i, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
		if i == 0 && a == "help" {
			return true
		}
	}
	return false
}

// dieHelp prints help to stderr and exits with code 2 (standard for "you used
// this wrong, here's how to use it right").
func dieHelp(h helpSpec) {
	h.print(os.Stderr)
	os.Exit(2)
}

// showHelp prints help to stdout and exits 0.
func showHelp(h helpSpec) {
	h.print(os.Stdout)
	os.Exit(0)
}

// showUsageErr prints help to stderr WITHOUT exiting. This is the right
// shape for fs.Usage on a flag.ExitOnError FlagSet: on a bad flag, the
// flag package calls Usage and then exits 2 itself — exiting 0 from the
// Usage hook (as showHelp would) would mask the error.
func showUsageErr(h helpSpec) {
	h.print(os.Stderr)
}
