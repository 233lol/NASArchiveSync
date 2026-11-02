package app

import (
	"errors"
	"io"
	"os"
	"testing"

	"github.com/fjf/NASArchiveSync/internal/storage"
)

// renameTestBackend is a minimal in-memory backend used to exercise the
// overwrite/restore paths without touching a real remote server.
type renameTestBackend struct {
	files      map[string]string
	replaces   bool
	failRename func(oldRel, newRel string) bool
}

func (b *renameTestBackend) Kind() string               { return "fake" }
func (b *renameTestBackend) Root() string               { return "fake" }
func (b *renameTestBackend) Location(rel string) string { return rel }
func (b *renameTestBackend) MkdirAll(string) error      { return nil }

func (b *renameTestBackend) Stat(rel string) (storage.FileInfo, error) {
	v, ok := b.files[rel]
	if !ok {
		return storage.FileInfo{}, os.ErrNotExist
	}
	return storage.FileInfo{Name: rel, Size: int64(len(v))}, nil
}

func (b *renameTestBackend) Open(string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

func (b *renameTestBackend) Create(string) (storage.Writer, error) {
	return nil, errors.New("not implemented")
}

func (b *renameTestBackend) Remove(rel string) error {
	delete(b.files, rel)
	return nil
}

func (b *renameTestBackend) Rename(oldRel, newRel string) error {
	if b.failRename != nil && b.failRename(oldRel, newRel) {
		return errors.New("rename failed")
	}
	v, ok := b.files[oldRel]
	if !ok {
		return os.ErrNotExist
	}
	if _, exists := b.files[newRel]; exists && !b.replaces {
		return errors.New("destination exists")
	}
	b.files[newRel] = v
	delete(b.files, oldRel)
	return nil
}

func (b *renameTestBackend) RenameReplaces() bool { return b.replaces }
func (b *renameTestBackend) Close() error         { return nil }

func TestRenameReplacingDoesNotRemoveOldFileBeforeFailedRename(t *testing.T) {
	b := &renameTestBackend{
		files:    map[string]string{"tmp": "new", "final": "old"},
		replaces: true,
	}
	b.failRename = func(oldRel, newRel string) bool {
		return oldRel == "tmp" && newRel == "final"
	}

	if err := renameReplacing(b, "tmp", "final"); err == nil {
		t.Fatal("expected the rename to fail")
	}
	if got := b.files["final"]; got != "old" {
		t.Errorf("old remote file = %q, want it preserved", got)
	}
}

func TestRenameReplacingRestoresOldFileWhenRenameFails(t *testing.T) {
	b := &renameTestBackend{
		files: map[string]string{"tmp": "new", "final": "old"},
	}
	b.failRename = func(oldRel, newRel string) bool {
		return oldRel == "tmp" && newRel == "final"
	}

	if err := renameReplacing(b, "tmp", "final"); err == nil {
		t.Fatal("expected the rename to fail")
	}
	if got := b.files["final"]; got != "old" {
		t.Errorf("old remote file = %q, want it restored", got)
	}
	if got := b.files["tmp"]; got != "new" {
		t.Errorf("temporary file = %q, want it kept for cleanup", got)
	}
	if _, ok := b.files["tmp.bak"]; ok {
		t.Error("backup must not remain after a successful restore")
	}
}

func TestRenameReplacingReplacesOldFile(t *testing.T) {
	b := &renameTestBackend{
		files: map[string]string{"tmp": "new", "final": "old"},
	}

	if err := renameReplacing(b, "tmp", "final"); err != nil {
		t.Fatalf("renameReplacing: %v", err)
	}
	if got := b.files["final"]; got != "new" {
		t.Errorf("remote file = %q, want new content", got)
	}
	if _, ok := b.files["tmp"]; ok {
		t.Error("temporary file must be gone")
	}
	if _, ok := b.files["tmp.bak"]; ok {
		t.Error("backup must be removed")
	}
}
