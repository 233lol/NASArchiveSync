package app

import (
	"fmt"
	"path"
	"strings"

	"github.com/fjf/NASArchiveSync/internal/scan"
	"github.com/fjf/NASArchiveSync/internal/storage"
)

// caseInsensitiveTarget reports whether the backend collapses path case.
func caseInsensitiveTarget(backend storage.Backend) bool {
	ci, ok := backend.(storage.CaseInsensitive)
	return ok && ci.CaseInsensitive()
}

// pathCollisionGroups returns the source files that map to the same remote path
// when the target ignores case. Each group preserves scan order so the error
// message is stable between runs.
func pathCollisionGroups(files []scan.File, sourcePrefix string) [][]string {
	groups := make(map[string][]string, len(files))
	keys := make([]string, 0, len(files))
	for _, file := range files {
		rel := file.Rel
		if sourcePrefix != "" {
			rel = path.Join(sourcePrefix, rel)
		}
		key := strings.ToLower(rel)
		if _, ok := groups[key]; !ok {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], file.Rel)
	}

	var collisions [][]string
	for _, key := range keys {
		if len(groups[key]) > 1 {
			collisions = append(collisions, groups[key])
		}
	}
	return collisions
}

func pathCollisionError(groups [][]string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "检测到 %d 组目标路径冲突（当前存储不区分大小写）:", len(groups))
	for _, group := range groups {
		b.WriteString("\n  ")
		b.WriteString(strings.Join(group, " == "))
	}
	b.WriteString("\n请重命名冲突文件，或用 --exclude 排除后再运行")
	return fmt.Errorf("%s", b.String())
}
