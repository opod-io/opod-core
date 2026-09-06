package main

import (
	"os"

	"github.com/mattn/go-isatty"
)

// colorEnabled is the single switch every coloring helper consults.
// Set once at startup based on the standard rules: respect the cross-CLI
// NO_COLOR convention (https://no-color.org), the Opod-specific
// OPOD_NO_COLOR override, and stdout-TTY detection so redirected /
// piped output stays clean for grep + jq.
var colorEnabled = func() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("OPOD_NO_COLOR") != "" {
		return false
	}
	return isatty.IsTerminal(os.Stdout.Fd())
}()

// ansi wraps s in an ANSI escape and a reset. No-op when colors are off
// so callers don't have to guard every call site themselves.
func ansi(code, s string) string {
	if !colorEnabled {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func bold(s string) string   { return ansi("1", s) }
func dim(s string) string    { return ansi("2", s) }
func red(s string) string    { return ansi("31", s) }
func green(s string) string  { return ansi("32", s) }
func yellow(s string) string { return ansi("33", s) }
func cyan(s string) string   { return ansi("36", s) }

// ansiCodes returns the raw bold/dim/reset escape strings, or empties when
// color is off. For call sites that interpolate the codes directly into
// Printf format strings rather than wrapping one value — same gating as
// the helpers above, so pipes and OPOD_NO_COLOR stay escape-free.
func ansiCodes() (boldCode, dimCode, resetCode string) {
	if !colorEnabled {
		return "", "", ""
	}
	return "\033[1m", "\033[2m", "\033[0m"
}

// colorEnabledStderr mirrors colorEnabled for output drawn on stderr (the
// progress bar): same NO_COLOR / OPOD_NO_COLOR opt-outs, but keyed to
// stderr's TTY-ness — `opod model add … > log` still has an interactive
// stderr, and `2> log` must stay escape-free even when stdout is a TTY.
var colorEnabledStderr = func() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("OPOD_NO_COLOR") != "" {
		return false
	}
	return isatty.IsTerminal(os.Stderr.Fd())
}()

// ansiErr is ansi gated on stderr's color switch instead of stdout's.
func ansiErr(code, s string) string {
	if !colorEnabledStderr {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func greenErr(s string) string { return ansiErr("32", s) }
func dimErr(s string) string   { return ansiErr("2", s) }
