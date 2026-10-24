package progress

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/fjf/NASArchiveSync/internal/console"
)

const redrawEvery = 80 * time.Millisecond

// Bar renders a single line progress bar with percentage, transferred bytes,
// speed and estimated time of arrival. It is a no-op when disabled, which is
// the case for non terminal output.
type Bar struct {
	mu        sync.Mutex
	out       io.Writer
	line      *Line
	enabled   bool
	finished  bool
	lastWidth int
	lastDraw  time.Time
}

// New creates a progress bar. The bar disables itself when out is not a
// terminal or when total is not positive.
func New(out io.Writer, label string, total int64, enabled bool) *Bar {
	if !enabled || total <= 0 || out == nil || !console.IsTerminal(out) {
		return &Bar{enabled: false}
	}
	now := time.Now()
	return &Bar{
		out:      out,
		line:     NewLine(label, total, console.Width(out, 120)),
		enabled:  true,
		lastDraw: now,
	}
}

// Add increases the transferred counter by n.
func (b *Bar) Add(n int64) {
	if !b.enabled || n <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finished {
		return
	}
	b.line.Add(n)
	if time.Since(b.lastDraw) >= redrawEvery {
		b.drawLocked()
	}
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
	b.line.Set(n)
	if time.Since(b.lastDraw) >= redrawEvery {
		b.drawLocked()
	}
}

func (b *Bar) drawLocked() {
	width := b.line.Width()
	if b.out != nil {
		width = console.Width(b.out, width)
		b.line.SetWidth(width)
	}
	line := b.line.render(time.Now())
	lineWidth := runewidth.StringWidth(line)

	// Pad with spaces to erase the previous, possibly longer line. This avoids
	// ANSI escape sequences, which are not understood by every Windows console.
	clearWidth := b.lastWidth
	if max := width - 1; clearWidth > max {
		clearWidth = max
	}
	pad := 0
	if clearWidth > lineWidth {
		pad = clearWidth - lineWidth
	}
	fmt.Fprintf(b.out, "\r%s%s", line, strings.Repeat(" ", pad))
	b.lastWidth = lineWidth
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
	b.line.Finish()
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
	b.line.Abort()
	b.drawLocked()
	b.finished = true
	fmt.Fprint(b.out, "\n")
}

// Reader wraps an io.Reader and reports progress while reading.
type Reader struct {
	r    io.Reader
	sink Sink
}

// NewReader wraps r so every successfully read byte is reported to sink.
func NewReader(r io.Reader, sink Sink) *Reader {
	return &Reader{r: r, sink: sink}
}

func (r *Reader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 && r.sink != nil {
		r.sink.Add(int64(n))
	}
	return n, err
}
