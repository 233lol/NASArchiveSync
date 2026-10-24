package progress

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func newTestMulti(lines ...*Line) (*MultiBar, *bytes.Buffer) {
	var out bytes.Buffer
	m := &MultiBar{out: &out, enabled: true, lastDraw: time.Now()}
	for _, line := range lines {
		line.SetOnChange(m.requestDraw)
		m.lines = append(m.lines, line)
	}
	return m, &out
}

func TestMultiBarRendersAndRedraws(t *testing.T) {
	m, out := newTestMulti(
		NewLine("上传 file1.7z", 100, 120),
		NewLine("校验 file0.7z", 100, 120),
	)
	m.lines[0].Set(50)
	m.lines[1].Set(75)
	m.drawLocked()

	first := out.String()
	if !strings.Contains(first, "上传 file1.7z") || !strings.Contains(first, "校验 file0.7z") {
		t.Fatalf("first draw missing lines: %q", first)
	}
	if strings.Contains(first, "\x1b[1A") {
		t.Fatalf("first draw must not move the cursor up: %q", first)
	}

	out.Reset()
	m.lines[0].Set(60)
	m.drawLocked()
	second := out.String()
	if !strings.Contains(second, "\x1b[1A") {
		t.Fatalf("redraw should move the cursor up: %q", second)
	}
	if !strings.Contains(second, "上传 file1.7z") || !strings.Contains(second, "校验 file0.7z") {
		t.Fatalf("redraw missing lines: %q", second)
	}
}

func TestMultiBarPauseResume(t *testing.T) {
	m, out := newTestMulti(
		NewLine("上传", 100, 120),
		NewLine("校验", 100, 120),
	)
	m.drawLocked()
	out.Reset()

	m.Pause()
	if !strings.Contains(out.String(), "\x1b[K") {
		t.Fatalf("pause should clear lines: %q", out.String())
	}
	if m.drawn {
		t.Error("area should be marked not drawn after pause")
	}

	out.Reset()
	m.Resume()
	if !strings.Contains(out.String(), "上传") || !strings.Contains(out.String(), "校验") {
		t.Fatalf("resume should redraw: %q", out.String())
	}
}

func TestPauseWriter(t *testing.T) {
	m, areaOut := newTestMulti(
		NewLine("上传", 100, 120),
		NewLine("校验", 100, 120),
	)
	m.drawLocked()
	areaOut.Reset()

	var logOut bytes.Buffer
	w := NewPauseWriter(&logOut, m)
	if _, err := w.Write([]byte("log line\n")); err != nil {
		t.Fatal(err)
	}
	if got := logOut.String(); got != "log line\n" {
		t.Errorf("log output = %q", got)
	}
	if !strings.Contains(areaOut.String(), "上传") || !strings.Contains(areaOut.String(), "校验") {
		t.Errorf("area was not redrawn after log: %q", areaOut.String())
	}
}

func TestMultiBarFinishWithoutDrawWritesNothing(t *testing.T) {
	m, out := newTestMulti(
		NewLine("上传", 100, 120),
		NewLine("校验", 100, 120),
	)
	m.Finish()
	if got := out.String(); got != "" {
		t.Errorf("finish without a draw should not write anything, got %q", got)
	}
}

func TestNewMultiDisabledForBuffer(t *testing.T) {
	m := NewMulti(&bytes.Buffer{}, []string{"a", "b"}, []int64{0, 0}, true)
	if m.Enabled() {
		t.Error("a bytes.Buffer is not a terminal; MultiBar should be disabled")
	}
}
