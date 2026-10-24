package humanize

import (
	"fmt"
	"time"
)

// Bytes formats a byte count using binary units (KiB, MiB, ...).
func Bytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 5; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Rate formats a transfer rate in bytes per second.
func Rate(bytesPerSecond float64) string {
	if bytesPerSecond < 1 {
		return "0 B/s"
	}
	return Bytes(int64(bytesPerSecond)) + "/s"
}

// Clock formats a duration as mm:ss or h:mm:ss.
func Clock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// Duration formats an elapsed duration for humans.
func Duration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return Clock(d)
}
