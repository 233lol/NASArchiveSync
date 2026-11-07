package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fjf/NASArchiveSync/internal/progress"
	"github.com/fjf/NASArchiveSync/internal/scan"
	"github.com/fjf/NASArchiveSync/internal/storage"
	"github.com/fjf/NASArchiveSync/internal/xlog"
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

func (b *renameTestBackend) Open(rel string) (io.ReadCloser, error) {
	v, ok := b.files[rel]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(v)), nil
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

func TestRenameReplacingKeepBackupDefersOldFileRemoval(t *testing.T) {
	b := &renameTestBackend{
		files: map[string]string{"tmp": "new", "final": "old"},
	}

	outcome, err := renameReplacingKeepBackup(b, "tmp", "final")
	if err != nil {
		t.Fatalf("renameReplacingKeepBackup: %v", err)
	}
	if outcome.backupRel != "tmp.bak" {
		t.Fatalf("backup rel = %q, want tmp.bak", outcome.backupRel)
	}
	if got := b.files["final"]; got != "new" {
		t.Errorf("remote file = %q, want new content", got)
	}
	if got := b.files["tmp.bak"]; got != "old" {
		t.Errorf("backup = %q, want the old content", got)
	}

	outcome.commit(b)
	if _, ok := b.files["tmp.bak"]; ok {
		t.Error("commit must remove the backup")
	}
}

func TestReplaceOutcomeRollbackRestoresOldFile(t *testing.T) {
	b := &renameTestBackend{
		files: map[string]string{"tmp": "new", "final": "old"},
	}

	outcome, err := renameReplacingKeepBackup(b, "tmp", "final")
	if err != nil {
		t.Fatalf("renameReplacingKeepBackup: %v", err)
	}
	if err := outcome.rollback(b, "final"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := b.files["final"]; got != "old" {
		t.Errorf("remote file = %q, want the old content restored", got)
	}
	if _, ok := b.files["tmp.bak"]; ok {
		t.Error("backup must not remain after a rollback")
	}
}

func TestVerifyUploadedRestoresOldCopyOnMismatch(t *testing.T) {
	b := &renameTestBackend{
		files: map[string]string{
			"a.zip":   "corrupted",
			"tmp.bak": "old",
		},
	}
	up := uploadResult{
		sha:     shaOf("new"),
		outcome: replaceOutcome{backupRel: "tmp.bak"},
	}
	st := &fileState{
		file:  scan.File{Rel: "a.zip", Path: filepath.Join(t.TempDir(), "a.zip"), Size: int64(len("new"))},
		size:  int64(len("new")),
		paths: filePaths{remoteRel: "a.zip", sidecarRel: "a.zip.txt", remotePath: "a.zip"},
	}

	res := verifyUploaded(context.Background(), Options{},
		xlog.New(io.Discard, false, false), b, st, up, fileContext{},
		func(string, int64) progress.Counter { return progress.Discard }, true)

	if res.outcome != OutcomeFailed {
		t.Fatalf("outcome = %v, want failed", res.outcome)
	}
	if got := b.files["a.zip"]; got != "old" {
		t.Errorf("remote file = %q, want the old copy restored", got)
	}
	if _, ok := b.files["tmp.bak"]; ok {
		t.Error("backup must not remain after a verification mismatch")
	}
}
