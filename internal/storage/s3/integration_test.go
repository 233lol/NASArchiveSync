package s3

import (
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/fjf/NASArchiveSync/internal/storage"
)

// TestRealMultipart is a manual integration test for multipart uploads,
// renames and read-back. It is skipped unless NAS_S3_TEST_ENDPOINT and
// NAS_S3_TEST_BUCKET are set.
//
//	$env:NAS_S3_TEST_ENDPOINT="http://192.168.1.5:3900"
//	$env:NAS_S3_TEST_BUCKET="oss"
//	$env:NAS_S3_REGION="garage"
//	$env:NAS_S3_ACCESS_KEY="..."
//	$env:NAS_S3_SECRET_KEY="..."
//	go test -race ./internal/storage/s3 -run TestRealMultipart -v
func TestRealMultipart(t *testing.T) {
	endpoint := os.Getenv("NAS_S3_TEST_ENDPOINT")
	bucket := os.Getenv("NAS_S3_TEST_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("set NAS_S3_TEST_ENDPOINT and NAS_S3_TEST_BUCKET to run")
	}
	region := os.Getenv("NAS_S3_REGION")
	if region == "" {
		region = "garage"
	}
	prefix := fmt.Sprintf("nassync-selftest/%d", time.Now().UnixNano())

	b, err := Open(storage.Target{Scheme: "s3", Bucket: bucket, Path: prefix}, storage.Options{
		S3Endpoint:    endpoint,
		S3Region:      region,
		S3AccessKey:   os.Getenv("NAS_S3_ACCESS_KEY"),
		S3SecretKey:   os.Getenv("NAS_S3_SECRET_KEY"),
		S3Style:       "path",
		Timeout:       30 * time.Second,
		S3PartSize:    5 << 20,
		S3Concurrency: 4,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer b.Close()

	const size = int64(17 << 20)
	w, err := b.Create("multipart.bin")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sized, ok := w.(storage.SizedWriter); ok {
		sized.SetSize(size)
	}
	chunk := make([]byte, 1<<20)
	for written := int64(0); written < size; written += int64(len(chunk)) {
		if _, err := w.Write(chunk); err != nil {
			t.Fatalf("write at %d: %v", written, err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	info, err := b.Stat("multipart.bin")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size != size {
		t.Fatalf("size = %d, want %d", info.Size, size)
	}

	if err := b.Rename("multipart.bin", "multipart-renamed.bin"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	r, err := b.Open("multipart-renamed.bin")
	if err != nil {
		t.Fatalf("open read: %v", err)
	}
	n, readErr := io.Copy(io.Discard, r)
	_ = r.Close()
	if readErr != nil || n != size {
		t.Fatalf("read %d bytes, err %v, want %d", n, readErr, size)
	}
	if err := b.Remove("multipart-renamed.bin"); err != nil {
		t.Fatalf("remove: %v", err)
	}
}
