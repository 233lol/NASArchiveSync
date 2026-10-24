package xlog

import (
	"fmt"
	"io"
	"sync"
)

const (
	reset  = "\x1b[0m"
	cyan   = "\x1b[36m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	red    = "\x1b[31m"
	gray   = "\x1b[90m"
)

// Logger is a small, dependency free, leveled logger writing one line per
// message. It is safe for concurrent use.
type Logger struct {
	mu      sync.Mutex
	out     io.Writer
	verbose bool
	color   bool
}

// New creates a logger writing to out. When color is true, level tags are
// wrapped in ANSI escape sequences.
func New(out io.Writer, verbose, color bool) *Logger {
	return &Logger{out: out, verbose: verbose, color: color}
}

func (l *Logger) write(code, tag, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if l.color {
		tag = code + tag + reset
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.out, "%s %s\n", tag, msg)
}

// Debugf logs only when verbose output was requested.
func (l *Logger) Debugf(format string, args ...any) {
	if l.verbose {
		l.write(gray, "[DBG ]", format, args...)
	}
}

// Infof logs a normal status message.
func (l *Logger) Infof(format string, args ...any) {
	l.write(cyan, "[INFO]", format, args...)
}

// Successf logs a successful operation.
func (l *Logger) Successf(format string, args ...any) {
	l.write(green, "[ OK ]", format, args...)
}

// Warnf logs a non fatal problem.
func (l *Logger) Warnf(format string, args ...any) {
	l.write(yellow, "[WARN]", format, args...)
}

// Errorf logs a failure.
func (l *Logger) Errorf(format string, args ...any) {
	l.write(red, "[FAIL]", format, args...)
}
