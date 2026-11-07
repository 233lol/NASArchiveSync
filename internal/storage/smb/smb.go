// Package smb implements the storage.Backend interface on top of SMB2/3 using
// github.com/hirochachacha/go-smb2.
package smb

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	smb2 "github.com/hirochachacha/go-smb2"

	"github.com/fjf/NASArchiveSync/internal/storage"
)

type Backend struct {
	conn    net.Conn
	session *smb2.Session
	share   *smb2.Share
	host    string
	shareTo string
	root    string
	target  storage.Target
	opt     storage.Options
}

// Open connects to the SMB share described by t, authenticates and optionally
// creates the target root directory.
func Open(t storage.Target, opt storage.Options) (storage.Backend, error) {
	port := t.Port
	if port == 0 {
		port = opt.Port
	}
	if port == 0 {
		port = 445
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	addr := net.JoinHostPort(t.Host, strconv.Itoa(port))
	rawConn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("连接 SMB 服务器 %s 失败: %w", addr, err)
	}
	// Apply the connection timeout during authentication, then switch to the
	// longer idle limit for transfers.
	conn := storage.NewIdleTimeoutConn(rawConn, timeout)
	// Bound the authentication and tree connect phases; long uploads must not be
	// affected once the deadline is cleared below.
	_ = conn.SetDeadline(time.Now().Add(timeout))

	// Accept "DOMAIN\\user" (or "domain/user") as a convenience.
	user := opt.User
	domain := opt.Domain
	if domain == "" {
		if i := strings.LastIndexAny(user, `\/`); i > 0 && i < len(user)-1 {
			domain, user = user[:i], user[i+1:]
		}
	}

	dialer := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     user,
			Password: opt.Password,
			Domain:   domain,
		},
	}
	session, err := dialer.Dial(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("SMB 认证失败（用户 %q）: %w", opt.User, err)
	}

	share, err := session.Mount(t.Share)
	if err != nil {
		_ = session.Logoff()
		_ = conn.Close()
		return nil, fmt.Errorf("挂载共享 %q 失败: %w", t.Share, err)
	}
	_ = conn.SetDeadline(time.Time{})
	conn.SetIdleTimeout(opt.IOTimeout)

	b := &Backend{
		conn:    conn,
		session: session,
		share:   share,
		host:    t.Host,
		shareTo: t.Share,
		root:    strings.Trim(t.Path, "/"),
		target:  t,
		opt:     opt,
	}

	if opt.CreateRoot && b.root != "" {
		if err := b.MkdirAll(""); err != nil {
			_ = b.Close()
			return nil, fmt.Errorf("创建目标目录 %s 失败: %w", b.Location(""), err)
		}
	}

	return b, nil
}

func (b *Backend) Kind() string { return "smb" }

// RenameReplaces reports that SMB2 rename does not replace an existing target
// (ReplaceIfExists is not set by the client library).
func (b *Backend) RenameReplaces() bool { return false }

// CaseInsensitive reports that SMB shares are normally matched without regard
// to case.
func (b *Backend) CaseInsensitive() bool { return true }

func (b *Backend) Root() string {
	p := `\\` + b.host + `\` + b.shareTo
	if b.root != "" {
		p += `\` + strings.ReplaceAll(b.root, "/", `\`)
	}
	return p
}

func (b *Backend) Location(rel string) string {
	clean, err := storage.CleanRel(rel)
	if err != nil {
		clean = ""
	}
	p := b.Root()
	if clean != "" {
		p += `\` + strings.ReplaceAll(clean, "/", `\`)
	}
	return p
}

func (b *Backend) MkdirAll(rel string) error {
	p, err := b.resolve(rel)
	if err != nil {
		return err
	}
	if p == "" {
		return nil
	}
	return b.share.MkdirAll(p, 0o755)
}

func (b *Backend) Stat(rel string) (storage.FileInfo, error) {
	p, err := b.resolve(rel)
	if err != nil {
		return storage.FileInfo{}, err
	}
	fi, err := b.share.Stat(p)
	if err != nil {
		return storage.FileInfo{}, err
	}
	return storage.FileInfo{
		Name:    fi.Name(),
		Size:    fi.Size(),
		IsDir:   fi.IsDir(),
		ModTime: fi.ModTime(),
	}, nil
}

func (b *Backend) Open(rel string) (io.ReadCloser, error) {
	p, err := b.resolve(rel)
	if err != nil {
		return nil, err
	}
	return b.share.Open(p)
}

// OpenRange opens a fresh handle and reads a byte range from it, which lets the
// verifier fetch large files over several connections.
func (b *Backend) OpenRange(rel string, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("无效的 SMB 读取范围: offset=%d length=%d", offset, length)
	}
	p, err := b.resolve(rel)
	if err != nil {
		return nil, err
	}
	f, err := b.share.Open(p)
	if err != nil {
		return nil, err
	}
	return storage.NewRangeReadCloser(f, offset, length, f), nil
}

// List returns the immediate children of a shared directory.
func (b *Backend) List(rel string) ([]storage.FileInfo, error) {
	p, err := b.resolve(rel)
	if err != nil {
		return nil, err
	}
	entries, err := b.share.ReadDir(p)
	if err != nil {
		return nil, err
	}
	out := make([]storage.FileInfo, 0, len(entries))
	for _, info := range entries {
		out = append(out, storage.FileInfo{
			Name:    info.Name(),
			Size:    info.Size(),
			IsDir:   info.IsDir(),
			ModTime: info.ModTime(),
		})
	}
	return out, nil
}

func (b *Backend) Create(rel string) (storage.Writer, error) {
	p, err := b.resolve(rel)
	if err != nil {
		return nil, err
	}
	return b.share.Create(p)
}

func (b *Backend) Remove(rel string) error {
	p, err := b.resolve(rel)
	if err != nil {
		return err
	}
	if p == "" {
		return errors.New("拒绝删除共享根目录")
	}
	return b.share.Remove(p)
}

func (b *Backend) Rename(oldRel, newRel string) error {
	oldPath, err := b.resolve(oldRel)
	if err != nil {
		return err
	}
	newPath, err := b.resolve(newRel)
	if err != nil {
		return err
	}
	return b.share.Rename(oldPath, newPath)
}

func (b *Backend) Close() error {
	var firstErr error
	if b.share != nil {
		if err := b.share.Umount(); err != nil {
			firstErr = err
		}
		b.share = nil
	}
	if b.session != nil {
		if err := b.session.Logoff(); err != nil && firstErr == nil {
			firstErr = err
		}
		b.session = nil
	}
	if b.conn != nil {
		if err := b.conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		b.conn = nil
	}
	return firstErr
}

// Reconnect rebuilds the SMB session after a transient network failure.
func (b *Backend) Reconnect() error {
	target, opt := b.target, b.opt
	_ = b.Close()
	next, err := Open(target, opt)
	if err != nil {
		return err
	}
	other, ok := next.(*Backend)
	if !ok {
		_ = next.Close()
		return errors.New("SMB 重连返回了意外的后端类型")
	}
	b.conn, b.session, b.share = other.conn, other.session, other.share
	b.host, b.shareTo, b.root = other.host, other.shareTo, other.root
	b.target, b.opt = other.target, other.opt
	other.conn, other.session, other.share = nil, nil, nil
	return nil
}

func (b *Backend) resolve(rel string) (string, error) {
	clean, err := storage.CleanRel(rel)
	if err != nil {
		return "", err
	}
	return storage.JoinRel(b.root, clean), nil
}

// ensure the shared file type satisfies storage.Writer at compile time.
var _ storage.Writer = (*smb2.File)(nil)
