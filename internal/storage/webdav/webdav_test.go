package webdav

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fjf/NASArchiveSync/internal/storage"
)

const (
	testUser = "test"
	testPass = "secret-password"
)

func TestParseFileInfoNotFoundPropstat(t *testing.T) {
	body := []byte(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"><D:response><D:href>/x</D:href><D:propstat><D:prop/><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat></D:response></D:multistatus>`)
	if _, err := parseFileInfo(body, "/x"); !errors.Is(err, errPropNotFound) {
		t.Fatalf("err = %v", err)
	}
	if !os.IsNotExist(notExist("stat", "/x")) {
		t.Error("notExist must satisfy os.IsNotExist")
	}
}

func TestBackendCRUD(t *testing.T) {
	fake := newFakeDAV()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	b := openTestBackend(t, srv, "/dav/base")
	defer b.Close()

	if !strings.HasPrefix(b.Root(), "webdav://test@") || !strings.HasSuffix(b.Root(), "/dav/base") {
		t.Errorf("Root() = %q", b.Root())
	}
	if err := b.MkdirAll("a/b"); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	name := "中文 空格.zip"
	dst, err := b.Create("a/b/" + name)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	content := []byte("webdav-content-0123456789")
	dst.(storage.SizedWriter).SetSize(int64(len(content)))
	if _, err := dst.Write(content[:7]); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Write(content[7:]); err != nil {
		t.Fatal(err)
	}
	if err := dst.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fake.lastContentLength != int64(len(content)) {
		t.Errorf("Content-Length = %d, want %d", fake.lastContentLength, len(content))
	}

	info, err := b.Stat("a/b/" + name)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.IsDir || info.Size != int64(len(content)) || info.Name != name {
		t.Errorf("Stat = %+v", info)
	}

	rc, err := b.Open("a/b/" + name)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("Open content = %q", got)
	}

	rangeRC, err := b.OpenRange("a/b/"+name, 7, 10)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	part, err := io.ReadAll(rangeRC)
	_ = rangeRC.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(part) != string(content[7:17]) {
		t.Errorf("OpenRange = %q, want %q", part, content[7:17])
	}

	if err := b.Rename("a/b/"+name, "a/b/moved.zip"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := b.Stat("a/b/" + name); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old path still exists: %v", err)
	}
	if info, err := b.Stat("a/b/moved.zip"); err != nil || info.Size != int64(len(content)) {
		t.Errorf("Stat moved = %+v, %v", info, err)
	}

	if err := b.Remove("a/b/moved.zip"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := b.Stat("a/b/moved.zip"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("removed path still exists: %v", err)
	}
}

func TestOpenCreatesRoot(t *testing.T) {
	fake := newFakeDAV()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	b := openTestBackend(t, srv, "/dav/game/2026")
	defer b.Close()
	if !fake.hasDir("/dav/game/2026") {
		t.Error("Open with CreateRoot did not create the target directory")
	}
}

func TestMkdirFollowsTrailingSlashRedirect(t *testing.T) {
	fake := newFakeDAV()
	fake.redirectRoot = true
	srv := httptest.NewServer(fake)
	defer srv.Close()

	b := openTestBackend(t, srv, "/dav/game")
	defer b.Close()
	if !fake.hasDir("/dav/game") {
		t.Error("MKCOL did not follow the trailing-slash redirect")
	}
}

func TestHTTPSWithInsecure(t *testing.T) {
	fake := newFakeDAV()
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()

	target, err := storage.ParseTarget(srv.URL + "/dav/base")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(target, storage.Options{
		User:           testUser,
		Password:       testPass,
		Timeout:        5 * time.Second,
		CreateRoot:     true,
		WebDAVInsecure: true,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer b.Close()
	if b.Kind() != "webdavs" {
		t.Errorf("Kind() = %q", b.Kind())
	}
	if fake.hasDir("/dav/base") != true {
		t.Error("HTTPS target root was not created")
	}
}

func TestStatFollowsDirectoryRedirect(t *testing.T) {
	fake := newFakeDAV()
	fake.redirectRoot = true
	srv := httptest.NewServer(fake)
	defer srv.Close()

	b := openTestBackend(t, srv, "/dav")
	defer b.Close()
	info, err := b.Stat("")
	if err != nil || !info.IsDir {
		t.Fatalf("Stat root = %+v, %v", info, err)
	}
}

func TestOpenAuthFailure(t *testing.T) {
	fake := newFakeDAV()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	target, err := storage.ParseTarget(srv.URL + "/dav")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(target, storage.Options{
		User:       testUser,
		Password:   "wrong",
		Timeout:    5 * time.Second,
		CreateRoot: true,
	})
	if err == nil || !strings.Contains(err.Error(), "认证失败") {
		t.Fatalf("Open error = %v", err)
	}
}

func TestOpenRejectsNonWebDAV(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not webdav", http.StatusMethodNotAllowed)
	}))
	defer srv.Close()

	target, err := storage.ParseTarget(srv.URL + "/dav")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(target, storage.Options{
		User:       testUser,
		Password:   testPass,
		Timeout:    5 * time.Second,
		CreateRoot: true,
	})
	if err == nil || !strings.Contains(err.Error(), "不是 WebDAV") {
		t.Fatalf("Open error = %v", err)
	}
}

func TestOpenRangeRejectsIgnoredRange(t *testing.T) {
	fake := newFakeDAV()
	fake.ignoreRange = true
	srv := httptest.NewServer(fake)
	defer srv.Close()

	b := openTestBackend(t, srv, "/dav/base")
	defer b.Close()
	writeRemote(t, b, "a.bin", []byte("0123456789"))

	if _, err := b.OpenRange("a.bin", 2, 4); err == nil || !strings.Contains(err.Error(), "忽略了 Range") {
		t.Fatalf("OpenRange error = %v", err)
	}
}

func TestRenameFallsBackToCopy(t *testing.T) {
	fake := newFakeDAV()
	fake.noMove = true
	srv := httptest.NewServer(fake)
	defer srv.Close()

	b := openTestBackend(t, srv, "/dav/base")
	defer b.Close()
	writeRemote(t, b, "old.zip", []byte("data"))

	if err := b.Rename("old.zip", "new.zip"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := b.Stat("old.zip"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old file still exists: %v", err)
	}
	if info, err := b.Stat("new.zip"); err != nil || info.Size != 4 {
		t.Errorf("new file = %+v, %v", info, err)
	}
	if fake.copyCount != 1 {
		t.Errorf("COPY count = %d, want 1", fake.copyCount)
	}
}

func TestWriterServerError(t *testing.T) {
	fake := newFakeDAV()
	fake.failPut = true
	srv := httptest.NewServer(fake)
	defer srv.Close()

	b := openTestBackend(t, srv, "/dav/base")
	defer b.Close()
	dst, err := b.Create("a.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err == nil || !strings.Contains(err.Error(), "PUT") {
		t.Fatalf("Close error = %v", err)
	}
}

func TestBackendRejectsEscapingPaths(t *testing.T) {
	fake := newFakeDAV()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	b := openTestBackend(t, srv, "/dav/base")
	defer b.Close()
	if _, err := b.Create("../evil.zip"); err == nil {
		t.Error("Create accepted a path outside the root")
	}
	if err := b.MkdirAll("../evil"); err == nil {
		t.Error("MkdirAll accepted a path outside the root")
	}
}

func openTestBackend(t *testing.T, srv *httptest.Server, p string) *Backend {
	t.Helper()
	target, err := storage.ParseTarget(srv.URL + p)
	if err != nil {
		t.Fatalf("ParseTarget: %v", err)
	}
	b, err := Open(target, storage.Options{
		User:       testUser,
		Password:   testPass,
		Timeout:    5 * time.Second,
		CreateRoot: true,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return b.(*Backend)
}

func writeRemote(t *testing.T, b storage.Backend, rel string, data []byte) {
	t.Helper()
	dst, err := b.Create(rel)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dst.(storage.SizedWriter).SetSize(int64(len(data)))
	if _, err := dst.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

type fakeDAV struct {
	mu                sync.Mutex
	files             map[string][]byte
	dirs              map[string]bool
	ignoreRange       bool
	redirectRoot      bool
	noMove            bool
	failPut           bool
	lastContentLength int64
	copyCount         int
}

func newFakeDAV() *fakeDAV {
	return &fakeDAV{
		files: make(map[string][]byte),
		dirs:  map[string]bool{"/dav": true},
	}
}

func (f *fakeDAV) hasDir(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dirs[clean(p)]
}

func clean(p string) string {
	if p == "" {
		return "/"
	}
	return path.Clean(p)
}

func (f *fakeDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != testUser || pass != testPass {
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if f.redirectRoot && (r.Method == "MKCOL" || r.Method == "PROPFIND") && r.URL.Path == "/dav" {
		w.Header().Set("Location", "/dav/")
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	p := clean(r.URL.Path)
	switch r.Method {
	case "PROPFIND":
		f.propfind(w, p)
	case "MKCOL":
		f.mkcol(w, p)
	case "PUT":
		f.put(w, r, p)
	case "GET":
		f.get(w, r, p)
	case "DELETE":
		f.remove(w, p)
	case "MOVE":
		f.move(w, r, p, false)
	case "COPY":
		f.move(w, r, p, true)
	default:
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
	}
}

func (f *fakeDAV) propfind(w http.ResponseWriter, p string) {
	f.mu.Lock()
	data, isFile := f.files[p]
	isDir := f.dirs[p]
	f.mu.Unlock()
	if !isFile && !isDir {
		http.NotFound(w, nil)
		return
	}
	href := (&url.URL{Path: p}).EscapedPath()
	if isDir && !strings.HasSuffix(href, "/") {
		href += "/"
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
	b.WriteString(`<D:multistatus xmlns:D="DAV:"><D:response><D:href>`)
	xmlEscape(&b, href)
	b.WriteString(`</D:href><D:propstat><D:prop><D:resourcetype>`)
	if isDir {
		b.WriteString(`<D:collection/>`)
	}
	b.WriteString(`</D:resourcetype>`)
	if isFile {
		fmt.Fprintf(&b, `<D:getcontentlength>%d</D:getcontentlength>`, len(data))
	}
	b.WriteString(`<D:getlastmodified>`)
	xmlEscape(&b, time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC).Format(http.TimeFormat))
	b.WriteString(`</D:getlastmodified></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, b.String())
}

func xmlEscape(b *strings.Builder, s string) {
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteRune(r)
		}
	}
}

func (f *fakeDAV) mkcol(w http.ResponseWriter, p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, isFile := f.files[p]; isFile || f.dirs[p] {
		http.Error(w, "exists", http.StatusMethodNotAllowed)
		return
	}
	parent := path.Dir(p)
	if parent != "/" && !f.dirs[parent] {
		http.Error(w, "parent missing", http.StatusConflict)
		return
	}
	f.dirs[p] = true
	w.WriteHeader(http.StatusCreated)
}

func (f *fakeDAV) put(w http.ResponseWriter, r *http.Request, p string) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPut {
		http.Error(w, "server says no", http.StatusInternalServerError)
		return
	}
	parent := path.Dir(p)
	if parent != "/" && !f.dirs[parent] {
		http.Error(w, "parent missing", http.StatusConflict)
		return
	}
	f.files[p] = data
	f.lastContentLength = r.ContentLength
	w.WriteHeader(http.StatusCreated)
}

func (f *fakeDAV) get(w http.ResponseWriter, r *http.Request, p string) {
	f.mu.Lock()
	data, ok := f.files[p]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, nil)
		return
	}
	if raw := r.Header.Get("Range"); raw != "" && !f.ignoreRange {
		start, end, ok := fakeRange(raw, int64(len(data)))
		if !ok {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func fakeRange(raw string, size int64) (int64, int64, bool) {
	raw = strings.TrimPrefix(raw, "bytes=")
	parts := strings.SplitN(raw, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, err1 := strconv.ParseInt(parts[0], 10, 64)
	end, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start || end >= size {
		return 0, 0, false
	}
	return start, end, true
}

func (f *fakeDAV) remove(w http.ResponseWriter, p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[p]; !ok {
		http.NotFound(w, nil)
		return
	}
	delete(f.files, p)
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeDAV) move(w http.ResponseWriter, r *http.Request, oldPath string, copyOnly bool) {
	if !copyOnly && f.noMove {
		http.Error(w, "move unsupported", http.StatusMethodNotAllowed)
		return
	}
	destURL, err := url.Parse(r.Header.Get("Destination"))
	if err != nil || destURL.Path == "" {
		http.Error(w, "bad destination", http.StatusBadRequest)
		return
	}
	newPath := clean(destURL.Path)

	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[oldPath]
	if !ok {
		http.NotFound(w, nil)
		return
	}
	if parent := path.Dir(newPath); parent != "/" && !f.dirs[parent] {
		http.Error(w, "parent missing", http.StatusConflict)
		return
	}
	_, existed := f.files[newPath]
	f.files[newPath] = append([]byte(nil), data...)
	if copyOnly {
		f.copyCount++
	} else {
		delete(f.files, oldPath)
	}
	if existed {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}
