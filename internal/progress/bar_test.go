package progress

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
)

func TestLineRender(t *testing.T) {
	now := time.Now()
	line := NewLine("上传 a/z.7z", 100*1024*1024, 110)
	line.Set(50 * 1024 * 1024)
	line.lastAt = now.Add(-time.Second)
	line.lastBytes = 40 * 1024 * 1024
	got := line.render(now)

	for _, want := range []string{"上传 a/z.7z", "50.0%", "50.00 MiB/100.00 MiB", "ETA"} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q does not contain %q", got, want)
		}
	}
	if strings.Contains(got, "--:--") {
		t.Errorf("line %q should contain an ETA estimate", got)
	}
	if line.speed <= 0 {
		t.Errorf("speed was not updated: %v", line.speed)
	}
}

func TestLineTruncatesLongLabel(t *testing.T) {
	line := NewLine(strings.Repeat("很长的文件名", 20), 1024, 60)
	line.Set(512)
	got := line.render(time.Now())
	if w, max := runewidth.StringWidth(got), 59; w > max {
		t.Errorf("line width %d exceeds width %d: %q", w, max, got)
	}
	for _, want := range []string{"50.0%", "512 B/1.00 KiB"} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q does not contain %q", got, want)
		}
	}
}

func TestLineFitsWideChineseLabel(t *testing.T) {
	line := NewLine("上传 [金色猫]/The Forbidden Tomes of Olipos.7z", 953*1024*1024, 120)
	got := line.render(time.Now())
	if w, max := runewidth.StringWidth(got), 119; w > max {
		t.Errorf("line width %d exceeds width %d: %q", w, max, got)
	}
	for _, want := range []string{"0.0%", "ETA"} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q does not contain %q", got, want)
		}
	}
}

func TestLineWaitsWithoutTotal(t *testing.T) {
	line := NewLine("上传 等待开始", 0, 80)
	if got := line.render(time.Now()); got != "上传 等待开始" {
		t.Errorf("line = %q", got)
	}
}

func TestBarDrawClearsPreviousLine(t *testing.T) {
	var out bytes.Buffer
	line := NewLine(strings.Repeat("x", 40), 100, 60)
	line.Set(50)
	b := &Bar{out: &out, line: line, enabled: true}
	b.drawLocked()
	firstWidth := b.lastWidth

	line.SetLabel("x")
	b.drawLocked()
	second := out.String()[strings.LastIndex(out.String(), "\r")+1:]
	if got := runewidth.StringWidth(strings.TrimRight(second, " ")); got != b.lastWidth {
		t.Errorf("visible width = %d, want %d", got, b.lastWidth)
	}
	if b.lastWidth >= firstWidth {
		t.Errorf("label shortening did not reduce the line width: %d >= %d", b.lastWidth, firstWidth)
	}
	if got := runewidth.StringWidth(second); got < firstWidth {
		t.Errorf("previous line was not padded: width %d < %d", got, firstWidth)
	}
}

func TestReaderReportsBytes(t *testing.T) {
	line := NewLine("x", 10, 80)
	r := NewReader(strings.NewReader("hello"), line)
	buf := make([]byte, 16)
	if _, err := r.Read(buf); err != nil {
		t.Fatal(err)
	}
	line.mu.Lock()
	got := line.current
	line.mu.Unlock()
	if got != 5 {
		t.Errorf("current = %d, want 5", got)
	}
}
