package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fjf/NASArchiveSync/internal/storage"
)

func TestAutoStyle(t *testing.T) {
	tests := map[string]string{
		"":                               "virtual",
		"http://192.168.1.5:3900":        "path",
		"http://localhost:3900":          "path",
		"http://nas:3900":                "path",
		"https://s3.example.com":         "virtual",
		"http://192.168.1.5.nip.io:3900": "virtual",
		"http://[2001:db8::1]:3900":      "path",
	}
	for endpoint, want := range tests {
		if got := autoStyle(endpoint); got != want {
			t.Errorf("autoStyle(%q) = %q, want %q", endpoint, got, want)
		}
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	tests := map[string]string{
		"":                             "",
		"192.168.1.5:3900":             "http://192.168.1.5:3900",
		"http://192.168.1.5:3900":      "http://192.168.1.5:3900",
		"https://s3.example.com/":      "https://s3.example.com",
		"https://s3.example.com/base/": "https://s3.example.com/base",
	}
	for in, want := range tests {
		got, err := normalizeEndpoint(in)
		if err != nil {
			t.Errorf("normalizeEndpoint(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("normalizeEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := normalizeEndpoint("ftp://host"); err == nil {
		t.Error("expected an error for a non-http endpoint")
	}
}

func TestCopySourceEscapesSegments(t *testing.T) {
	got := copySource("oss", "a b/中文/100%.zip")
	want := "oss/a%20b/%E4%B8%AD%E6%96%87/100%25.zip"
	if got != want {
		t.Errorf("copySource = %q, want %q", got, want)
	}
}

func TestPathAndVirtualHostStyles(t *testing.T) {
	for _, style := range []string{"path", "virtual"} {
		t.Run(style, func(t *testing.T) {
			fake := newFakeS3()
			srv := httptest.NewServer(fake)
			defer srv.Close()

			b := openTestBackend(t, srv, style, "base")

			w, err := b.Create("dir/a.zip")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if _, err := io.WriteString(w, "hello"); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			if got := string(fake.object("base/dir/a.zip")); got != "hello" {
				t.Errorf("stored body = %q", got)
			}

			if err := b.Rename("dir/a.zip", "dir/b.zip"); err != nil {
				t.Fatalf("rename: %v", err)
			}
			if got := string(fake.object("base/dir/b.zip")); got != "hello" {
				t.Errorf("renamed body = %q", got)
			}
			if fake.has("base/dir/a.zip") {
				t.Error("rename should delete the temporary object")
			}

			info, err := b.Stat("dir/b.zip")
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if info.Size != 5 || info.IsDir {
				t.Errorf("stat = %+v", info)
			}

			if _, err := b.Stat("missing"); !isNotFound(err) {
				t.Errorf("missing object error = %v", err)
			}

			// Verify the request layout that distinguishes the two modes.
			var sawBucketHead, sawPut bool
			for _, r := range fake.requests() {
				path := r.URL
				if i := strings.IndexByte(path, '?'); i >= 0 {
					path = path[:i]
				}
				if r.Method == "HEAD" && (path == "/oss" || path == "/") {
					sawBucketHead = true
				}
				if r.Method == "PUT" && strings.HasSuffix(path, "/base/dir/a.zip") {
					sawPut = true
					if style == "virtual" && !strings.HasPrefix(r.Host, "oss.") {
						t.Errorf("virtual style host = %q", r.Host)
					}
					if style == "path" && strings.HasPrefix(r.Host, "oss.") {
						t.Errorf("path style host = %q", r.Host)
					}
				}
			}
			if !sawBucketHead || !sawPut {
				t.Errorf("requests did not include expected bucket/put calls: %+v", fake.requests())
			}
		})
	}
}

func openTestBackend(t *testing.T, srv *httptest.Server, style, prefix string) storage.Backend {
	t.Helper()
	port := portOf(t, srv.URL)
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Any bucket.example.test host reaches the local test server.
			return dialer.DialContext(ctx, "tcp", srv.Listener.Addr().String())
		},
	}}
	b, err := openWithHTTPClient(storage.Target{
		Scheme: "s3",
		Bucket: "oss",
		Path:   prefix,
	}, storage.Options{
		S3Endpoint:  "http://example.test:" + port,
		S3Region:    "garage",
		S3AccessKey: "access",
		S3SecretKey: "secret",
		S3Style:     style,
		Timeout:     5 * time.Second,
	}, hc)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return b
}

func TestOpenRange(t *testing.T) {
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	b := openTestBackend(t, srv, "path", "base")

	w, err := b.Create("obj.bin")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := io.WriteString(w, "0123456789abcdefghij"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rr, ok := b.(storage.RangeReader)
	if !ok {
		t.Fatal("backend does not implement storage.RangeReader")
	}
	rc, err := rr.OpenRange("obj.bin", 5, 4)
	if err != nil {
		t.Fatalf("open range: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(got) != "5678" {
		t.Fatalf("range body = %q, err %v", got, err)
	}

	// A server that ignores Range would return the whole object; the backend
	// must reject it instead of hashing those bytes as a small chunk.
	fake.mu.Lock()
	fake.ignoreRange = true
	fake.mu.Unlock()
	if _, err := rr.OpenRange("obj.bin", 5, 4); err == nil {
		t.Fatal("expected an error when the server ignores Range")
	}
	fake.mu.Lock()
	fake.ignoreRange = false
	fake.mu.Unlock()

	if _, err := rr.OpenRange("missing.bin", 0, 4); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing object error = %v", err)
	}
}

func portOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

type recorded struct {
	Method string
	URL    string
	Host   string
}

type fakeS3 struct {
	mu          sync.Mutex
	data        map[string][]byte
	reqs        []recorded
	ignoreRange bool
}

func newFakeS3() *fakeS3 {
	return &fakeS3{data: make(map[string][]byte)}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{Method: r.Method, URL: r.URL.RequestURI(), Host: r.Host})
	f.mu.Unlock()

	key := f.key(r)
	switch {
	case r.Method == "HEAD" && (r.URL.Path == "/oss" || r.URL.Path == "/"):
		w.WriteHeader(http.StatusOK)
		return
	case r.Method == "PUT" && r.Header.Get("x-amz-copy-source") != "":
		f.mu.Lock()
		f.data[key] = append([]byte(nil), f.data[strings.TrimPrefix(r.Header.Get("x-amz-copy-source"), "oss/")]...)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<CopyObjectResult><ETag>"etag"</ETag></CopyObjectResult>`)
		return
	case r.Method == "PUT":
		f.mu.Lock()
		f.data[key] = body
		f.mu.Unlock()
		w.Header().Set("ETag", `"etag"`)
		w.WriteHeader(http.StatusOK)
		return
	case r.Method == "GET" && r.URL.Query().Get("list-type") == "2":
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<ListBucketResult><Name>oss</Name><KeyCount>0</KeyCount><MaxKeys>1</MaxKeys><IsTruncated>false</IsTruncated></ListBucketResult>`)
		return
	case r.Method == "GET":
		f.mu.Lock()
		data, ok := f.data[key]
		ignoreRange := f.ignoreRange
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if rng := r.Header.Get("Range"); rng != "" && !ignoreRange {
			start, end, ok := parseRange(rng, int64(len(data)))
			if !ok {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[start : end+1])
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		_, _ = w.Write(data)
		return
	case r.Method == "HEAD":
		f.mu.Lock()
		data, ok := f.data[key]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
		return
	case r.Method == "DELETE":
		f.mu.Lock()
		delete(f.data, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		w.WriteHeader(http.StatusOK)
	}
}

// parseRange understands the single "bytes=start-end" form used by OpenRange.
func parseRange(header string, size int64) (start, end int64, ok bool) {
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false
	}
	parts := strings.SplitN(strings.TrimPrefix(header, "bytes="), "-", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	end, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if start < 0 || end < start || end >= size {
		return 0, 0, false
	}
	return start, end, true
}

func (f *fakeS3) key(r *http.Request) string {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if strings.HasPrefix(r.Host, "oss.") {
		return p
	}
	return strings.TrimPrefix(p, "oss/")
}

func (f *fakeS3) object(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.data[key]...)
}

func (f *fakeS3) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.data[key]
	return ok
}

func (f *fakeS3) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}
