package local

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/fjf/NASArchiveSync/internal/storage"
)

func TestOpenRangeReadsRequestedBytes(t *testing.T) {
	root := t.TempDir()
	content := []byte("0123456789abcdef")
	if err := os.WriteFile(filepath.Join(root, "f.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	backend, err := Open(storage.Target{Scheme: "local", Path: root}, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()

	rr, ok := backend.(storage.RangeReader)
	if !ok {
		t.Fatal("local backend does not implement RangeReader")
	}
	rc, err := rr.OpenRange("f.bin", 4, 6)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if string(got) != "456789" {
		t.Errorf("range = %q, want %q", got, "456789")
	}
}

func TestOpenRangeRejectsInvalidRange(t *testing.T) {
	backend, err := Open(storage.Target{Scheme: "local", Path: t.TempDir()}, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()

	rr := backend.(storage.RangeReader)
	if _, err := rr.OpenRange("f.bin", -1, 1); err == nil {
		t.Error("negative offset must be rejected")
	}
	if _, err := rr.OpenRange("f.bin", 0, 0); err == nil {
		t.Error("zero length must be rejected")
	}
}
