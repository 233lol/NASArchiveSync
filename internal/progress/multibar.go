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

// MultiBar renders a fixed block of progress lines in place. It needs a
// terminal that understands ANSI cursor movement; NewMulti disables itself
// otherwise.
type MultiBar struct {
	mu       sync.Mutex
	out      io.Writer
	lines    []*Line
	drawn    bool
	paused   bool
	finished bool
	enabled  bool
	lastDraw time.Time
}

// NewMulti creates a multi-line progress area. labels and totals define the
// initial lines; callers update them through Line(i).
func NewMulti(out io.Writer, labels []string, totals []int64, enabled bool) *MultiBar {
	if !enabled || out == nil || !console.IsTerminal(out) || !console.EnableVirtualTerminal(out) {
		return &MultiBar{enabled: false}
	}
	m := &MultiBar{out: out, enabled: true}
	width := console.Width(out, 120)
	for i, label := range labels {
		var total int64
		if i < len(totals) {
			total = totals[i]
		}
		line := NewLine(label, total, width)
		line.SetOnChange(m.requestDraw)
		m.lines = append(m.lines, line)
	}
	return m
}

// Enabled reports whether the area can render.
func (m *MultiBar) Enabled() bool {
	return m != nil && m.enabled
}

// Line returns line i, or nil when out of range.
func (m *MultiBar) Line(i int) *Line {
	if m == nil || i < 0 || i >= len(m.lines) {
		return nil
	}
	return m.lines[i]
}

func (m *MultiBar) requestDraw() {
	if !m.enabled {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.paused || m.finished {
		return
	}
	if time.Since(m.lastDraw) < redrawEvery {
		return
	}
	m.drawLocked()
}

func (m *MultiBar) drawLocked() {
	width := console.Width(m.out, 120)
	rendered := make([]string, len(m.lines))
	for i, line := range m.lines {
		line.SetWidth(width)
		rendered[i] = line.render(time.Now())
		if w := runewidth.StringWidth(rendered[i]); w > width-1 {
			rendered[i] = runewidth.Truncate(rendered[i], width-1, "...")
		}
	}

	var b strings.Builder
	if m.drawn && len(rendered) > 1 {
		fmt.Fprintf(&b, "\x1b[%dA", len(rendered)-1)
	}
	for i, line := range rendered {
		b.WriteString("\r")
		b.WriteString(line)
		b.WriteString("\x1b[K")
		if i < len(rendered)-1 {
			b.WriteString("\n")
		}
	}
	fmt.Fprint(m.out, b.String())
	m.drawn = true
	m.lastDraw = time.Now()
}

// Pause clears the progress block and leaves the cursor at its first line.
// Logs can then be written without being overwritten by the next redraw.
func (m *MultiBar) Pause() {
	if !m.enabled {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.drawn || m.paused {
		return
	}
	m.clearLocked()
	m.paused = true
}

func (m *MultiBar) clearLocked() {
	n := len(m.lines)
	var b strings.Builder
	if n > 1 {
		fmt.Fprintf(&b, "\x1b[%dA", n-1)
	}
	for i := 0; i < n; i++ {
		b.WriteString("\r\x1b[K")
		if i < n-1 {
			b.WriteString("\n")
		}
	}
	if n > 1 {
		fmt.Fprintf(&b, "\x1b[%dA", n-1)
	}
	fmt.Fprint(m.out, b.String())
	m.drawn = false
}

// Resume redraws the progress block below the current cursor position.
func (m *MultiBar) Resume() {
	if !m.enabled {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.paused {
		return
	}
	m.paused = false
	m.drawLocked()
}

// Finish draws the final state and moves to the next line.
func (m *MultiBar) Finish() {
	if !m.enabled {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.finished {
		return
	}
	m.finished = true
	// Nothing was ever drawn (for example every file failed before its first
	// progress update); leave the terminal untouched instead of printing two
	// placeholder lines.
	if !m.drawn {
		return
	}
	m.paused = false
	m.drawLocked()
	fmt.Fprint(m.out, "\n")
}
