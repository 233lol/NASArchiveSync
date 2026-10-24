package progress

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/fjf/NASArchiveSync/internal/humanize"
)

const barWidth = 24

// Sink receives transferred byte counts.
type Sink interface {
	Add(int64)
}

// Counter is a progress sink that can be completed.
type Counter interface {
	Sink
	Finish()
	Abort()
}

// Line renders one progress line. All methods are safe for concurrent use.
type Line struct {
	mu        sync.Mutex
	label     string
	total     int64
	current   int64
	width     int
	startedAt time.Time
	lastAt    time.Time
	lastBytes int64
	speed     float64
	finished  bool
	onChange  func()
}

// NewLine creates a line with the given label, total size and terminal width.
func NewLine(label string, total int64, width int) *Line {
	now := time.Now()
	return &Line{
		label:     label,
		total:     total,
		width:     width,
		startedAt: now,
		lastAt:    now,
	}
}

// SetOnChange registers a callback invoked after the line state changes.
func (l *Line) SetOnChange(fn func()) {
	l.mu.Lock()
	l.onChange = fn
	l.mu.Unlock()
}

// Width returns the current render width.
func (l *Line) Width() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.width
}

// SetWidth updates the render width.
func (l *Line) SetWidth(w int) {
	if w <= 0 {
		return
	}
	l.mu.Lock()
	l.width = w
	l.mu.Unlock()
}

// Reset starts a new phase on this line.
func (l *Line) Reset(label string, total int64) {
	now := time.Now()
	l.mu.Lock()
	l.label = label
	l.total = total
	l.current = 0
	l.startedAt = now
	l.lastAt = now
	l.lastBytes = 0
	l.speed = 0
	l.finished = false
	l.mu.Unlock()
	l.changed()
}

// SetLabel changes the label without resetting progress.
func (l *Line) SetLabel(label string) {
	l.mu.Lock()
	l.label = label
	l.mu.Unlock()
	l.changed()
}

// Add increases the transferred counter by n.
func (l *Line) Add(n int64) {
	if n <= 0 {
		return
	}
	l.mu.Lock()
	if l.finished {
		l.mu.Unlock()
		return
	}
	next := l.current + n
	if l.total > 0 && next > l.total {
		next = l.total
	}
	l.current = next
	l.mu.Unlock()
	l.changed()
}

// Set sets the transferred counter to n.
func (l *Line) Set(n int64) {
	if n < 0 {
		n = 0
	}
	l.mu.Lock()
	if l.finished {
		l.mu.Unlock()
		return
	}
	if l.total > 0 && n > l.total {
		n = l.total
	}
	l.current = n
	l.mu.Unlock()
	l.changed()
}

// Finish marks the line complete.
func (l *Line) Finish() {
	l.mu.Lock()
	if l.finished {
		l.mu.Unlock()
		return
	}
	l.current = l.total
	l.finished = true
	l.mu.Unlock()
	l.changed()
}

// Abort marks the line finished without forcing it to 100 percent.
func (l *Line) Abort() {
	l.mu.Lock()
	if l.finished {
		l.mu.Unlock()
		return
	}
	l.finished = true
	l.mu.Unlock()
	l.changed()
}

func (l *Line) changed() {
	l.mu.Lock()
	fn := l.onChange
	l.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// render builds the visible line and updates the speed estimate.
func (l *Line) render(now time.Time) string {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.total <= 0 {
		return l.label
	}

	elapsed := now.Sub(l.lastAt).Seconds()
	if elapsed > 0 {
		instant := float64(l.current-l.lastBytes) / elapsed
		if instant < 0 {
			instant = 0
		}
		if l.speed <= 0 {
			l.speed = instant
		} else {
			l.speed = l.speed*0.7 + instant*0.3
		}
	}
	l.lastAt = now
	l.lastBytes = l.current

	ratio := float64(l.current) / float64(l.total)
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	w := barWidth
	switch {
	case l.width < 80:
		w = 10
	case l.width < 100:
		w = 16
	}
	filled := int(ratio * float64(w))
	if filled > w {
		filled = w
	}
	blocks := strings.Repeat("=", filled)
	if filled < w {
		blocks += ">" + strings.Repeat(" ", w-filled-1)
	}

	parts := []string{
		fmt.Sprintf("%5.1f%%", ratio*100),
		"[" + blocks + "]",
		fmt.Sprintf("%s/%s", humanize.Bytes(l.current), humanize.Bytes(l.total)),
	}
	if l.width >= 70 {
		parts = append(parts, humanize.Rate(l.speed))
	}
	if l.width >= 85 {
		var eta string
		if l.speed > 1 {
			remaining := float64(l.total-l.current) / l.speed
			eta = humanize.Clock(time.Duration(remaining * float64(time.Second)))
		} else {
			eta = "--:--"
		}
		parts = append(parts, "ETA "+eta)
	}

	stats := " " + strings.Join(parts, " ")
	maxLabel := l.width - runewidth.StringWidth(stats) - 1
	if maxLabel < 0 {
		maxLabel = 0
	}
	line := runewidth.Truncate(l.label, maxLabel, "...") + stats
	if runewidth.StringWidth(line) > l.width-1 {
		line = runewidth.Truncate(line, l.width-1, "...")
	}
	return line
}
