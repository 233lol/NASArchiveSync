package scan

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, root, rel string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("data:"+rel), 0o644); err != nil {
		t.Fatal(err)
	}
}

func makeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{
		"root.zip",
		"a/one.7z",
		"a/b/two.7z",
		"a/b/c/three.zip",
		"a/temp/skip.zip",
		"deep/temp/skip2.zip",
		"notes.txt",
	} {
		writeFile(t, root, rel)
	}
	return root
}

func rels(files []File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Rel
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestScanDepth(t *testing.T) {
	root := makeTree(t)

	cases := []struct {
		depth int
		want  []string
	}{
		{-1, []string{"a/b/c/three.zip", "a/b/two.7z", "a/one.7z", "a/temp/skip.zip", "deep/temp/skip2.zip", "root.zip"}},
		{0, []string{"root.zip"}},
		{1, []string{"a/one.7z", "root.zip"}},
		{2, []string{"a/b/two.7z", "a/one.7z", "a/temp/skip.zip", "deep/temp/skip2.zip", "root.zip"}},
	}
	for _, tc := range cases {
		files, _, err := Scan(Options{Root: root, MaxDepth: tc.depth, Extensions: []string{"zip", "7z"}})
		if err != nil {
			t.Fatalf("depth %d: %v", tc.depth, err)
		}
		if got := rels(files); !equalStrings(got, tc.want) {
			t.Errorf("depth %d: got %v, want %v", tc.depth, got, tc.want)
		}
	}
}

func TestScanIncludeExclude(t *testing.T) {
	root := makeTree(t)

	files, _, err := Scan(Options{
		Root:       root,
		MaxDepth:   -1,
		Extensions: []string{"zip", "7z"},
		Exclude:    []string{"temp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rels(files), []string{"a/b/c/three.zip", "a/b/two.7z", "a/one.7z", "root.zip"}; !equalStrings(got, want) {
		t.Errorf("exclude temp: got %v, want %v", got, want)
	}

	files, _, err = Scan(Options{
		Root:       root,
		MaxDepth:   -1,
		Extensions: []string{"zip", "7z"},
		Include:    []string{"a/b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rels(files), []string{"a/b/c/three.zip", "a/b/two.7z"}; !equalStrings(got, want) {
		t.Errorf("include a/b: got %v, want %v", got, want)
	}

	files, _, err = Scan(Options{
		Root:       root,
		MaxDepth:   -1,
		Extensions: []string{"zip", "7z"},
		Include:    []string{"*.zip"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rels(files), []string{"a/b/c/three.zip", "a/temp/skip.zip", "deep/temp/skip2.zip", "root.zip"}; !equalStrings(got, want) {
		t.Errorf("include *.zip: got %v, want %v", got, want)
	}

	files, _, err = Scan(Options{
		Root:       root,
		MaxDepth:   -1,
		Extensions: []string{"zip", "7z"},
		Exclude:    []string{"a/**"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rels(files), []string{"deep/temp/skip2.zip", "root.zip"}; !equalStrings(got, want) {
		t.Errorf("exclude a/**: got %v, want %v", got, want)
	}
}

func TestScanExtensionsCaseInsensitive(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "upper.ZIP")
	writeFile(t, root, "other.txt")

	files, _, err := Scan(Options{Root: root, MaxDepth: -1, Extensions: []string{"zip"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rels(files), []string{"upper.ZIP"}; !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestScanDefaultExtensions(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.zip")
	writeFile(t, root, "b.7z")
	writeFile(t, root, "c.rar")
	writeFile(t, root, "d.txt")

	files, _, err := Scan(Options{Root: root, MaxDepth: -1})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rels(files), []string{"a.zip", "b.7z", "c.rar"}; !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestScanInvalidPattern(t *testing.T) {
	root := makeTree(t)
	if _, _, err := Scan(Options{Root: root, MaxDepth: -1, Include: []string{"a/["}}); err == nil {
		t.Fatal("expected an error for an invalid pattern")
	}
}

func TestScanSkipDir(t *testing.T) {
	root := makeTree(t)
	skip := filepath.Join(root, "a")
	files, _, err := Scan(Options{
		Root:       root,
		MaxDepth:   -1,
		Extensions: []string{"zip", "7z"},
		SkipDirs:   []string{skip},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rels(files), []string{"deep/temp/skip2.zip", "root.zip"}; !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
