package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fjf/NASArchiveSync/internal/scan"
)

func TestPathCollisionGroups(t *testing.T) {
	files := []scan.File{
		{Rel: "a.7z"},
		{Rel: "A.7z"},
		{Rel: "sub/b.zip"},
		{Rel: "sub/B.zip"},
		{Rel: "c.rar"},
	}

	groups := pathCollisionGroups(files, "")
	if len(groups) != 2 {
		t.Fatalf("groups = %v, want 2 groups", groups)
	}
	if got := strings.Join(groups[0], "|"); got != "a.7z|A.7z" {
		t.Errorf("first group = %q", got)
	}
	if got := strings.Join(groups[1], "|"); got != "sub/b.zip|sub/B.zip" {
		t.Errorf("second group = %q", got)
	}

	// --with-source-dir only prefixes every path, so the collisions remain.
	if groups := pathCollisionGroups(files, "source"); len(groups) != 2 {
		t.Errorf("groups with source dir = %v, want 2 groups", groups)
	}
}

func TestPathCollisionErrorListsFiles(t *testing.T) {
	err := pathCollisionError([][]string{{"a.7z", "A.7z"}})
	msg := err.Error()
	for _, want := range []string{"1 组", "a.7z == A.7z", "--exclude"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

func TestSamePathAndIsWithinResolveSymlinks(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realDir, link); err != nil {
		// Creating a symbolic link needs extra privileges on Windows, but a
		// junction does not and is resolved by EvalSymlinks as well.
		if runtime.GOOS != "windows" {
			t.Skipf("symlinks are not available: %v", err)
		}
		out, cmdErr := exec.Command("cmd", "/c", "mklink", "/J", link, realDir).CombinedOutput()
		if cmdErr != nil {
			t.Skipf("symlinks and junctions are not available: %v (%s)", cmdErr, strings.TrimSpace(string(out)))
		}
	}

	if !samePath(realDir, link) {
		t.Error("samePath must treat a symlink and its target as the same directory")
	}
	if !samePath(link, realDir) {
		t.Error("samePath must be symmetric")
	}
	if !isWithin(link, realDir) {
		t.Error("isWithin must follow symlinks")
	}
	// The target directory may not exist yet; the existing ancestor still has
	// to be resolved.
	if !isWithin(filepath.Join(link, "newdir"), realDir) {
		t.Error("isWithin must resolve symlinked parents of missing paths")
	}
	if samePath(realDir, filepath.Join(root, "other")) {
		t.Error("different directories must not be treated as the same")
	}
}
