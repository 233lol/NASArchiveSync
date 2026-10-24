// Package report renders and parses the small completion note that is written
// next to every uploaded archive (for example z.7z.txt).
package report

import (
	"encoding/hex"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/fjf/NASArchiveSync/internal/humanize"
)

const (
	KeyFileName     = "原始文件名"
	KeyTargetPath   = "上传路径"
	KeySize         = "文件大小(字节)"
	KeySHA256       = "SHA256"
	KeyVerifyMode   = "校验方式"
	KeyUploadedAt   = "上传时间"
	KeyVerified     = "校验结果"
	KeySourcePath   = "源路径"
	KeySourceMTime  = "源文件修改时间"
	KeyDuration     = "上传耗时"
	KeyToolVersion  = "工具版本"
	timeLayout      = "2006-01-02 15:04:05 -07:00"
	verifiedMessage = "本地与远端 SHA256 一致"
	// VerifyModeNone marks a note written with --no-verify. Such a note has no
	// SHA256 and must not be treated as a verified upload.
	VerifyModeNone = "none"
	shaNotComputed = "未计算"
)

// Record is the content of a completion note.
type Record struct {
	FileName      string
	TargetPath    string
	Size          int64
	SHA256        string
	UploadedAt    time.Time
	SourcePath    string
	SourceModTime time.Time
	Duration      time.Duration
	ToolVersion   string
	Verified      bool
	// NoVerify is true when the archive was uploaded with --no-verify: no
	// SHA256 was computed and the remote file was not read back.
	NoVerify bool
}

// SidecarName returns the completion note file name for an archive, e.g.
// "z.7z" -> "z.7z.txt".
func SidecarName(name string) string {
	return name + ".txt"
}

// Render formats the record as human readable UTF-8 text. Every line uses
// "key: value" so the file can also be parsed again for idempotency checks.
func (r Record) Render() string {
	var b strings.Builder
	b.WriteString("# NASArchiveSync 上传记录\n")
	writeLine(&b, KeyFileName, r.FileName)
	writeLine(&b, KeyTargetPath, r.TargetPath)
	writeLine(&b, KeySize, strconv.FormatInt(r.Size, 10))
	if r.NoVerify {
		writeLine(&b, KeyVerifyMode, VerifyModeNone)
		writeLine(&b, KeySHA256, shaNotComputed)
	} else {
		writeLine(&b, KeySHA256, r.SHA256)
	}
	writeLine(&b, KeyUploadedAt, r.UploadedAt.Format(timeLayout))
	writeLine(&b, KeyVerified, verifiedText(r))
	writeLine(&b, KeySourcePath, r.SourcePath)
	writeLine(&b, KeySourceMTime, r.SourceModTime.Format(timeLayout))
	writeLine(&b, KeyDuration, humanize.Duration(r.Duration))
	writeLine(&b, KeyToolVersion, r.ToolVersion)
	return b.String()
}

func verifiedText(r Record) string {
	if r.NoVerify {
		return "未校验（已关闭校验）"
	}
	if r.Verified {
		return verifiedMessage
	}
	return "未校验"
}

func writeLine(b *strings.Builder, key, value string) {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	fmt.Fprintf(b, "%s: %s\n", key, value)
}

// Parse reads a completion note produced by Render. Only the SHA256 field is
// strictly required; a missing or malformed field results in an error so the
// caller can treat the target as "not verified" and avoid deleting data.
func Parse(data []byte) (*Record, error) {
	rec := &Record{Size: -1}
	seenSHA := false
	verifyMode := "sha256"

	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if i == 0 {
			line = strings.TrimPrefix(line, "\ufeff")
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		switch key {
		case KeyFileName:
			rec.FileName = value
		case KeyTargetPath:
			rec.TargetPath = value
		case KeySize:
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("上传记录中的文件大小无效: %q", value)
			}
			rec.Size = n
		case KeyVerifyMode:
			switch strings.ToLower(value) {
			case "sha256", VerifyModeNone:
				verifyMode = strings.ToLower(value)
			default:
				return nil, fmt.Errorf("上传记录中的校验方式无效: %q", value)
			}
		case KeySHA256:
			sum := strings.ToLower(value)
			if sum == "" || sum == shaNotComputed {
				// Allowed for --no-verify notes; validated below.
				continue
			}
			if len(sum) != 64 {
				return nil, fmt.Errorf("上传记录中的 SHA256 长度无效")
			}
			if _, err := hex.DecodeString(sum); err != nil {
				return nil, fmt.Errorf("上传记录中的 SHA256 格式无效: %w", err)
			}
			rec.SHA256 = sum
			seenSHA = true
		case KeyUploadedAt:
			if t, err := time.Parse(timeLayout, value); err == nil {
				rec.UploadedAt = t
			}
		case KeyVerified:
			rec.Verified = value == verifiedMessage
		case KeySourcePath:
			rec.SourcePath = value
		case KeySourceMTime:
			if t, err := time.Parse(timeLayout, value); err == nil {
				rec.SourceModTime = t
			}
		case KeyToolVersion:
			rec.ToolVersion = value
		}
	}

	if verifyMode == VerifyModeNone {
		rec.NoVerify = true
	} else if !seenSHA {
		return nil, fmt.Errorf("上传记录缺少 SHA256 字段")
	}
	if rec.FileName == "" && rec.TargetPath != "" {
		rec.FileName = path.Base(strings.ReplaceAll(rec.TargetPath, `\`, "/"))
	}
	return rec, nil
}
