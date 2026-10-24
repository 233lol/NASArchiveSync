package console

import (
	"io"
	"os"

	"golang.org/x/term"
)

// IsTerminal reports whether w is attached to a character device, which in
// practice means a terminal window.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// Width returns the terminal width for w, or fallback when it cannot be
// determined (for example when output is redirected).
func Width(w io.Writer, fallback int) int {
	f, ok := w.(*os.File)
	if !ok {
		return fallback
	}
	width, _, err := term.GetSize(int(f.Fd()))
	if err != nil || width <= 0 {
		return fallback
	}
	return width
}
