//go:build windows

package console

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// EnableVirtualTerminal turns on ANSI escape processing for a Windows console.
// It returns true when w already supports ANSI or was enabled successfully.
func EnableVirtualTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	h := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	if err := windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return false
	}
	return true
}
