// Package sftp implements the storage.Backend interface on top of the SSH
// file transfer protocol using github.com/pkg/sftp.
//
// The same backend also serves scp style targets (host:/path). Modern OpenSSH
// scp uses the SFTP subsystem internally, so there is no separate protocol
// implementation to maintain.
package sftp

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	pkgsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/fjf/NASArchiveSync/internal/storage"
)

// DefaultCipher is used when --sftp-cipher is not set. AES-128-GCM and
// AES-256-GCM measured equally fast on the reference 25 Gbps link; AES-128-GCM
// is chosen for its lower CPU cost.
const DefaultCipher = "aes128-gcm@openssh.com"

// SupportedCiphers lists the ciphers this build accepts. The first entry is the
// default; the rest are fallbacks for servers that do not offer it.
var SupportedCiphers = []string{
	"aes128-gcm@openssh.com",
	"aes256-gcm@openssh.com",
	"chacha20-poly1305@openssh.com",
	"aes128-ctr",
	"aes192-ctr",
	"aes256-ctr",
}

// sftpPacketSize is the maximum SFTP payload. OpenSSH limits a whole SFTP
// message to 256 KiB, so 128 KiB of payload leaves room for the packet header
// while still being four times the 32 KiB protocol default.
const sftpPacketSize = 128 * 1024

// maxConcurrentRequests is the number of in-flight SFTP packets per file.
const maxConcurrentRequests = 64

type Backend struct {
	ssh    *ssh.Client
	client *pkgsftp.Client
	user   string
	host   string
	port   int
	root   string
}

// Open connects to the SSH server described by t, authenticates and optionally
// creates the target root directory.
func Open(t storage.Target, opt storage.Options) (storage.Backend, error) {
	port := t.Port
	if port == 0 {
		port = opt.Port
	}
	if port == 0 {
		port = 22
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	user := t.User
	if user == "" {
		user = opt.User
	}
	if user == "" {
		user = currentUser()
	}
	password := t.Password
	if password == "" {
		password = opt.Password
	}

	auth, err := authMethods(opt, password)
	if err != nil {
		return nil, err
	}
	hostKey, err := hostKeyCallback(opt)
	if err != nil {
		return nil, err
	}
	ciphers, err := cipherList(opt.Cipher)
	if err != nil {
		return nil, err
	}

	addr := net.JoinHostPort(t.Host, strconv.Itoa(port))

	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: hostKey,
		Timeout:         timeout,
	}
	// Ciphers lives on the embedded ssh.Config. Assigning it after the struct
	// literal keeps the module compatible with go1.26.
	cfg.Config.Ciphers = ciphers
	sshClient, err := dialSSH(addr, timeout, cfg)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "no common algorithm") && opt.Cipher != "" && strings.ToLower(opt.Cipher) != "auto" {
			return nil, fmt.Errorf("SFTP 服务器不支持加密算法 %s: %w", opt.Cipher, err)
		}
		if strings.Contains(msg, "knownhosts: key is unknown") {
			return nil, fmt.Errorf("SFTP 主机密钥不在 known_hosts 中（可用 ssh-keyscan -H %s >> ~/.ssh/known_hosts 添加，或加 --sftp-insecure）: %w", t.Host, err)
		}
		return nil, fmt.Errorf("SFTP SSH 握手或认证失败（用户 %q）: %w", user, err)
	}

	client, err := pkgsftp.NewClient(sshClient,
		pkgsftp.MaxPacketUnchecked(sftpPacketSize),
		pkgsftp.UseConcurrentWrites(true),
		pkgsftp.UseConcurrentReads(true),
		pkgsftp.MaxConcurrentRequestsPerFile(maxConcurrentRequests),
	)
	if err != nil {
		_ = sshClient.Close()
		return nil, fmt.Errorf("启动 SFTP 子系统失败: %w", err)
	}

	b := &Backend{
		ssh:    sshClient,
		client: client,
		user:   user,
		host:   t.Host,
		port:   port,
		root:   cleanRoot(t.Path),
	}

	if opt.CreateRoot && b.root != "/" {
		if err := b.MkdirAll(""); err != nil {
			_ = b.Close()
			return nil, fmt.Errorf("创建目标目录 %s 失败: %w", b.Location(""), err)
		}
	}

	return b, nil
}

// hostKeyPreference is used to retry the handshake when the algorithm chosen
// by the default negotiation is not the one recorded in known_hosts. The Go
// SSH client prefers ECDSA, while most known_hosts entries are ED25519.
var hostKeyPreference = []string{
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoECDSA256,
	ssh.KeyAlgoECDSA384,
	ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoRSASHA512,
	ssh.KeyAlgoRSASHA256,
}

func dialSSH(addr string, timeout time.Duration, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	attempts := make([][]string, 0, len(hostKeyPreference)+1)
	attempts = append(attempts, nil) // default negotiation first
	for _, algo := range hostKeyPreference {
		attempts = append(attempts, []string{algo})
	}

	var firstErr error
	for _, algos := range attempts {
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			return nil, fmt.Errorf("连接 SFTP 服务器 %s 失败: %w", addr, err)
		}
		// Bound the handshake and authentication; uploads must not inherit
		// this deadline once the session is established.
		_ = conn.SetDeadline(time.Now().Add(timeout))
		cfg.HostKeyAlgorithms = algos
		cc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
		if err == nil {
			_ = conn.SetDeadline(time.Time{})
			return ssh.NewClient(cc, chans, reqs), nil
		}
		_ = conn.Close()
		if firstErr == nil {
			firstErr = err
		}
		if !retryableHostKeyError(err) {
			return nil, err
		}
	}
	return nil, firstErr
}

func retryableHostKeyError(err error) bool {
	var keyErr *knownhosts.KeyError
	if errors.As(err, &keyErr) {
		// An empty Want list means the host is entirely unknown; trying other
		// key algorithms cannot help.
		return len(keyErr.Want) > 0
	}
	msg := err.Error()
	return strings.Contains(msg, "knownhosts:") || strings.Contains(msg, "no common algorithm")
}

func cleanRoot(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

func (b *Backend) Kind() string { return "sftp" }

// endpoint renders host[:port] for logs. Port 22 is omitted so the common case
// stays identical to scp notation.
func (b *Backend) endpoint() string {
	if b.port == 22 {
		return b.host
	}
	return net.JoinHostPort(b.host, strconv.Itoa(b.port))
}

func (b *Backend) Root() string {
	return fmt.Sprintf("%s@%s:%s", b.user, b.endpoint(), b.root)
}

func (b *Backend) Location(rel string) string {
	clean, err := storage.CleanRel(rel)
	if err != nil {
		clean = ""
	}
	if clean == "" {
		return b.Root()
	}
	return fmt.Sprintf("%s@%s:%s", b.user, b.endpoint(), path.Join(b.root, clean))
}

func (b *Backend) MkdirAll(rel string) error {
	p, err := b.resolve(rel)
	if err != nil {
		return err
	}
	return b.client.MkdirAll(p)
}

func (b *Backend) Stat(rel string) (storage.FileInfo, error) {
	p, err := b.resolve(rel)
	if err != nil {
		return storage.FileInfo{}, err
	}
	fi, err := b.client.Stat(p)
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
	return b.client.Open(p)
}

func (b *Backend) Create(rel string) (storage.Writer, error) {
	p, err := b.resolve(rel)
	if err != nil {
		return nil, err
	}
	return b.client.Create(p)
}

func (b *Backend) Remove(rel string) error {
	p, err := b.resolve(rel)
	if err != nil {
		return err
	}
	if p == "/" {
		return errors.New("拒绝删除目标根目录")
	}
	return b.client.Remove(p)
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
	// posix-rename@openssh.com atomically replaces an existing destination and
	// is what scp/sftp rely on. Fall back to the plain SFTP v3 rename for
	// servers that do not advertise the extension.
	if err := b.client.PosixRename(oldPath, newPath); err == nil {
		return nil
	}
	return b.client.Rename(oldPath, newPath)
}

func (b *Backend) Close() error {
	var firstErr error
	if b.client != nil {
		if err := b.client.Close(); err != nil {
			firstErr = err
		}
		b.client = nil
	}
	if b.ssh != nil {
		if err := b.ssh.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		b.ssh = nil
	}
	return firstErr
}

func (b *Backend) resolve(rel string) (string, error) {
	clean, err := storage.CleanRel(rel)
	if err != nil {
		return "", err
	}
	if clean == "" {
		return b.root, nil
	}
	return path.Join(b.root, clean), nil
}

func cipherList(requested string) ([]string, error) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	switch requested {
	case "", "auto":
		// Prefer the measured default, then the remaining supported ciphers so
		// that servers without AES-GCM still connect.
		return append([]string(nil), SupportedCiphers...), nil
	}
	for _, c := range SupportedCiphers {
		if requested == c {
			return []string{requested}, nil
		}
	}
	return nil, fmt.Errorf("不支持的 SFTP 加密算法 %q（可选: %s）", requested, strings.Join(SupportedCiphers, ", "))
}

func currentUser() string {
	for _, key := range []string{"USER", "USERNAME", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	if u, err := os.UserHomeDir(); err == nil {
		return filepath.Base(u)
	}
	return ""
}

func hostKeyCallback(opt storage.Options) (ssh.HostKeyCallback, error) {
	if opt.InsecureHostKey {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	p := expandHome(opt.KnownHostsPath)
	if p == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("无法定位 known_hosts: %w（可用 --sftp-insecure 跳过主机密钥校验）", err)
		}
		p = filepath.Join(home, ".ssh", "known_hosts")
	}
	cb, err := knownhosts.New(p)
	if err == nil {
		return cb, nil
	}

	// OpenSSH warns about and skips malformed known_hosts lines, while
	// x/crypto/ssh/knownhosts rejects the whole file. Retry with only the
	// entries that parse so one broken line does not block every connection.
	clean, cleanErr := sanitizeKnownHosts(p)
	if cleanErr == nil {
		cb, cleanErr = knownhosts.New(clean)
		_ = os.Remove(clean)
		if cleanErr == nil {
			return cb, nil
		}
	}
	return nil, fmt.Errorf("加载 known_hosts 失败: %w（可用 --sftp-insecure 跳过主机密钥校验，或用 --sftp-known-hosts 指定文件）", err)
}

// sanitizeKnownHosts writes a temporary copy of p that only contains lines
// which parse as a known_hosts entry. The caller must remove the returned file.
func sanitizeKnownHosts(p string) (string, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		idx := 0
		if strings.HasPrefix(fields[0], "@") {
			idx = 1
		}
		if len(fields) < idx+3 {
			continue
		}
		key, err := base64.StdEncoding.DecodeString(fields[idx+2])
		if err != nil {
			continue
		}
		if _, err := ssh.ParsePublicKey(key); err != nil {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return "", errors.New("known_hosts 中没有可用条目")
	}

	f, err := os.CreateTemp("", "nassync-known-hosts-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

func authMethods(opt storage.Options, password string) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	var keyErr error

	keyPath := expandHome(strings.TrimSpace(opt.KeyPath))
	explicitKey := keyPath != ""
	if keyPath == "" {
		keyPath = defaultKeyPath()
	}
	if keyPath != "" {
		signer, err := loadPrivateKey(keyPath, opt.KeyPassphrase)
		if err != nil {
			if explicitKey {
				return nil, err
			}
			keyErr = err
		} else {
			methods = append(methods, ssh.PublicKeys(signer))
		}
	}

	if password != "" {
		methods = append(methods, ssh.Password(password))
		// Some servers only advertise keyboard-interactive.
		methods = append(methods, ssh.KeyboardInteractive(func(name, instruction string, questions []string, echos []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i := range answers {
				answers[i] = password
			}
			return answers, nil
		}))
	}

	if len(methods) == 0 {
		if keyErr != nil {
			return nil, fmt.Errorf("没有可用的 SFTP 认证方式（%v）；请提供 --sftp-key、--sftp-pass 或 --sftp-ask-pass", keyErr)
		}
		return nil, errors.New("缺少 SFTP 认证信息：请提供 --sftp-key、--sftp-pass 或 --sftp-ask-pass")
	}
	return methods, nil
}

// expandHome expands a leading ~ or ~/ (or ~\) to the user's home directory.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

func defaultKeyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		p := filepath.Join(home, ".ssh", name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

func loadPrivateKey(p, passphrase string) (ssh.Signer, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("读取 SFTP 私钥失败: %w", err)
	}
	if passphrase != "" {
		// A passphrase may also be configured globally while this particular
		// key is unencrypted, so fall through to the plain parser below.
		if signer, err := ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase)); err == nil {
			return signer, nil
		}
	}

	signer, err := ssh.ParsePrivateKey(data)
	if err == nil {
		return signer, nil
	}
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		if passphrase != "" {
			return nil, fmt.Errorf("SFTP 私钥 %s 的口令不正确", p)
		}
		return nil, fmt.Errorf("SFTP 私钥 %s 已加密，请用 --sftp-key-pass 或 NAS_SFTP_KEY_PASS 提供口令", p)
	}
	return nil, fmt.Errorf("解析 SFTP 私钥 %s 失败: %w", p, err)
}

var _ storage.Writer = (*pkgsftp.File)(nil)
