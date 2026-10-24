package progress

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/fjf/NASArchiveSync/internal/console"
	"github.com/fjf/NASArchiveSync/internal/humanize"
)

const (
	barWidth    = 24
	redrawEvery = 80 * time.Millisecond
)

// Bar renders a single line progress bar with percentage, transferred bytes,
// speed and estimated time of arrival. It is a no-op when disabled, which is
// the case for non terminal output.
type Bar struct {
	mu        sync.Mutex
	out       io.Writer
	label     string
	total     int64
	current   int64
	width     int
	enabled   bool
	finished  bool
	startedAt time.Time
	lastDraw  time.Time
	lastAt    time.Time
	lastBytes int64
	speed     float64
	lastWidth int
}

// New creates a progress bar. The bar disables itself when out is not a
// terminal or when total is not positive.
func New(out io.Writer, label string, total int64, enabled bool) *Bar {
	if !enabled || total <= 0 || out == nil || !console.IsTerminal(out) {
		return &Bar{enabled: false}
	}
	now := time.Now()
	return &Bar{
		out:       out,
		label:     label,
		total:     total,
		width:     console.Width(out, 120),
		enabled:   true,
		startedAt: now,
		lastAt:    now,
		lastBytes: 0,
	}
}

// Add increases the transferred counter by n.
func (b *Bar) Add(n int64) {
	if n <= 0 {
		return
	}
	b.Set(b.current + n)
}

// Set updates the transferred counter and repaints when enough time passed.
func (b *Bar) Set(n int64) {
	if !b.enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finished {
		return
	}
	if n > b.total {
		n = b.total
	}
	b.current = n
	if time.Since(b.lastDraw) >= redrawEvery {
		b.drawLocked()
	}
}

func (b *Bar) drawLocked() {
	if b.out != nil {
		if w := console.Width(b.out, b.width); w > 0 {
			b.width = w
		}
	}
	line := b.renderLine(time.Now())
	lineWidth := runewidth.StringWidth(line)

	// Pad with spaces to erase the previous, possibly longer line. This avoids
	// ANSI escape sequences, which are not understood by every Windows console.
	clearWidth := b.lastWidth
	if max := b.width - 1; clearWidth > max {
		clearWidth = max
	}
	pad := 0
	if clearWidth > lineWidth {
		pad = clearWidth - lineWidth
	}
	fmt.Fprintf(b.out, "\r%s%s", line, strings.Repeat(" ", pad))
	b.lastWidth = lineWidth
}

// renderLine builds the visible bar line and updates the speed estimate.
func (b *Bar) renderLine(now time.Time) string {
	elapsed := now.Sub(b.lastAt).Seconds()
	if elapsed > 0 {
		instant := float64(b.current-b.lastBytes) / elapsed
		if instant < 0 {
			instant = 0
		}
		if b.speed <= 0 {
			b.speed = instant
		} else {
			b.speed = b.speed*0.7 + instant*0.3
		}
	}
	b.lastAt = now
	b.lastBytes = b.current
	b.lastDraw = now

	ratio := float64(b.current) / float64(b.total)
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	barW := barWidth
	switch {
	case b.width < 80:
		barW = 10
	case b.width < 100:
		barW = 16
	}
	filled := int(ratio * float64(barW))
	if filled > barW {
		filled = barW
	}
	blocks := strings.Repeat("=", filled)
	if filled < barW {
		blocks += ">" + strings.Repeat(" ", barW-filled-1)
	}

	parts := []string{
		fmt.Sprintf("%5.1f%%", ratio*100),
		"[" + blocks + "]",
		fmt.Sprintf("%s/%s", humanize.Bytes(b.current), humanize.Bytes(b.total)),
	}
	if b.width >= 70 {
		parts = append(parts, humanize.Rate(b.speed))
	}
	if b.width >= 85 {
		var eta string
		if b.speed > 1 {
			remaining := float64(b.total-b.current) / b.speed
			eta = humanize.Clock(time.Duration(remaining * float64(time.Second)))
		} else {
			eta = "--:--"
		}
		parts = append(parts, "ETA "+eta)
	}

	stats := " " + strings.Join(parts, " ")
	maxLabel := b.width - runewidth.StringWidth(stats) - 1
	if maxLabel < 0 {
		maxLabel = 0
	}
	line := runewidth.Truncate(b.label, maxLabel, "...") + stats
	if runewidth.StringWidth(line) > b.width-1 {
		line = runewidth.Truncate(line, b.width-1, "...")
	}
	return line
}

// Finish completes the bar and moves to the next line.
func (b *Bar) Finish() {
	if !b.enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finished {
		return
	}
	b.current = b.total
	b.drawLocked()
	b.finished = true
	fmt.Fprint(b.out, "\n")
}

// Abort stops the bar without forcing it to 100 percent.
func (b *Bar) Abort() {
	if !b.enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finished {
		return
	}
	b.drawLocked()
	b.finished = true
	fmt.Fprint(b.out, "\n")
}

// Reader wraps an io.Reader and reports progress while reading.
type Reader struct {
	r   io.Reader
	bar *Bar
}

// NewReader wraps r so every successfully read byte is reported to bar.
func NewReader(r io.Reader, bar *Bar) *Reader {
	return &Reader{r: r, bar: bar}
}

func (r *Reader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 && r.bar != nil {
		r.bar.Add(int64(n))
	}
	return n, err
}
