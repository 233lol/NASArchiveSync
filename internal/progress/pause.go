package progress

import "io"

// PauseWriter temporarily clears a MultiBar before writing a log line and
// redraws it afterwards. It is meant to wrap a logger's output.
type PauseWriter struct {
	out  io.Writer
	area *MultiBar
}

// NewPauseWriter wraps out so writes do not corrupt area.
func NewPauseWriter(out io.Writer, area *MultiBar) *PauseWriter {
	if out == nil {
		out = io.Discard
	}
	return &PauseWriter{out: out, area: area}
}

func (w *PauseWriter) Write(p []byte) (int, error) {
	if w.area != nil {
		w.area.Pause()
	}
	n, err := w.out.Write(p)
	if w.area != nil {
		w.area.Resume()
	}
	return n, err
}
