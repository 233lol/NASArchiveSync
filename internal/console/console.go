package console

import (
	"io"
	"os"

	"golang.org/x/term"
)

// IsTerminal reports whether w is attached to a real terminal. Checking the
// character-device bit is not enough: NUL and some redirected handles are
// character devices but cannot be used for interactive password input.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
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
