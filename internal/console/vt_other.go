//go:build !windows

package console

import "io"

// EnableVirtualTerminal reports whether w is a terminal. ANSI escape sequences
// are expected to work on non-Windows terminals.
func EnableVirtualTerminal(w io.Writer) bool {
	return IsTerminal(w)
}
