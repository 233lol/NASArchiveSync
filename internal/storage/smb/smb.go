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
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("连接 SMB 服务器 %s 失败: %w", addr, err)
	}
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

	b := &Backend{
		conn:    conn,
		session: session,
		share:   share,
		host:    t.Host,
		shareTo: t.Share,
		root:    strings.Trim(t.Path, "/"),
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

func (b *Backend) resolve(rel string) (string, error) {
	clean, err := storage.CleanRel(rel)
	if err != nil {
		return "", err
	}
	return storage.JoinRel(b.root, clean), nil
}

// ensure the shared file type satisfies storage.Writer at compile time.
var _ storage.Writer = (*smb2.File)(nil)
