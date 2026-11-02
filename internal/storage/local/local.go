// Package local implements the storage.Backend interface for a local directory.
// It exists so the full upload pipeline can be exercised without a NAS and can
// be used for local copies.
package local

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/fjf/NASArchiveSync/internal/storage"
)

type Backend struct {
	root string
}

// Open opens (and optionally creates) a local target directory.
func Open(t storage.Target, opt storage.Options) (storage.Backend, error) {
	root, err := filepath.Abs(t.Path)
	if err != nil {
		return nil, fmt.Errorf("解析本地目标路径失败: %w", err)
	}
	if opt.CreateRoot {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, fmt.Errorf("创建本地目标目录失败: %w", err)
		}
	}
	return &Backend{root: root}, nil
}

func (b *Backend) Kind() string { return "local" }

// RenameReplaces reports whether os.Rename replaces an existing destination.
// Unix does; Windows returns an error instead.
func (b *Backend) RenameReplaces() bool { return runtime.GOOS != "windows" }

func (b *Backend) Root() string { return b.root }

func (b *Backend) Location(rel string) string {
	clean, err := storage.CleanRel(rel)
	if err != nil || clean == "" {
		return b.root
	}
	return filepath.Join(b.root, filepath.FromSlash(clean))
}

func (b *Backend) MkdirAll(rel string) error {
	p, err := b.path(rel)
	if err != nil {
		return err
	}
	return os.MkdirAll(p, 0o755)
}

func (b *Backend) Stat(rel string) (storage.FileInfo, error) {
	p, err := b.path(rel)
	if err != nil {
		return storage.FileInfo{}, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return storage.FileInfo{}, err
	}
	return storage.FileInfo{Name: fi.Name(), Size: fi.Size(), IsDir: fi.IsDir(), ModTime: fi.ModTime()}, nil
}

func (b *Backend) Open(rel string) (io.ReadCloser, error) {
	p, err := b.path(rel)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

// OpenRange reads length bytes starting at offset.
func (b *Backend) OpenRange(rel string, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("无效的本地读取范围: offset=%d length=%d", offset, length)
	}
	p, err := b.path(rel)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return storage.NewRangeReadCloser(f, offset, length, f), nil
}

// List returns the immediate children of a local directory.
func (b *Backend) List(rel string) ([]storage.FileInfo, error) {
	p, err := b.path(rel)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	out := make([]storage.FileInfo, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
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
	p, err := b.path(rel)
	if err != nil {
		return nil, err
	}
	if dir := filepath.Dir(p); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return os.Create(p)
}

func (b *Backend) Remove(rel string) error {
	p, err := b.path(rel)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

func (b *Backend) Rename(oldRel, newRel string) error {
	oldPath, err := b.path(oldRel)
	if err != nil {
		return err
	}
	newPath, err := b.path(newRel)
	if err != nil {
		return err
	}
	return os.Rename(oldPath, newPath)
}

func (b *Backend) Close() error { return nil }

func (b *Backend) path(rel string) (string, error) {
	clean, err := storage.CleanRel(rel)
	if err != nil {
		return "", err
	}
	return storage.LocalPath(b.root, clean), nil
}
