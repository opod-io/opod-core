package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/mattn/go-isatty"
	"golang.org/x/term"
)

// pickerItem is one row in the interactive picker. ID is what gets
// returned on selection; Label is the primary display column; Meta is
// the dim secondary column (license, description, capabilities, …).
type pickerItem struct {
	ID    string
	Label string
	Meta  string
}

const pickerVisibleRows = 12

// pickFromList renders an interactive ↑↓ / enter picker. Type to filter
// (substring first, falls back to subsequence so "mstr" finds "mistral").
// Returns the selected ID, or "" if the user cancelled or stdin isn't a TTY.
//
// `seed` pre-populates the filter — useful when the user typed an unknown
// ID and we want to land them close to what they meant.
func pickFromList(prompt string, items []pickerItem, seed string) string {
	if len(items) == 0 {
		return ""
	}
	inFD := int(os.Stdin.Fd())
	if !isatty.IsTerminal(uintptr(inFD)) || !isatty.IsTerminal(os.Stderr.Fd()) {
		// Non-interactive (CI, piped). Caller will die with a clear error.
		return ""
	}
	oldState, err := term.MakeRaw(inFD)
	if err != nil {
		return ""
	}
	defer func() { _ = term.Restore(inFD, oldState) }()

	out := os.Stderr
	fmt.Fprint(out, "\x1b[?25l")       // hide cursor
	defer fmt.Fprint(out, "\x1b[?25h") // restore on exit

	// A SIGTERM/SIGHUP mid-pick would otherwise leave the terminal in raw
	// mode with the cursor hidden (the defers above never run). Restore
	// the terminal, then re-raise so the process still dies with the
	// conventional signal status. Stopped (and the goroutine released) on
	// normal return.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	go func() {
		s, open := <-sigc
		if !open {
			return
		}
		_ = term.Restore(inFD, oldState)
		fmt.Fprint(out, "\x1b[?25h")
		signal.Stop(sigc)
		// Re-raise so the process dies with the conventional signal
		// status (portable: best-effort no-op where unsupported).
		if p, perr := os.FindProcess(os.Getpid()); perr == nil {
			_ = p.Signal(s)
		}
		os.Exit(1)
	}()
	defer func() {
		signal.Stop(sigc)
		close(sigc)
	}()

	query := seed
	selected := 0
	top := 0
	rowsDrawn := 0

	eraseFrame := func() {
		if rowsDrawn == 0 {
			return
		}
		fmt.Fprint(out, "\r\x1b[K")
		for i := 1; i < rowsDrawn; i++ {
			fmt.Fprint(out, "\x1b[1A\r\x1b[K")
		}
		rowsDrawn = 0
	}

	draw := func() {
		filtered := filterPickerItems(items, query)
		if selected >= len(filtered) {
			selected = len(filtered) - 1
		}
		if selected < 0 {
			selected = 0
		}
		// Slide the window so the selection stays on screen: scroll up
		// when it moves above `top`, down when it passes the last visible
		// row, and clamp to the list bounds.
		if selected < top {
			top = selected
		}
		if selected >= top+pickerVisibleRows {
			top = selected - pickerVisibleRows + 1
		}
		if top > len(filtered)-pickerVisibleRows {
			top = len(filtered) - pickerVisibleRows
		}
		if top < 0 {
			top = 0
		}
		end := top + pickerVisibleRows
		if end > len(filtered) {
			end = len(filtered)
		}
		visible := filtered[top:end]
		idLen := 0
		for _, it := range visible {
			if len(it.ID) > idLen {
				idLen = len(it.ID)
			}
		}

		var b strings.Builder
		b.WriteString(prompt)
		b.WriteString("  \x1b[1m> ")
		b.WriteString(query)
		b.WriteString("\x1b[0m\x1b[K\n\r")
		if len(visible) == 0 {
			b.WriteString("  \x1b[2m(no matches — type to filter, esc to cancel)\x1b[0m\x1b[K\n\r")
		}
		extraRows := 0
		if top > 0 {
			b.WriteString(fmt.Sprintf("  \x1b[2m↑ %d more above\x1b[0m\x1b[K\n\r", top))
			extraRows++
		}
		for i, it := range visible {
			if top+i == selected {
				b.WriteString("\x1b[7m▸ ")
			} else {
				b.WriteString("  ")
			}
			pad := idLen - len(it.ID)
			if pad < 0 {
				pad = 0
			}
			b.WriteString(it.ID)
			b.WriteString(strings.Repeat(" ", pad))
			if it.Meta != "" {
				if top+i == selected {
					b.WriteString("  ")
					b.WriteString(it.Meta)
				} else {
					b.WriteString("  \x1b[2m")
					b.WriteString(it.Meta)
				}
			}
			b.WriteString("\x1b[0m\x1b[K\n\r")
		}
		if below := len(filtered) - end; below > 0 {
			b.WriteString(fmt.Sprintf("  \x1b[2m↓ %d more below\x1b[0m\x1b[K\n\r", below))
			extraRows++
		}
		hint := fmt.Sprintf("\x1b[2m  %d match", len(filtered))
		if len(filtered) != 1 {
			hint += "es"
		}
		hint += " · ↑↓ select · enter to confirm · esc to cancel\x1b[0m\x1b[K"
		b.WriteString(hint)

		fmt.Fprint(out, b.String())
		rowsDrawn = 1 + maxInt(1, len(visible)) + extraRows + 1
	}

	draw()

	buf := make([]byte, 8)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			eraseFrame()
			return ""
		}
		// Arrow keys: ESC [ A/B/C/D
		if n >= 3 && buf[0] == 0x1b && buf[1] == '[' {
			switch buf[2] {
			case 'A':
				if selected > 0 {
					selected--
				}
			case 'B':
				selected++
			}
			eraseFrame()
			draw()
			continue
		}
		c := buf[0]
		switch c {
		case '\r', '\n':
			filtered := filterPickerItems(items, query)
			eraseFrame()
			if selected >= 0 && selected < len(filtered) {
				return filtered[selected].ID
			}
			return ""
		case 0x03, 0x1b: // Ctrl-C, lone Esc
			eraseFrame()
			return ""
		case 0x7f, 0x08: // backspace
			if len(query) > 0 {
				query = query[:len(query)-1]
			}
			selected = 0
		default:
			if c >= 0x20 && c < 0x7f {
				query += string(c)
				selected = 0
			} else {
				continue
			}
		}
		eraseFrame()
		draw()
	}
}

// filterPickerItems keeps items whose ID/Label/Meta contains q (substring,
// case-insensitive). Falls back to subsequence match on ID/Label so
// "mstr" still finds "mistral-nemo-12b".
func filterPickerItems(items []pickerItem, q string) []pickerItem {
	if q == "" {
		return items
	}
	ql := strings.ToLower(q)
	var out []pickerItem
	for _, it := range items {
		hay := strings.ToLower(it.ID + " " + it.Label + " " + it.Meta)
		if strings.Contains(hay, ql) {
			out = append(out, it)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, it := range items {
		if subseqContainsFold(it.ID, ql) || subseqContainsFold(it.Label, ql) {
			out = append(out, it)
		}
	}
	return out
}

func subseqContainsFold(s, qLower string) bool {
	sl := strings.ToLower(s)
	j := 0
	for i := 0; i < len(sl) && j < len(qLower); i++ {
		if sl[i] == qLower[j] {
			j++
		}
	}
	return j == len(qLower)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
