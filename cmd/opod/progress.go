package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"golang.org/x/term"
)

// progressBar renders a single-line ANSI progress bar that gets redrawn
// in place. Designed to plug into the engine `Pull` callback shape
//
//	func(status string, completed, total int64)
//
// On non-TTY destinations (CI, piped output) it falls back to occasional
// human-readable updates so logs stay grep-able without exploding in size.
type progressBar struct {
	label    string
	start    time.Time
	lastDraw time.Time
	isTTY    bool
	width    int
	finished bool

	// Rate is sampled over a ~1s moving window so the displayed MB/s
	// and ETA don't twitch with every chunk arrival. The bar fill
	// itself still redraws at ~20fps from `completed`.
	rate      int64
	rateAt    time.Time
	rateBytes int64
}

func newProgressBar(label string) *progressBar {
	fd := os.Stderr.Fd()
	tty := isatty.IsTerminal(fd)
	w := 80
	if tty {
		if cols, _, err := term.GetSize(int(fd)); err == nil && cols > 40 {
			w = cols
		}
	}
	return &progressBar{
		label: label,
		start: time.Now(),
		isTTY: tty,
		width: w,
	}
}

// update is intended for use as the engine Pull callback. status is a
// short string ("downloading", "verifying", "writing manifest") that the
// engine reports per chunk; completed/total are bytes.
func (p *progressBar) update(status string, completed, total int64) {
	if p.finished {
		return
	}
	now := time.Now()

	// Rate-limit redraws to ~20fps on TTY so we don't spam the terminal.
	if p.isTTY && now.Sub(p.lastDraw) < 50*time.Millisecond && completed != total {
		return
	}

	if !p.isTTY {
		// Coarser updates for non-TTY: every 5 s or on completion.
		if now.Sub(p.lastDraw) < 5*time.Second && completed != total {
			return
		}
		if total > 0 {
			fmt.Fprintf(os.Stderr, "  %s %s %s / %s\n",
				p.label, status, humanBytes(completed), humanBytes(total))
		} else {
			fmt.Fprintf(os.Stderr, "  %s %s\n", p.label, status)
		}
		p.lastDraw = now
		return
	}

	// Refresh the rate sample once per second so the displayed MB/s and
	// ETA stay readable. Between samples we reuse the previous value,
	// which lets the bar redraw smoothly without making the numbers
	// flicker on every chunk.
	if p.rateAt.IsZero() {
		p.rateAt = p.start
	}
	if dt := now.Sub(p.rateAt); dt >= time.Second {
		switch {
		case p.rateBytes > 0:
			p.rate = int64(float64(completed-p.rateBytes) / dt.Seconds())
		case completed > 0:
			// Bootstrap on the very first window: average since start so
			// the user sees a number on the first tick instead of nothing.
			if since := now.Sub(p.start); since > 0 {
				p.rate = int64(float64(completed) / since.Seconds())
			}
		}
		p.rateAt = now
		p.rateBytes = completed
	}
	// On the final update, recompute over whatever time has elapsed so
	// completion shows an accurate average rather than the last sample.
	if completed == total && total > 0 && p.rateBytes > 0 {
		if dt := now.Sub(p.rateAt); dt > 0 && completed-p.rateBytes > 0 {
			p.rate = int64(float64(completed-p.rateBytes) / dt.Seconds())
		}
	}
	rate := p.rate

	// Right side: "12.3/17.0 GB · 85 MB/s · ETA 0:52"
	var right string
	if total > 0 {
		right = fmt.Sprintf(" %s/%s", humanBytes(completed), humanBytes(total))
		if rate > 0 {
			right += fmt.Sprintf(" · %s/s", humanBytes(rate))
			if completed < total {
				eta := time.Duration(float64(total-completed)/float64(rate)) * time.Second
				right += " · ETA " + shortDur(eta)
			}
		}
	} else if rate > 0 {
		right = fmt.Sprintf(" %s · %s/s", humanBytes(completed), humanBytes(rate))
	} else if completed > 0 {
		right = " " + humanBytes(completed)
	}

	// Left side: "  pulling qwen3.6-27b · downloading "
	left := fmt.Sprintf("  %s · %s ", p.label, status)

	// Bar fills the remaining width.
	bar := ""
	if total > 0 {
		barWidth := p.width - len(left) - len(right) - 2
		if barWidth < 8 {
			barWidth = 0
		}
		if barWidth > 0 {
			filled := int(float64(barWidth) * float64(completed) / float64(total))
			if filled > barWidth {
				filled = barWidth
			}
			// The bar draws on stderr, so its color gate must be stderr's,
			// not stdout's (`opod model add … > log` keeps the colors;
			// `2> log` drops them).
			bar = "[" + greenErr(strings.Repeat("█", filled)) + dimErr(strings.Repeat("░", barWidth-filled)) + "]"
		}
	}

	line := left + bar + right
	// Only truncate when colors are off — otherwise ANSI escape bytes
	// inflate len(line) and we'd slice into an escape sequence. The line
	// goes to stderr, so consult stderr's color switch.
	if !colorEnabledStderr && len(line) > p.width {
		line = line[:p.width]
	}
	fmt.Fprintf(os.Stderr, "\r\x1b[K%s", line)
	p.lastDraw = now
}

// done finalizes the bar — newline so subsequent output starts cleanly.
func (p *progressBar) done() {
	if p.finished {
		return
	}
	p.finished = true
	if p.isTTY {
		fmt.Fprintln(os.Stderr)
	}
}

func humanBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const (
		k = 1024
		m = k * 1024
		g = m * 1024
		t = g * 1024
	)
	switch {
	case n >= t:
		return fmt.Sprintf("%.1f TB", float64(n)/t)
	case n >= g:
		return fmt.Sprintf("%.1f GB", float64(n)/g)
	case n >= m:
		return fmt.Sprintf("%.1f MB", float64(n)/m)
	case n >= k:
		return fmt.Sprintf("%.0f KB", float64(n)/k)
	}
	return fmt.Sprintf("%d B", n)
}

// shortDur renders a duration in MM:SS or H:MM:SS so progress lines stay
// narrow. Caps at "99:59:59" — anything bigger means something is wrong.
func shortDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, (s%3600)/60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}
