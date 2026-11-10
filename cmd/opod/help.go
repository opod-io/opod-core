package main

import (
	"errors"
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
	name    string
	summary string
	usage   string
	flags   *flag.FlagSet
	// flagLines describe the flags of a command that parses its arguments by
	// hand (no FlagSet): one line each, "--name VALUE  what it does (default)".
	flagLines []string
	examples  []string
	notes     []string
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
	if h.flags == nil && len(h.flagLines) > 0 {
		fmt.Fprintln(w, "Flags:")
		for _, l := range h.flagLines {
			fmt.Fprintf(w, "  %s\n", l)
		}
		fmt.Fprintln(w)
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

// dieHelp is a command called with too little to act on (no subcommand, no
// argument): the usage error, not the whole help screen.
func dieHelp(h helpSpec) {
	dieUsage(h, "opod %s is missing a subcommand or an argument", h.name)
}

// showHelp prints help to stdout and exits 0.
func showHelp(h helpSpec) {
	h.print(os.Stdout)
	os.Exit(0)
}

// usageExit is the exit code of a command used wrongly: a bad flag, a missing
// or extra argument, an unknown verb (CONTRIBUTING.md "CLI output").
const usageExit = 2

// dieUsage says why the command was used wrongly, its usage line, and where
// every flag is described — three lines, never the whole help — and exits 2.
func dieUsage(h helpSpec, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", ansiFor(os.Stderr, "31", "opod:"), fmt.Sprintf(format, args...))
	if h.usage != "" {
		fmt.Fprintf(os.Stderr, "  usage: %s\n", h.usage)
	}
	fmt.Fprintf(os.Stderr, "  opod %s --help for every flag\n", h.name)
	os.Exit(usageExit)
}

// dieUsageOf is dieUsage for code that knows only the command's name.
func dieUsageOf(name, format string, args ...any) {
	dieUsage(helpSpec{name: name}, format, args...)
}

// parseFlags parses fs the one way every command does: a bad flag or value is
// one `opod:` line and exit 2 — never Go's "Usage of …" and a flag dump —
// and -h/--help prints the flags to stdout and exits 0. name is the command
// whose --help describes them ("cache", "image").
func parseFlags(fs *flag.FlagSet, name string, args []string) {
	fs.Init(fs.Name(), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {} // ContinueOnError still calls Usage; the line below is the message
	err := fs.Parse(args)
	switch {
	case err == nil:
		return
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprintf(os.Stdout, "opod %s — flags:\n", fs.Name())
		fs.SetOutput(os.Stdout)
		fs.PrintDefaults()
		os.Exit(0)
	}
	dieUsageOf(name, "%v", err)
}
