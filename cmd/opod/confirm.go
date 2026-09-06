package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/mattn/go-isatty"
)

// confirm prints a yes/no prompt and returns true if the user typed "y" or
// "yes" (case-insensitive). Default on a bare enter is "no" — destructive
// ops should require an explicit acknowledgement.
//
// When stdin is not a TTY (CI, piped input), confirm returns false. Callers
// that need scriptable destructive ops accept `--yes` and skip this entirely.
func confirm(prompt string) bool {
	if !isatty.IsTerminal(os.Stdin.Fd()) {
		return false
	}
	fmt.Fprint(os.Stderr, prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	s := strings.ToLower(strings.TrimSpace(line))
	return s == "y" || s == "yes"
}

// extractYesFlag pulls --yes / -y out of args and returns (remaining, yes).
// Order-independent; supports `--yes` and `-y`.
func extractYesFlag(args []string) ([]string, bool) {
	yes := false
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--yes" || a == "-y" {
			yes = true
			continue
		}
		out = append(out, a)
	}
	return out, yes
}

// extractJSONFlag pulls --json out of args. Order-independent. Read
// commands accept it to switch from a human-friendly table to a
// machine-readable JSON dump on stdout.
func extractJSONFlag(args []string) ([]string, bool) {
	asJSON := false
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--json" {
			asJSON = true
			continue
		}
		out = append(out, a)
	}
	return out, asJSON
}

// emitJSON prints v as indented JSON to stdout. Used by read commands
// when --json is set. Encoding failure is fatal (exit 1): --json is the
// scripting path, and a truncated/empty document must not look like
// success to the consumer.
func emitJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		die("encode json: %v", err)
	}
}
