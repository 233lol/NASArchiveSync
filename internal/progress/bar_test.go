package progress

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
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
	if got, max := runewidth.StringWidth(line), 59; got > max {
		t.Errorf("line width %d exceeds width %d: %q", got, max, line)
	}
	for _, want := range []string{"50.0%", "512 B/1.00 KiB"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q does not contain %q", line, want)
		}
	}
}

func TestRenderLineAccountsForWideCharacters(t *testing.T) {
	b := &Bar{
		label:     "上传 [金色猫]/" + strings.Repeat("很长的文件名", 10),
		total:     1024,
		current:   512,
		width:     60,
		enabled:   true,
		lastAt:    time.Now(),
		lastBytes: 0,
	}
	line := b.renderLine(time.Now())
	if got, max := runewidth.StringWidth(line), 59; got > max {
		t.Errorf("line width %d exceeds width %d: %q", got, max, line)
	}
	for _, want := range []string{"50.0%", "512 B/1.00 KiB"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q does not contain %q", line, want)
		}
	}
}

func TestRenderLineFitsWideChineseLabel(t *testing.T) {
	b := &Bar{
		label:     "上传 [金色猫]/The Forbidden Tomes of Olipos.7z",
		total:     953 * 1024 * 1024,
		current:   0,
		width:     120,
		enabled:   true,
		lastAt:    time.Now(),
		lastBytes: 0,
	}
	line := b.renderLine(time.Now())
	if got, max := runewidth.StringWidth(line), 119; got > max {
		t.Errorf("line width %d exceeds width %d: %q", got, max, line)
	}
	if !strings.Contains(line, "ETA") {
		t.Errorf("line %q should keep the ETA field", line)
	}
	if !strings.Contains(line, "0.0%") {
		t.Errorf("line %q should keep the percentage", line)
	}
}

func TestDrawClearsPreviousLine(t *testing.T) {
	var out bytes.Buffer
	b := &Bar{
		out:     &out,
		label:   strings.Repeat("x", 40),
		total:   100,
		current: 50,
		width:   60,
		enabled: true,
		lastAt:  time.Now(),
	}
	b.drawLocked()
	firstWidth := b.lastWidth

	b.label = "x"
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
