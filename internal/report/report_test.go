package report

import (
	"strings"
	"testing"
	"time"
)

func sampleRecord() Record {
	return Record{
		FileName:      "z.7z",
		TargetPath:    `\\192.168.12.1\mnt\hdd1\game\a\z.7z`,
		Size:          123456789,
		SHA256:        strings.Repeat("ab", 32),
		UploadedAt:    time.Date(2026, 10, 24, 15, 4, 5, 0, time.FixedZone("CST", 8*3600)),
		SourcePath:    `C:\game\a\z.7z`,
		SourceModTime: time.Date(2026, 10, 20, 9, 12, 31, 0, time.FixedZone("CST", 8*3600)),
		Duration:      28*time.Second + 400*time.Millisecond,
		ToolVersion:   "v0.1.0",
		Verified:      true,
	}
}

func TestRenderParseRoundTrip(t *testing.T) {
	in := sampleRecord()
	out, err := Parse([]byte(in.Render()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if out.FileName != in.FileName || out.TargetPath != in.TargetPath || out.Size != in.Size {
		t.Errorf("basic fields mismatch: %+v", out)
	}
	if out.SHA256 != in.SHA256 {
		t.Errorf("sha mismatch: %q != %q", out.SHA256, in.SHA256)
	}
	if !out.UploadedAt.Equal(in.UploadedAt) {
		t.Errorf("time mismatch: %v != %v", out.UploadedAt, in.UploadedAt)
	}
	if !out.SourceModTime.Equal(in.SourceModTime) {
		t.Errorf("source time mismatch: %v != %v", out.SourceModTime, in.SourceModTime)
	}
	if !out.Verified {
		t.Error("verified flag lost")
	}
}

func TestParseRejectsMissingSHA(t *testing.T) {
	_, err := Parse([]byte("原始文件名: z.7z\n文件大小(字节): 1\n"))
	if err == nil {
		t.Fatal("expected an error when SHA256 is missing")
	}
}

func TestParseRejectsBadSHA(t *testing.T) {
	_, err := Parse([]byte("SHA256: not-a-hash\n"))
	if err == nil {
		t.Fatal("expected an error for a malformed SHA256")
	}
}

func TestSidecarName(t *testing.T) {
	if got := SidecarName("z.7z"); got != "z.7z.txt" {
		t.Fatalf("got %q", got)
	}
}
