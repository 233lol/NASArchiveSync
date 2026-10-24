// Package storage defines the protocol independent backend abstraction used by
// NASArchiveSync. Concrete implementations live in subpackages so that new
// protocols (sftp, ftp, nfs, ...) can be added without touching the pipeline.
package storage

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FileInfo is a minimal, protocol independent view of a remote file.
type FileInfo struct {
	Name    string
	Size    int64
	IsDir   bool
	ModTime time.Time
}

// Writer is a remote file opened for writing. Sync must flush the data to the
// server before Close is called.
type Writer interface {
	io.Writer
	Sync() error
	Close() error
}

// Backend abstracts a remote directory tree. All paths are slash separated and
// relative to the backend root; the empty string refers to the root itself.
type Backend interface {
	// Kind returns a short protocol name such as "smb" or "local".
	Kind() string
	// Root returns the backend root in its native form.
	Root() string
	// Location returns a human readable absolute path for rel.
	Location(rel string) string
	MkdirAll(rel string) error
	Stat(rel string) (FileInfo, error)
	Open(rel string) (io.ReadCloser, error)
	Create(rel string) (Writer, error)
	Remove(rel string) error
	Rename(oldRel, newRel string) error
	Close() error
}

// Options carries credentials and connection settings shared by all backends.
type Options struct {
	User     string
	Password string
	Domain   string
	Port     int
	Timeout  time.Duration
	// CreateRoot asks the backend to create the target root when missing. It is
	// false for dry runs so that a preview never changes the remote side.
	CreateRoot bool

	// SFTP specific settings. They are ignored by other backends.
	KeyPath         string
	KeyPassphrase   string
	Cipher          string
	KnownHostsPath  string
	InsecureHostKey bool
}

// Target is a parsed --target value.
type Target struct {
	Scheme   string // "smb", "sftp" or "local"
	User     string
	Password string
	Host     string
	Port     int
	Share    string
	// Path is slash separated. SMB paths are relative to the share; SFTP and
	// local paths are absolute and keep their leading slash.
	Path string
	Raw  string
}

// UNCPath returns the \\host\share\path form of an SMB target. It is used by
// the native backend, which lets the operating system's SMB redirector handle
// the connection and reuse credentials that are already available.
func (t Target) UNCPath() string {
	if t.Scheme != "smb" {
		return t.Path
	}
	p := `\\` + t.Host + `\` + t.Share
	if t.Path != "" {
		p += `\` + strings.ReplaceAll(t.Path, "/", `\`)
	}
	return p
}

// ParseTarget understands the following forms:
//
//	\\192.168.12.1\mnt\hdd1\game      UNC path (smb)
//	//192.168.12.1/mnt/hdd1/game      UNC path with forward slashes (smb)
//	smb://192.168.12.1/mnt/hdd1/game  SMB URL
//	smb://host:1445/mnt/hdd1/game     SMB URL with explicit port
//	sftp://user@host/mnt/hdd1/game    SFTP URL
//	sftp://user@host:2222/mnt/game    SFTP URL with explicit port
//	scp://user@host/mnt/hdd1/game     alias for SFTP (scp uses the SFTP subsystem)
//	user@host:/mnt/hdd1/game          scp style absolute path (SFTP)
//	192.168.12.1:/mnt/hdd1/game       scp style absolute path (SFTP)
//	local:C:\game                     local directory (mainly for testing)
//	local:///mnt/game                 local directory with URL syntax
//	C:\game                           plain local path
func ParseTarget(raw string) (Target, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Target{}, errors.New("目标路径不能为空")
	}

	t := Target{Raw: raw}

	// Normalise the extended UNC prefix.
	if strings.HasPrefix(s, `\\?\UNC\`) {
		s = `\\` + s[len(`\\?\UNC\`):]
	}

	// UNC or //host/share/... paths.
	if strings.HasPrefix(s, `\\`) || strings.HasPrefix(s, `//`) {
		parts := splitPath(s)
		if len(parts) < 2 {
			return Target{}, fmt.Errorf("SMB 路径需要包含共享名，例如 \\\\host\\share\\dir: %s", raw)
		}
		t.Scheme = "smb"
		t.Host = parts[0]
		t.Share = parts[1]
		t.Path = strings.Join(parts[2:], "/")
		return t, nil
	}

	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "smb://"):
		u, err := url.Parse(s)
		if err != nil {
			return Target{}, fmt.Errorf("解析 SMB 地址失败: %w", err)
		}
		if u.Hostname() == "" {
			return Target{}, fmt.Errorf("SMB 地址缺少主机名: %s", raw)
		}
		port := 0
		if p := u.Port(); p != "" {
			port, err = strconv.Atoi(p)
			if err != nil || port <= 0 || port > 65535 {
				return Target{}, fmt.Errorf("SMB 端口无效: %s", p)
			}
		}
		parts := splitPath(u.Path)
		if len(parts) < 1 {
			return Target{}, fmt.Errorf("SMB 地址缺少共享名: %s", raw)
		}
		t.Scheme = "smb"
		t.User = urlUsername(u)
		t.Host = u.Hostname()
		t.Port = port
		t.Share = parts[0]
		t.Path = strings.Join(parts[1:], "/")
		return t, nil

	case strings.HasPrefix(lower, "sftp://"), strings.HasPrefix(lower, "scp://"):
		u, err := url.Parse(s)
		if err != nil {
			return Target{}, fmt.Errorf("解析 SFTP 地址失败: %w", err)
		}
		if u.Hostname() == "" {
			return Target{}, fmt.Errorf("SFTP 地址缺少主机名: %s", raw)
		}
		port := 0
		if p := u.Port(); p != "" {
			port, err = strconv.Atoi(p)
			if err != nil || port <= 0 || port > 65535 {
				return Target{}, fmt.Errorf("SFTP 端口无效: %s", p)
			}
		}
		t.Scheme = "sftp"
		t.User = urlUsername(u)
		t.Password = urlPassword(u)
		t.Host = u.Hostname()
		t.Port = port
		t.Path = cleanRemotePath(u.Path)
		return t, nil

	case strings.HasPrefix(lower, "local:"):
		p := s[len("local:"):]
		p = strings.TrimPrefix(p, "//")
		// local:///C:/dir leaves a leading slash in front of the drive letter.
		if len(p) >= 3 && p[0] == '/' && isDriveLetter(p[1]) && p[2] == ':' {
			p = p[1:]
		}
		if strings.TrimSpace(p) == "" {
			return Target{}, fmt.Errorf("本地目标路径不能为空: %s", raw)
		}
		t.Scheme = "local"
		t.Path = p
		return t, nil
	}

	// scp style "host:/absolute/path". Requiring an absolute path keeps
	// Windows drive letters such as C:\game and C:/game local.
	if !strings.Contains(s, "://") {
		if scpTarget, ok := parseScpLike(s); ok {
			return scpTarget, nil
		}
	}

	if scheme, ok := unknownScheme(s); ok {
		switch scheme {
		case "sftp", "scp":
			return Target{}, fmt.Errorf("SFTP 地址格式无效，请使用 sftp://host/path、scp://host/path 或 host:/path: %s", raw)
		case "smb":
			return Target{}, fmt.Errorf("SMB 地址格式无效，请使用 smb://host/share/path 或 \\\\host\\share\\path: %s", raw)
		}
		return Target{}, fmt.Errorf("暂不支持协议 %q，目前支持 smb、sftp 与 local", scheme)
	}

	// Everything else is treated as a local file system path.
	t.Scheme = "local"
	t.Path = s
	return t, nil
}

// parseScpLike recognises the scp shorthand "user@host:/absolute/path" (the
// user part is optional). scp style targets use the SFTP subsystem, which is
// what modern OpenSSH scp does as well.
func parseScpLike(s string) (Target, bool) {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return Target{}, false
	}
	head, rest := s[:i], s[i+1:]
	if strings.ContainsAny(head, `/\`) {
		return Target{}, false
	}
	// A single letter in front of a colon is a Windows drive, not a host.
	if len(head) == 1 && isDriveLetter(head[0]) {
		return Target{}, false
	}
	switch strings.ToLower(head) {
	case "smb", "sftp", "scp", "local", "file", "http", "https", "ftp", "ftps", "nfs":
		return Target{}, false
	}
	if !strings.HasPrefix(rest, "/") {
		return Target{}, false
	}

	user, host := "", head
	if j := strings.LastIndexByte(head, '@'); j >= 0 {
		user, host = head[:j], head[j+1:]
	}
	if host == "" {
		return Target{}, false
	}

	return Target{
		Scheme: "sftp",
		User:   user,
		Host:   host,
		Path:   cleanRemotePath(rest),
		Raw:    s,
	}, true
}

func cleanRemotePath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

func urlUsername(u *url.URL) string {
	if u.User == nil {
		return ""
	}
	return u.User.Username()
}

func urlPassword(u *url.URL) string {
	if u.User == nil {
		return ""
	}
	pw, _ := u.User.Password()
	return pw
}

func splitPath(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool {
		return r == '/' || r == '\\'
	})
}

func isDriveLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// unknownScheme detects strings like "ftp://host" or "sftp:host" while leaving
// Windows drive letters alone.
func unknownScheme(s string) (string, bool) {
	if len(s) >= 2 && s[1] == ':' && isDriveLetter(s[0]) {
		return "", false
	}
	if i := strings.Index(s, "://"); i > 0 {
		return strings.ToLower(s[:i]), true
	}
	if i := strings.IndexByte(s, ':'); i > 0 {
		head := s[:i]
		if head != "" && !strings.ContainsAny(head, `\/.`) {
			return strings.ToLower(head), true
		}
	}
	return "", false
}

// CleanRel normalises a relative slash separated path and rejects any attempt
// to escape the backend root.
func CleanRel(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	rel = strings.ReplaceAll(rel, `\`, "/")
	rel = strings.TrimPrefix(rel, "./")
	if rel == "" {
		return "", nil
	}
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("路径不能是绝对路径: %s", rel)
	}
	clean := path.Clean(rel)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("路径不能跳出目标根目录: %s", rel)
	}
	return clean, nil
}

// JoinRel joins two relative slash separated paths.
func JoinRel(base, rel string) string {
	switch {
	case base == "":
		return rel
	case rel == "":
		return base
	default:
		return base + "/" + rel
	}
}

// LocalPath converts a slash separated relative path to a native path below
// root.
func LocalPath(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}
