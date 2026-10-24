// Package scan locates archive files below a source directory while applying
// depth limits and include/exclude patterns.
package scan

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

// Options controls the directory walk.
type Options struct {
	Root       string
	MaxDepth   int // -1 means unlimited, 0 only the root, 1 includes one level of sub directories
	Include    []string
	Exclude    []string
	Extensions []string
	// SkipDirs lists absolute directories that must not be entered, for example
	// a local target directory located inside the source tree.
	SkipDirs      []string
	CaseSensitive bool
}

// File describes an archive found below the source root.
type File struct {
	Path     string // native absolute path
	Rel      string // slash separated path relative to the root, including the file name
	RelDir   string // slash separated directory relative to the root, "" for the root
	Size     int64
	Modified time.Time
}

// Scan walks Opts.Root and returns matching archive files sorted by relative
// path. Per entry errors are returned as warnings instead of aborting the walk.
func Scan(o Options) ([]File, []error, error) {
	root, err := filepath.Abs(o.Root)
	if err != nil {
		return nil, nil, fmt.Errorf("解析源目录失败: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, fmt.Errorf("访问源目录失败: %w", err)
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("源路径不是目录: %s", root)
	}

	m, err := newMatcher(o.Include, o.Exclude, o.CaseSensitive)
	if err != nil {
		return nil, nil, err
	}
	exts := newExtSet(o.Extensions)

	var files []File
	var warnings []error

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			warnings = append(warnings, fmt.Errorf("%s: %w", p, walkErr))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		if isSkippedDir(p, o.SkipDirs) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			warnings = append(warnings, fmt.Errorf("%s: %w", p, relErr))
			return nil
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if o.MaxDepth >= 0 && dirDepth(rel) > o.MaxDepth {
				return fs.SkipDir
			}
			if m.excluded(rel) {
				return fs.SkipDir
			}
			return nil
		}

		if !d.Type().IsRegular() {
			return nil
		}
		relDir := path.Dir(rel)
		if relDir == "." {
			relDir = ""
		}
		if o.MaxDepth >= 0 && dirDepth(relDir) > o.MaxDepth {
			return nil
		}
		if !exts.match(rel) {
			return nil
		}
		if m.excluded(rel) {
			return nil
		}
		if !m.included(rel) {
			return nil
		}
		fi, infoErr := d.Info()
		if infoErr != nil {
			warnings = append(warnings, fmt.Errorf("%s: %w", p, infoErr))
			return nil
		}

		files = append(files, File{
			Path:     p,
			Rel:      rel,
			RelDir:   relDir,
			Size:     fi.Size(),
			Modified: fi.ModTime(),
		})
		return nil
	})
	if walkErr != nil {
		return nil, warnings, fmt.Errorf("遍历源目录失败: %w", walkErr)
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Rel < files[j].Rel })
	return files, warnings, nil
}

func dirDepth(rel string) int {
	rel = strings.Trim(rel, "/")
	if rel == "" || rel == "." {
		return 0
	}
	return strings.Count(rel, "/") + 1
}

func isSkippedDir(p string, skip []string) bool {
	if len(skip) == 0 {
		return false
	}
	clean := filepath.Clean(p)
	for _, s := range skip {
		if s == "" {
			continue
		}
		candidate := filepath.Clean(s)
		if clean == candidate {
			return true
		}
		if runtime.GOOS == "windows" && strings.EqualFold(clean, candidate) {
			return true
		}
	}
	return false
}

type extSet map[string]struct{}

func newExtSet(in []string) extSet {
	set := make(extSet)
	for _, raw := range in {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			part = strings.TrimPrefix(part, ".")
			part = strings.ToLower(part)
			if part == "" {
				continue
			}
			set[part] = struct{}{}
		}
	}
	if len(set) == 0 {
		set["zip"] = struct{}{}
		set["7z"] = struct{}{}
		set["rar"] = struct{}{}
	}
	return set
}

func (e extSet) match(rel string) bool {
	base := strings.ToLower(path.Base(rel))
	for ext := range e {
		if strings.HasSuffix(base, "."+ext) {
			return true
		}
	}
	return false
}

type matcher struct {
	include       []string
	exclude       []string
	caseSensitive bool
}

func newMatcher(include, exclude []string, caseSensitive bool) (*matcher, error) {
	m := &matcher{caseSensitive: caseSensitive}
	var err error
	if m.include, err = normalizePatterns(include); err != nil {
		return nil, err
	}
	if m.exclude, err = normalizePatterns(exclude); err != nil {
		return nil, err
	}
	return m, nil
}

func normalizePatterns(in []string) ([]string, error) {
	var out []string
	for _, raw := range in {
		p := normalizePattern(raw)
		if p == "" {
			continue
		}
		if !doublestar.ValidatePattern(p) {
			return nil, fmt.Errorf("无效的匹配模式: %q", raw)
		}
		out = append(out, p)
	}
	return out, nil
}

func normalizePattern(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, `\`, "/")
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	p = strings.TrimLeft(p, "/")
	p = strings.TrimRight(p, "/")
	return p
}

func (m *matcher) included(rel string) bool {
	if len(m.include) == 0 {
		return true
	}
	return m.matchesAny(m.include, rel)
}

func (m *matcher) excluded(rel string) bool {
	if len(m.exclude) == 0 {
		return false
	}
	return m.matchesAny(m.exclude, rel)
}

// matchesAny tests rel and all of its parent directories so that a pattern
// selecting a directory also selects everything below it.
func (m *matcher) matchesAny(patterns []string, rel string) bool {
	rel = strings.Trim(rel, "/")
	if rel == "" || rel == "." {
		return false
	}
	candidates := []string{rel}
	for dir := path.Dir(rel); dir != "." && dir != "/" && dir != ""; dir = path.Dir(dir) {
		candidates = append(candidates, dir)
	}
	for _, candidate := range candidates {
		for _, pattern := range patterns {
			if m.matchOne(pattern, candidate) {
				return true
			}
		}
	}
	return false
}

func (m *matcher) matchOne(pattern, rel string) bool {
	if !m.caseSensitive {
		pattern = strings.ToLower(pattern)
		rel = strings.ToLower(rel)
	}
	if !strings.Contains(pattern, "/") {
		for _, segment := range strings.Split(rel, "/") {
			if ok, _ := doublestar.Match(pattern, segment); ok {
				return true
			}
		}
		return false
	}
	ok, _ := doublestar.Match(pattern, rel)
	return ok
}
