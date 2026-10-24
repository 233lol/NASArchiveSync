// Package webdav implements the storage.Backend interface on top of the
// WebDAV protocol (RFC 4918). It speaks plain HTTP or HTTPS and therefore also
// works with WebDAV frontends such as OpenList/AList, Nextcloud and Apache
// mod_dav.
package webdav

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fjf/NASArchiveSync/internal/storage"
)

const (
	defaultTimeout = 30 * time.Second
	// responseHeaderTimeout applies after the request body has been written, so
	// it does not limit how long a large upload may take.
	responseHeaderTimeout = 2 * time.Minute
	maxErrorBody          = 4 << 10
	maxPropfindBody       = 2 << 20

	methodMKCOL    = "MKCOL"
	methodPROPFIND = "PROPFIND"
	methodMOVE     = "MOVE"
	methodCOPY     = "COPY"
)

const propfindBody = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:">
  <d:prop>
    <d:resourcetype/>
    <d:getcontentlength/>
    <d:getlastmodified/>
  </d:prop>
</d:propfind>`

// Backend stores files under a directory on a WebDAV server.
type Backend struct {
	http     *http.Client
	base     *url.URL // http(s)://host[:port]
	display  string   // webdav or webdavs
	rootPath string
	user     string
	pass     string
	hasAuth  bool

	mkdirMu sync.Mutex
	created map[string]bool
}

// Open connects to the WebDAV server described by t and verifies that the
// target directory is reachable (or creates it when allowed).
func Open(t storage.Target, opt storage.Options) (storage.Backend, error) {
	if strings.TrimSpace(t.Host) == "" {
		return nil, errors.New("WebDAV 地址缺少主机名")
	}

	secure := t.TLS
	scheme := "http"
	displayScheme := "webdav"
	if secure {
		scheme = "https"
		displayScheme = "webdavs"
	}

	port := t.Port
	if port == 0 {
		if secure {
			port = 443
		} else {
			port = 80
		}
	}
	hostPort := t.Host
	if strings.Contains(t.Host, ":") || (secure && port != 443) || (!secure && port != 80) {
		hostPort = net.JoinHostPort(t.Host, strconv.Itoa(port))
	}

	user := t.User
	if user == "" {
		user = opt.User
	}
	pass := t.Password
	if pass == "" {
		pass = opt.Password
	}

	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	client := newHTTPClient(timeout, opt.WebDAVInsecure)

	b := &Backend{
		http:     client,
		base:     &url.URL{Scheme: scheme, Host: hostPort},
		display:  displayScheme,
		rootPath: cleanPath(t.Path),
		user:     user,
		pass:     pass,
		hasAuth:  user != "" || pass != "",
		created:  make(map[string]bool),
	}

	info, err := b.Stat("")
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			_ = b.Close()
			return nil, err
		}
		if !opt.CreateRoot {
			return b, nil
		}
		if err := b.MkdirAll(""); err != nil {
			_ = b.Close()
			return nil, fmt.Errorf("创建 WebDAV 目标目录 %s 失败: %w", b.Root(), err)
		}
		return b, nil
	}
	if !info.IsDir {
		_ = b.Close()
		return nil, fmt.Errorf("WebDAV 目标不是目录: %s", b.Root())
	}
	b.cacheAncestors()
	return b, nil
}

// cacheAncestors marks the already existing target and its parents so later
// MkdirAll calls only create the new subdirectories.
func (b *Backend) cacheAncestors() {
	b.mkdirMu.Lock()
	defer b.mkdirMu.Unlock()
	for p := b.rootPath; ; p = path.Dir(p) {
		b.created[p] = true
		if p == "/" {
			break
		}
	}
}

func newHTTPClient(timeout time.Duration, insecure bool) *http.Client {
	var tr *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	} else {
		tr = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	tr.DialContext = (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext
	tr.MaxIdleConns = 64
	tr.MaxIdleConnsPerHost = 32
	tr.MaxConnsPerHost = 32
	tr.IdleConnTimeout = 90 * time.Second
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.ResponseHeaderTimeout = responseHeaderTimeout
	tr.ExpectContinueTimeout = time.Second
	tr.WriteBufferSize = 64 << 10
	tr.ReadBufferSize = 64 << 10
	if insecure {
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{}
		} else {
			tr.TLSClientConfig = tr.TLSClientConfig.Clone()
		}
		tr.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // explicit opt-in
	}
	return &http.Client{
		Transport: tr,
		// Redirects would repeat a PUT/MOVE body or leak credentials to
		// another host. Report them instead so the user can use the final URL.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func cleanPath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, `\`, "/"))
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

func (b *Backend) Kind() string {
	if b.base.Scheme == "https" {
		return "webdavs"
	}
	return "webdav"
}

func (b *Backend) Root() string {
	return b.displayURL(b.rootPath)
}

func (b *Backend) Location(rel string) string {
	clean, err := storage.CleanRel(rel)
	if err != nil || clean == "" {
		return b.Root()
	}
	return b.displayURL(path.Join(b.rootPath, clean))
}

func (b *Backend) displayURL(p string) string {
	auth := ""
	if b.user != "" {
		auth = url.User(b.user).String() + "@"
	}
	return b.display + "://" + auth + b.base.Host + p
}

func (b *Backend) urlFor(p string) string {
	u := &url.URL{Scheme: b.base.Scheme, Host: b.base.Host, Path: p}
	return u.String()
}

func (b *Backend) MkdirAll(rel string) error {
	clean, err := storage.CleanRel(rel)
	if err != nil {
		return err
	}
	target := b.rootPath
	if clean != "" {
		target = path.Join(target, clean)
	}
	return b.mkdirPath(target)
}

// mkdirPath creates every missing component from the server root down to p.
// WebDAV MKCOL never creates parents, so the components are created in order.
func (b *Backend) mkdirPath(p string) error {
	b.mkdirMu.Lock()
	defer b.mkdirMu.Unlock()

	if p == "/" {
		b.created[p] = true
		return nil
	}
	current := ""
	for _, segment := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if segment == "" {
			continue
		}
		current += "/" + segment
		if b.created[current] {
			continue
		}
		status, err := b.mkcol(current)
		if err != nil {
			return err
		}
		// 201/200/204 means created, 405 means the collection already exists.
		// OpenList answers MKCOL /dav with a 307 to /dav/, so mkcol follows
		// that redirect internally.
		if status != http.StatusMethodNotAllowed && (status < 200 || status >= 300) {
			return fmt.Errorf("创建 WebDAV 目录 %s 失败: HTTP %d", b.LocationForPath(current), status)
		}
		b.created[current] = true
	}
	return nil
}

// mkcol sends MKCOL and returns the final status. Redirects to the trailing
// slash form are followed once, because a collection URL without the slash is
// commonly redirected by WebDAV servers (OpenList's /dav is one example).
func (b *Backend) mkcol(p string) (int, error) {
	resp, err := b.do(methodMKCOL, p, nil, 0)
	if err != nil {
		return 0, fmt.Errorf("创建 WebDAV 目录 %s 失败: %w", b.LocationForPath(p), err)
	}
	if isRedirect(resp.StatusCode) && !strings.HasSuffix(p, "/") {
		_ = drain(resp)
		_ = resp.Body.Close()
		resp, err = b.do(methodMKCOL, p+"/", nil, 0)
		if err != nil {
			return 0, fmt.Errorf("创建 WebDAV 目录 %s 失败: %w", b.LocationForPath(p), err)
		}
	}
	defer resp.Body.Close()
	if isRedirect(resp.StatusCode) {
		return 0, b.statusError("MKCOL", p, resp)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		return 0, b.statusError("MKCOL", p, resp)
	}
	_ = drain(resp)
	return resp.StatusCode, nil
}

func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// propfind follows a directory redirect once, matching how MKCOL handles
// collection URLs without a trailing slash.
func (b *Backend) propfind(p string) (*http.Response, error) {
	resp, err := b.do(methodPROPFIND, p, strings.NewReader(propfindBody), int64(len(propfindBody)))
	if err != nil {
		return nil, err
	}
	if isRedirect(resp.StatusCode) && !strings.HasSuffix(p, "/") {
		_ = drain(resp)
		_ = resp.Body.Close()
		return b.do(methodPROPFIND, p+"/", strings.NewReader(propfindBody), int64(len(propfindBody)))
	}
	return resp, nil
}

func (b *Backend) Stat(rel string) (storage.FileInfo, error) {
	p, err := b.resolve(rel)
	if err != nil {
		return storage.FileInfo{}, err
	}
	resp, err := b.propfind(p)
	if err != nil {
		return storage.FileInfo{}, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotFound:
		return storage.FileInfo{}, notExist("stat", b.LocationForPath(p))
	case http.StatusUnauthorized, http.StatusForbidden:
		return storage.FileInfo{}, b.statusError("PROPFIND", p, resp)
	case http.StatusMethodNotAllowed:
		return storage.FileInfo{}, fmt.Errorf("目标不是 WebDAV 服务（PROPFIND 返回 405）；OpenList/AList 的 WebDAV 地址通常带 /dav: %s", b.LocationForPath(p))
	}
	if resp.StatusCode != http.StatusMultiStatus && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		return storage.FileInfo{}, b.statusError("PROPFIND", p, resp)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPropfindBody))
	if err != nil {
		return storage.FileInfo{}, fmt.Errorf("读取 WebDAV PROPFIND 响应失败: %w", err)
	}
	info, err := parseFileInfo(body, p)
	if errors.Is(err, errPropNotFound) {
		return storage.FileInfo{}, notExist("stat", b.LocationForPath(p))
	}
	if err != nil {
		return storage.FileInfo{}, fmt.Errorf("解析 WebDAV PROPFIND 响应失败: %w", err)
	}
	return info, nil
}

var errPropNotFound = errors.New("WebDAV 资源不存在")

type multistatus struct {
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href      string      `xml:"href"`
	Propstats []davStatus `xml:"propstat"`
}

type davStatus struct {
	Status string  `xml:"status"`
	Prop   davProp `xml:"prop"`
}

type davProp struct {
	ResourceType  davResourceType `xml:"resourcetype"`
	ContentLength string          `xml:"getcontentlength"`
	LastModified  string          `xml:"getlastmodified"`
}

type davResourceType struct {
	Collection *struct{} `xml:"collection"`
}

func parseFileInfo(body []byte, p string) (storage.FileInfo, error) {
	var ms multistatus
	if err := xml.Unmarshal(body, &ms); err != nil {
		return storage.FileInfo{}, err
	}
	if len(ms.Responses) == 0 {
		return storage.FileInfo{}, errors.New("响应中没有文件信息")
	}
	notFound := false
	for _, response := range ms.Responses {
		for _, ps := range response.Propstats {
			if statusCode(ps.Status) == http.StatusNotFound {
				notFound = true
				continue
			}
			if !statusIsOK(ps.Status) {
				continue
			}
			info := storage.FileInfo{
				Name:  path.Base(p),
				IsDir: ps.Prop.ResourceType.Collection != nil,
			}
			if info.Name == "." || info.Name == "/" {
				info.Name = "/"
			}
			if props := strings.TrimSpace(ps.Prop.ContentLength); props != "" {
				size, err := strconv.ParseInt(props, 10, 64)
				if err != nil {
					return storage.FileInfo{}, fmt.Errorf("无效的 getcontentlength %q", props)
				}
				info.Size = size
			}
			if lm := strings.TrimSpace(ps.Prop.LastModified); lm != "" {
				if t, err := http.ParseTime(lm); err == nil {
					info.ModTime = t
				}
			}
			return info, nil
		}
	}
	if notFound {
		return storage.FileInfo{}, errPropNotFound
	}
	return storage.FileInfo{}, errors.New("响应中没有成功的属性")
}

func statusCode(status string) int {
	fields := strings.Fields(status)
	if len(fields) < 2 {
		return 0
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return n
}

func statusIsOK(status string) bool {
	return statusCode(status) == http.StatusOK
}

func (b *Backend) Open(rel string) (io.ReadCloser, error) {
	p, err := b.resolve(rel)
	if err != nil {
		return nil, err
	}
	req, err := b.newRequest(http.MethodGet, p, nil, 0)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("读取 WebDAV 文件 %s 失败: %w", b.LocationForPath(p), err)
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, notExist("open", b.LocationForPath(p))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, b.statusError("GET", p, resp)
	}
	return resp.Body, nil
}

// OpenRange reads length bytes starting at offset with an HTTP Range request.
// Servers that ignore the Range header are rejected so the verifier cannot hash
// the same bytes twice.
func (b *Backend) OpenRange(rel string, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("无效的 WebDAV 读取范围: offset=%d length=%d", offset, length)
	}
	p, err := b.resolve(rel)
	if err != nil {
		return nil, err
	}
	req, err := b.newRequest(http.MethodGet, p, nil, 0)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("读取 WebDAV 文件范围 %s 失败: %w", b.LocationForPath(p), err)
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, notExist("open", b.LocationForPath(p))
	}
	if resp.StatusCode != http.StatusPartialContent {
		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, fmt.Errorf("WebDAV 服务器忽略了 Range 请求: %s", b.LocationForPath(p))
		}
		return nil, b.statusError("GET Range", p, resp)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != length {
		defer resp.Body.Close()
		return nil, fmt.Errorf("WebDAV 服务器返回的范围大小不符（请求 %d 字节，实际 %d）: %s",
			length, resp.ContentLength, b.LocationForPath(p))
	}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		start, end, ok := parseContentRange(cr)
		if !ok || start != offset || end != offset+length-1 {
			defer resp.Body.Close()
			return nil, fmt.Errorf("WebDAV 服务器返回的范围不符（期望 %d-%d，收到 %q）: %s",
				offset, offset+length-1, cr, b.LocationForPath(p))
		}
	}
	return resp.Body, nil
}

func parseContentRange(s string) (int64, int64, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(strings.ToLower(s), "bytes ") {
		return 0, 0, false
	}
	s = strings.TrimSpace(s[len("bytes "):])
	slash := strings.IndexByte(s, '/')
	if slash < 0 {
		return 0, 0, false
	}
	bounds := strings.SplitN(s[:slash], "-", 2)
	if len(bounds) != 2 {
		return 0, 0, false
	}
	start, err1 := strconv.ParseInt(strings.TrimSpace(bounds[0]), 10, 64)
	end, err2 := strconv.ParseInt(strings.TrimSpace(bounds[1]), 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start {
		return 0, 0, false
	}
	return start, end, true
}

func (b *Backend) Create(rel string) (storage.Writer, error) {
	if rel == "" {
		return nil, errors.New("拒绝写入 WebDAV 根目录")
	}
	p, err := b.resolve(rel)
	if err != nil {
		return nil, err
	}
	if p == "/" {
		return nil, errors.New("拒绝写入 WebDAV 根目录")
	}
	return &writer{backend: b, path: p, size: -1, done: make(chan error, 1)}, nil
}

func (b *Backend) Remove(rel string) error {
	if rel == "" {
		return errors.New("拒绝删除 WebDAV 根目录")
	}
	p, err := b.resolve(rel)
	if err != nil {
		return err
	}
	if p == "/" {
		return errors.New("拒绝删除 WebDAV 根目录")
	}
	resp, err := b.do(http.MethodDelete, p, nil, 0)
	if err != nil {
		return fmt.Errorf("删除 WebDAV 文件 %s 失败: %w", b.LocationForPath(p), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return b.statusError("DELETE", p, resp)
	}
	_ = drain(resp)
	// The parent may have been removed from the cache by a concurrent action;
	// clearing the leaf is harmless and keeps the cache from growing forever.
	b.mkdirMu.Lock()
	delete(b.created, p)
	b.mkdirMu.Unlock()
	return nil
}

func (b *Backend) Rename(oldRel, newRel string) error {
	if oldRel == "" || newRel == "" {
		return errors.New("拒绝在 WebDAV 根目录上重命名")
	}
	oldPath, err := b.resolve(oldRel)
	if err != nil {
		return err
	}
	newPath, err := b.resolve(newRel)
	if err != nil {
		return err
	}
	if oldPath == "/" || newPath == "/" {
		return errors.New("拒绝在 WebDAV 根目录上重命名")
	}

	err = b.move(oldPath, newPath)
	if err == nil {
		return nil
	}
	// A few minimal WebDAV servers implement COPY but not MOVE. Fall back to a
	// server-side copy followed by a delete; the client never transfers the
	// file twice.
	var unsupported *methodUnsupportedError
	if !errors.As(err, &unsupported) {
		return err
	}
	if copyErr := b.copy(oldPath, newPath); copyErr != nil {
		return fmt.Errorf("重命名 WebDAV 文件失败（MOVE 不可用，COPY 也失败）: %w", copyErr)
	}
	if removeErr := b.Remove(oldRel); removeErr != nil {
		return fmt.Errorf("复制成功但删除 WebDAV 临时文件失败: %w", removeErr)
	}
	return nil
}

type methodUnsupportedError struct {
	method string
	status int
}

func (e *methodUnsupportedError) Error() string {
	return fmt.Sprintf("WebDAV 服务器不支持 %s（HTTP %d）", e.method, e.status)
}

func (b *Backend) move(oldPath, newPath string) error {
	req, err := b.newRequest(methodMOVE, oldPath, nil, 0)
	if err != nil {
		return err
	}
	req.Header.Set("Destination", b.urlFor(newPath))
	req.Header.Set("Overwrite", "T")
	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("重命名 WebDAV 文件 %s 失败: %w", b.LocationForPath(oldPath), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		return &methodUnsupportedError{method: "MOVE", status: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return b.statusError("MOVE", oldPath, resp)
	}
	_ = drain(resp)
	return nil
}

func (b *Backend) copy(oldPath, newPath string) error {
	req, err := b.newRequest(methodCOPY, oldPath, nil, 0)
	if err != nil {
		return err
	}
	req.Header.Set("Destination", b.urlFor(newPath))
	req.Header.Set("Overwrite", "T")
	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("复制 WebDAV 文件 %s 失败: %w", b.LocationForPath(oldPath), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return b.statusError("COPY", oldPath, resp)
	}
	_ = drain(resp)
	return nil
}

func (b *Backend) Close() error {
	if b.http != nil {
		b.http.CloseIdleConnections()
	}
	return nil
}

func (b *Backend) resolve(rel string) (string, error) {
	clean, err := storage.CleanRel(rel)
	if err != nil {
		return "", err
	}
	if clean == "" {
		return b.rootPath, nil
	}
	return path.Join(b.rootPath, clean), nil
}

func notExist(op, location string) error {
	return &os.PathError{Op: op, Path: location, Err: os.ErrNotExist}
}

func (b *Backend) LocationForPath(p string) string {
	return b.displayURL(p)
}

func (b *Backend) newRequest(method, p string, body io.Reader, contentLength int64) (*http.Request, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, b.urlFor(p), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = contentLength
	}
	req.Header.Set("User-Agent", "NASArchiveSync/WebDAV")
	if b.hasAuth {
		req.SetBasicAuth(b.user, b.pass)
	}
	return req, nil
}

func (b *Backend) do(method, p string, body io.Reader, contentLength int64) (*http.Response, error) {
	req, err := b.newRequest(method, p, body, contentLength)
	if err != nil {
		return nil, err
	}
	if method == methodPROPFIND {
		req.Header.Set("Depth", "0")
		req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	}
	return b.http.Do(req)
}

func (b *Backend) statusError(op, p string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	message := strings.TrimSpace(string(body))
	if len(message) > 300 {
		message = message[:300] + "..."
	}
	if message != "" {
		message = ": " + message
	}
	status := resp.StatusCode
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("WebDAV 认证失败（用户 %q，HTTP 401）%s；请检查 URL 中的用户名密码或 --webdav-user/--webdav-pass", b.user, message)
	case http.StatusForbidden:
		return fmt.Errorf("没有 WebDAV 访问权限（HTTP 403）%s: %s", message, b.LocationForPath(p))
	case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return fmt.Errorf("WebDAV 服务器返回重定向（HTTP %d）%s；请直接使用最终地址: %s", status, message, b.LocationForPath(p))
	}
	return fmt.Errorf("WebDAV %s 失败（HTTP %d %s）%s: %s", op, status, http.StatusText(status), message, b.LocationForPath(p))
}

func drain(resp *http.Response) error {
	_, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	return err
}

type writer struct {
	backend *Backend
	path    string

	mu      sync.Mutex
	pr      *io.PipeReader
	pw      *io.PipeWriter
	done    chan error
	started bool
	closed  bool
	size    int64
	err     error
}

// SetSize lets the writer send a Content-Length instead of a chunked body.
func (w *writer) SetSize(size int64) {
	w.mu.Lock()
	if !w.started {
		w.size = size
	}
	w.mu.Unlock()
}

func (w *writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.closed {
		err := w.err
		if err == nil {
			err = errors.New("WebDAV writer 已关闭")
		}
		w.mu.Unlock()
		return 0, err
	}
	if w.err != nil {
		err := w.err
		w.mu.Unlock()
		return 0, err
	}
	if !w.started {
		w.startLocked()
	}
	if w.err != nil {
		err := w.err
		w.mu.Unlock()
		return 0, err
	}
	pw := w.pw
	w.mu.Unlock()

	n, err := pw.Write(p)
	if err != nil {
		w.setErr(err)
	}
	return n, err
}

// Sync cannot acknowledge data before the server finishes the PUT. All writes
// have already been handed to the HTTP transport, and Close waits for the
// response, so Sync only reports an error that occurred while streaming.
func (w *writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *writer) Close() error {
	w.mu.Lock()
	if w.closed {
		err := w.err
		w.mu.Unlock()
		return err
	}
	w.closed = true
	if !w.started && w.err == nil {
		w.startLocked()
	}
	pw := w.pw
	writeErr := w.err
	w.mu.Unlock()

	if pw != nil {
		if writeErr != nil {
			_ = pw.CloseWithError(writeErr)
		} else {
			_ = pw.Close()
		}
	}

	doneErr := writeErr
	if w.started && w.done != nil {
		if err := <-w.done; err != nil {
			doneErr = err
		}
	}

	w.mu.Lock()
	if doneErr != nil {
		w.err = doneErr
	}
	err := w.err
	w.mu.Unlock()
	return err
}

func (w *writer) startLocked() {
	pr, pw := io.Pipe()
	req, err := w.backend.newRequest(http.MethodPut, w.path, pr, w.size)
	if err != nil {
		_ = pr.CloseWithError(err)
		w.err = err
		return
	}
	if w.size >= 0 {
		req.ContentLength = w.size
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Accept-Encoding", "identity")

	w.pr = pr
	w.pw = pw
	w.started = true

	go func() {
		resp, err := w.backend.http.Do(req)
		if err != nil {
			w.done <- fmt.Errorf("上传 WebDAV 文件 %s 失败: %w", w.backend.LocationForPath(w.path), err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			w.done <- w.backend.statusError("PUT", w.path, resp)
			return
		}
		_ = drain(resp)
		w.done <- nil
	}()
}

func (w *writer) setErr(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	if w.err == nil {
		w.err = err
	}
	w.mu.Unlock()
}

var (
	_ storage.Writer      = (*writer)(nil)
	_ storage.SizedWriter = (*writer)(nil)
	_ storage.RangeReader = (*Backend)(nil)
)
