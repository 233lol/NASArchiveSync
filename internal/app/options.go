package app

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/fjf/NASArchiveSync/internal/xlog"
)

// SMBOptions carries SMB credentials.
type SMBOptions struct {
	User     string
	Password string
	Domain   string
	Port     int
}

// SFTPOptions carries SSH/SFTP credentials and tuning.
type SFTPOptions struct {
	User           string
	Password       string
	KeyPath        string
	KeyPassphrase  string
	Port           int
	Cipher         string
	KnownHostsPath string
	Insecure       bool
}

// S3Options carries S3 credentials and endpoint settings.
type S3Options struct {
	Endpoint     string
	Region       string
	AccessKey    string
	SecretKey    string
	SessionToken string
	Style        string
	Insecure     bool
	PartSize     int64
	Concurrency  int
}

// WebDAVOptions carries WebDAV credentials. The server address and path are
// part of --target.
type WebDAVOptions struct {
	User     string
	Password string
	Insecure bool
}

// Options configures a Run.
type Options struct {
	Source string
	Target string
	Depth  int
	// WithSourceDir uploads below a folder named after the source directory
	// instead of directly into the target root. For example source C:\game\a
	// and target \\nas\share\game produces \\nas\share\game\a\....
	WithSourceDir bool
	// List only prints the matched archive files. No target is required and no
	// storage backend is opened or modified.
	List       bool
	Include    []string
	Exclude    []string
	Extensions []string
	DryRun     bool
	KeepSource bool
	Force      bool
	Recheck    bool
	// Pipeline overlaps the upload of the next file with the verification of
	// the previous one. The CLI enables it by default.
	Pipeline bool
	// NoVerify disables SHA256 entirely: the archive is uploaded without
	// hashing and is not read back. Completion notes are marked as unverified.
	NoVerify bool
	// VerifyConcurrency is the number of parallel range reads used when a
	// backend supports them. Values below 2 read the remote file sequentially.
	VerifyConcurrency int
	// Concurrency is the number of files processed at the same time. Values
	// above 1 disable the upload/verify pipeline and use one backend per worker.
	Concurrency int
	// VerifyOnly checks existing remote files without uploading, writing
	// records or deleting anything.
	VerifyOnly bool
	// CleanTemp removes stale NASArchiveSync temporary files below the target.
	CleanTemp bool
	// Timeout bounds connection setup and handshakes.
	Timeout time.Duration
	// IOTimeout aborts a read or write that makes no progress for this long.
	// Zero disables the idle deadlines.
	IOTimeout time.Duration
	// Retries is the number of extra attempts for transient failures.
	Retries    int
	RetryDelay time.Duration
	// LimitBytesPerSec caps upload and remote verification bandwidth. Zero
	// means unlimited.
	LimitBytesPerSec int64
	CaseSensitive    bool
	SMB              SMBOptions
	SFTP             SFTPOptions
	S3               S3Options
	WebDAV           WebDAVOptions
	// SMBMode is "auto" (default), "native" or "builtin". Native means the
	// operating system's SMB redirector is used and existing Windows
	// credentials are reused.
	SMBMode          string
	SMBAuthRequested bool
	Txt              TxtMode
	NoProgress       bool
	Version          string
	Logger           *xlog.Logger
	ProgressOut      io.Writer
	ListOut          io.Writer
	Now              func() time.Time

	// sourcePrefix is the source directory name used when WithSourceDir is set.
	// It is derived in Run and prefixes every remote relative path.
	sourcePrefix string
	// verifyPool reuses range-read buffers across files in one run.
	verifyPool *chunkPool
	// limiter throttles upload and remote verification reads.
	limiter *bandwidthLimiter
}

// TxtMode controls where the completion note (txt) is written.
type TxtMode string

const (
	TxtBoth   TxtMode = "both"
	TxtLocal  TxtMode = "local"
	TxtRemote TxtMode = "remote"
	TxtNone   TxtMode = "none"
)

// ParseTxtMode validates a --txt value. An empty value means "both".
func ParseTxtMode(s string) (TxtMode, error) {
	switch TxtMode(strings.ToLower(strings.TrimSpace(s))) {
	case "", TxtBoth:
		return TxtBoth, nil
	case TxtLocal:
		return TxtLocal, nil
	case TxtRemote:
		return TxtRemote, nil
	case TxtNone:
		return TxtNone, nil
	default:
		return "", fmt.Errorf("上传记录保存方式无效: %q（可选 both、local、remote、none）", s)
	}
}

func (m TxtMode) WritesLocal() bool  { return m == TxtBoth || m == TxtLocal }
func (m TxtMode) WritesRemote() bool { return m == TxtBoth || m == TxtRemote }

// Validate checks the mode combinations shared by the CLI and library callers.
func (o Options) Validate() error {
	return ValidateFlags(o.List, o.VerifyOnly, o.CleanTemp, o.DryRun, o.NoVerify, o.Recheck)
}

// ValidateFlags reports incompatible mode combinations. Keeping the messages in
// one place stops the CLI and app.Run from drifting apart.
func ValidateFlags(list, verifyOnly, cleanTemp, dryRun, noVerify, recheck bool) error {
	switch {
	case noVerify && recheck:
		return fmt.Errorf("--recheck 需要读取远端并计算 SHA256，不能与 --no-verify 同时使用")
	case verifyOnly && noVerify:
		return fmt.Errorf("--verify-only 需要读取并计算 SHA256，不能与 --no-verify 同时使用")
	case verifyOnly && dryRun:
		return fmt.Errorf("--verify-only 本身就是只读检查，不能与 --dry-run 同时使用")
	case cleanTemp && dryRun:
		return fmt.Errorf("--clean-temp 会删除文件，不能与 --dry-run 同时使用")
	case list && (cleanTemp || verifyOnly):
		return fmt.Errorf("--list 只列出文件，不能与 --clean-temp 或 --verify-only 同时使用")
	}
	return nil
}
