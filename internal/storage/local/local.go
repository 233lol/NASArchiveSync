// Package local implements the storage.Backend interface for a local directory.
// It exists so the full upload pipeline can be exercised without a NAS and can
// be used for local copies.
package local

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

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
