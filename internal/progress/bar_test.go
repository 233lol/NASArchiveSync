package progress

import (
	"strings"
	"testing"
	"time"
)

func TestRenderLine(t *testing.T) {
	now := time.Now()
	b := &Bar{
		label:     "上传 a/z.7z",
		total:     100 * 1024 * 1024,
		current:   50 * 1024 * 1024,
		width:     110,
		enabled:   true,
		lastAt:    now.Add(-time.Second),
		lastBytes: 40 * 1024 * 1024,
	}
	line := b.renderLine(now)

	for _, want := range []string{"上传 a/z.7z", "50.0%", "50.00 MiB/100.00 MiB", "ETA"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q does not contain %q", line, want)
		}
	}
	if strings.Contains(line, "--:--") {
		t.Errorf("line %q should contain an ETA estimate", line)
	}
	if b.speed <= 0 {
		t.Errorf("speed was not updated: %v", b.speed)
	}
}

func TestRenderLineTruncatesLongLabel(t *testing.T) {
	b := &Bar{
		label:     strings.Repeat("很长的文件名", 20),
		total:     1024,
		current:   512,
		width:     60,
		enabled:   true,
		lastAt:    time.Now(),
		lastBytes: 0,
	}
	line := b.renderLine(time.Now())
	if got, max := len([]rune(line)), 59; got > max {
		t.Errorf("line length %d exceeds width %d: %q", got, max, line)
	}
	for _, want := range []string{"50.0%", "512 B/1.00 KiB"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q does not contain %q", line, want)
		}
	}
}
