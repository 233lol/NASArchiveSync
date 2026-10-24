package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fjf/NASArchiveSync/internal/report"
	"github.com/fjf/NASArchiveSync/internal/storage"
	"github.com/fjf/NASArchiveSync/internal/xlog"
)

func writeSourceFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(data)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func shaOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func testOptions(src, dst string) Options {
	return Options{
		Source:      src,
		Target:      "local:" + dst,
		Depth:       -1,
		Extensions:  []string{"zip", "7z"},
		Version:     "test",
		Logger:      xlog.New(io.Discard, true, false),
		ProgressOut: io.Discard,
		Now:         func() time.Time { return time.Date(2026, 10, 24, 15, 4, 5, 0, time.UTC) },
	}
}

func TestRunUploadsVerifiesAndDeletes(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a/z.7z", "archive-z")
	writeSourceFile(t, src, "a/b/y.zip", "archive-y")
	writeSourceFile(t, src, "notes.txt", "keep me")

	res, err := Run(context.Background(), testOptions(src, dst))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Total != 2 || res.Uploaded != 2 || res.Failed != 0 || res.Skipped != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}

	if got := readFile(t, filepath.Join(dst, "a", "z.7z")); got != "archive-z" {
		t.Errorf("remote content = %q", got)
	}
	if got := readFile(t, filepath.Join(dst, "a", "b", "y.zip")); got != "archive-y" {
		t.Errorf("remote content = %q", got)
	}

	sidecar := filepath.Join(dst, "a", "z.7z.txt")
	rec, err := report.Parse([]byte(readFile(t, sidecar)))
	if err != nil {
		t.Fatalf("parse sidecar: %v", err)
	}
	if rec.SHA256 != shaOf("archive-z") {
		t.Errorf("sidecar sha = %q, want %q", rec.SHA256, shaOf("archive-z"))
	}
	if rec.Size != int64(len("archive-z")) {
		t.Errorf("sidecar size = %d", rec.Size)
	}
	if !rec.Verified || rec.FileName != "z.7z" {
		t.Errorf("sidecar fields = %+v", rec)
	}
	if !strings.Contains(rec.TargetPath, "z.7z") {
		t.Errorf("sidecar target path = %q", rec.TargetPath)
	}

	// The completion note is stored on both sides and the local copy stays
	// behind after the archive is deleted.
	localSidecar := filepath.Join(src, "a", "z.7z.txt")
	localRec, err := report.Parse([]byte(readFile(t, localSidecar)))
	if err != nil {
		t.Fatalf("parse local sidecar: %v", err)
	}
	if localRec.SHA256 != rec.SHA256 || localRec.Size != rec.Size {
		t.Errorf("local sidecar differs: %+v vs %+v", localRec, rec)
	}
	if readFile(t, localSidecar) != readFile(t, sidecar) {
		t.Error("local and remote completion notes should be identical")
	}

	if exists(filepath.Join(src, "a", "z.7z")) || exists(filepath.Join(src, "a", "b", "y.zip")) {
		t.Error("source archives should have been deleted")
	}
	if !exists(localSidecar) {
		t.Error("local completion note must be kept after deleting the archive")
	}
	if !exists(filepath.Join(src, "a", "b", "y.zip.txt")) {
		t.Error("completion note for the nested archive is missing")
	}
	if !exists(filepath.Join(src, "notes.txt")) {
		t.Error("non archive file must be kept")
	}
	if exists(filepath.Join(dst, "a", "z.7z.nassync.tmp")) {
		t.Error("temporary upload file left behind")
	}
}

func TestRunDryRunChangesNothing(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a/z.7z", "archive-z")
	writeSourceFile(t, src, "b/y.zip", "archive-y")

	opts := testOptions(src, dst)
	opts.DryRun = true
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Planned != 2 || res.Uploaded != 0 || res.Failed != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if exists(dst) {
		t.Error("dry run must not create the target directory")
	}
	if exists(filepath.Join(src, "a", "z.7z.txt")) {
		t.Error("dry run must not write a local completion note")
	}
	if !exists(filepath.Join(src, "a", "z.7z")) || !exists(filepath.Join(src, "b", "y.zip")) {
		t.Error("dry run must not delete source files")
	}
}

func TestRunSkipsAlreadyUploaded(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a/z.7z", "archive-z")

	opts := testOptions(src, dst)
	opts.KeepSource = true
	if res, err := Run(context.Background(), opts); err != nil || res.Uploaded != 1 {
		t.Fatalf("first run: res=%+v err=%v", res, err)
	}
	localNote := filepath.Join(src, "a", "z.7z.txt")
	firstNote := readFile(t, localNote)

	// A skipped run must restore a missing local note from the remote record.
	if err := os.Remove(localNote); err != nil {
		t.Fatal(err)
	}
	opts.Recheck = true
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Skipped != 1 || res.Uploaded != 0 || res.Failed != 0 {
		t.Fatalf("second run result: %+v", res)
	}
	if got := readFile(t, localNote); got != firstNote {
		t.Error("local note was not restored verbatim on skip")
	}

	// Third run without --keep-source removes the local copy while skipping.
	opts.Recheck = false
	opts.KeepSource = false
	res, err = Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if res.Skipped != 1 {
		t.Fatalf("third run result: %+v", res)
	}
	if exists(filepath.Join(src, "a", "z.7z")) {
		t.Error("local archive should be removed after a verified skip")
	}
	if !exists(localNote) {
		t.Error("local completion note must survive archive deletion")
	}
}

func TestRunDepthLimit(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "root.zip", "root")
	writeSourceFile(t, src, "a/one.7z", "one")
	writeSourceFile(t, src, "a/b/two.7z", "two")

	opts := testOptions(src, dst)
	opts.Depth = 1
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 2 {
		t.Fatalf("result: %+v", res)
	}
	if exists(filepath.Join(dst, "a", "b", "two.7z")) {
		t.Error("depth 1 must not upload files below two levels")
	}
	if !exists(filepath.Join(src, "a", "b", "two.7z")) {
		t.Error("file outside depth must stay local")
	}
}

func TestRunConflictKeepsLocal(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a/z.7z", "archive-z")
	if err := os.MkdirAll(filepath.Join(dst, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "a", "z.7z"), []byte("different"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := testOptions(src, dst)
	opts.KeepSource = true
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Failed != 1 || res.Uploaded != 0 {
		t.Fatalf("result: %+v", res)
	}
	if !exists(filepath.Join(src, "a", "z.7z")) {
		t.Error("local file must be kept on conflict")
	}
	if got := readFile(t, filepath.Join(dst, "a", "z.7z")); got != "different" {
		t.Errorf("remote file must be untouched, got %q", got)
	}
	if exists(filepath.Join(dst, "a", "z.7z.txt")) {
		t.Error("no completion note should be written on conflict")
	}
	if exists(filepath.Join(src, "a", "z.7z.txt")) {
		t.Error("no local completion note should be written on conflict")
	}
}

func TestRunForceOverwrites(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a/z.7z", "archive-z")
	if err := os.MkdirAll(filepath.Join(dst, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "a", "z.7z"), []byte("different"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := testOptions(src, dst)
	opts.Force = true
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 1 || res.Failed != 0 {
		t.Fatalf("result: %+v", res)
	}
	if got := readFile(t, filepath.Join(dst, "a", "z.7z")); got != "archive-z" {
		t.Errorf("remote content = %q", got)
	}
	if !exists(filepath.Join(dst, "a", "z.7z.txt")) {
		t.Error("completion note missing")
	}
}

func TestUseNativeSMB(t *testing.T) {
	smbTarget := storage.Target{Scheme: "smb", Host: "nas", Share: "share"}
	localTarget := storage.Target{Scheme: "local", Path: `Z:\game`}

	tests := []struct {
		name    string
		target  storage.Target
		mode    string
		auth    bool
		native  bool
		wantErr bool
	}{
		{name: "auto without credentials uses system", target: smbTarget, mode: "", native: true},
		{name: "auto with credentials uses builtin", target: smbTarget, mode: "auto", auth: true},
		{name: "auto with custom port uses builtin", target: storage.Target{Scheme: "smb", Host: "nas", Share: "share", Port: 1445}, mode: "auto"},
		{name: "native forced", target: smbTarget, mode: "native", native: true},
		{name: "native with custom port", target: storage.Target{Scheme: "smb", Host: "nas", Share: "share", Port: 1445}, mode: "native", wantErr: true},
		{name: "builtin forced", target: smbTarget, mode: "builtin"},
		{name: "builtin forced with credentials", target: smbTarget, mode: "builtin", auth: true},
		{name: "local target never native", target: localTarget, mode: "native"},
		{name: "invalid mode", target: smbTarget, mode: "magic", wantErr: true},
	}
	for _, tc := range tests {
		got, err := useNativeSMB(tc.target, Options{SMBMode: tc.mode, SMBAuthRequested: tc.auth})
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: expected an error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.native {
			t.Errorf("%s: got native=%v, want %v", tc.name, got, tc.native)
		}
	}
}

func TestParseTxtMode(t *testing.T) {
	tests := []struct {
		in         string
		want       TxtMode
		wantErr    bool
		wantLocal  bool
		wantRemote bool
	}{
		{in: "", want: TxtBoth, wantLocal: true, wantRemote: true},
		{in: "both", want: TxtBoth, wantLocal: true, wantRemote: true},
		{in: "BOTH", want: TxtBoth, wantLocal: true, wantRemote: true},
		{in: "local", want: TxtLocal, wantLocal: true},
		{in: "remote", want: TxtRemote, wantRemote: true},
		{in: "none", want: TxtNone},
		{in: "magic", wantErr: true},
	}
	for _, tc := range tests {
		got, err := ParseTxtMode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: expected an error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.in, got, tc.want)
		}
		if got.WritesLocal() != tc.wantLocal || got.WritesRemote() != tc.wantRemote {
			t.Errorf("%q: WritesLocal=%v WritesRemote=%v", tc.in, got.WritesLocal(), got.WritesRemote())
		}
	}
}

func TestRunTxtLocalOnly(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a/z.7z", "archive-z")

	opts := testOptions(src, dst)
	opts.Txt = TxtLocal
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 1 || res.Failed != 0 {
		t.Fatalf("result: %+v", res)
	}
	if !exists(filepath.Join(src, "a", "z.7z.txt")) {
		t.Error("local note missing")
	}
	if exists(filepath.Join(dst, "a", "z.7z.txt")) {
		t.Error("remote note must not be written with --txt local")
	}
}

func TestRunTxtRemoteOnly(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a/z.7z", "archive-z")

	opts := testOptions(src, dst)
	opts.Txt = TxtRemote
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 1 || res.Failed != 0 {
		t.Fatalf("result: %+v", res)
	}
	if !exists(filepath.Join(dst, "a", "z.7z.txt")) {
		t.Error("remote note missing")
	}
	if exists(filepath.Join(src, "a", "z.7z.txt")) {
		t.Error("local note must not be written with --txt remote")
	}
}

func TestRunTxtNone(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a/z.7z", "archive-z")

	opts := testOptions(src, dst)
	opts.Txt = TxtNone
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 1 || res.Failed != 0 {
		t.Fatalf("result: %+v", res)
	}
	if exists(filepath.Join(src, "a", "z.7z.txt")) || exists(filepath.Join(dst, "a", "z.7z.txt")) {
		t.Error("no note should be written with --txt none")
	}
	if exists(filepath.Join(src, "a", "z.7z")) {
		t.Error("source archive should be deleted")
	}
}

func TestRunTxtNoneIsIdempotent(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a/z.7z", "archive-z")

	opts := testOptions(src, dst)
	opts.Txt = TxtNone
	opts.KeepSource = true
	if res, err := Run(context.Background(), opts); err != nil || res.Uploaded != 1 {
		t.Fatalf("first run: res=%+v err=%v", res, err)
	}

	// No notes exist, so the remote file is read back to prove it matches.
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Skipped != 1 || res.Failed != 0 {
		t.Fatalf("second run result: %+v", res)
	}
	if !exists(filepath.Join(src, "a", "z.7z")) {
		t.Error("--keep-source must keep the archive")
	}

	opts.KeepSource = false
	res, err = Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if res.Skipped != 1 {
		t.Fatalf("third run result: %+v", res)
	}
	if exists(filepath.Join(src, "a", "z.7z")) {
		t.Error("archive should be deleted after a verified skip")
	}
}

func TestRunTxtLocalIsIdempotentAndRestoresNote(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a/z.7z", "archive-z")

	opts := testOptions(src, dst)
	opts.Txt = TxtLocal
	opts.KeepSource = true
	if res, err := Run(context.Background(), opts); err != nil || res.Uploaded != 1 {
		t.Fatalf("first run: res=%+v err=%v", res, err)
	}
	localNote := filepath.Join(src, "a", "z.7z.txt")

	if err := os.Remove(localNote); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Skipped != 1 || res.Failed != 0 {
		t.Fatalf("second run result: %+v", res)
	}
	restored, err := report.Parse([]byte(readFile(t, localNote)))
	if err != nil {
		t.Fatalf("restored note is invalid: %v", err)
	}
	if restored.SHA256 != shaOf("archive-z") {
		t.Errorf("restored sha = %q", restored.SHA256)
	}
	if exists(filepath.Join(dst, "a", "z.7z.txt")) {
		t.Error("remote note must not appear with --txt local")
	}
}

func TestRunRejectsIdenticalSourceAndTarget(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a/z.7z", "archive-z")
	opts := testOptions(src, src)
	if _, err := Run(context.Background(), opts); err == nil {
		t.Fatal("expected an error when source equals target")
	}
	if !exists(filepath.Join(src, "a", "z.7z")) {
		t.Error("source file must be kept")
	}
}

func TestRunKeepsTargetInsideSourceOutOfScan(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(src, "out")
	writeSourceFile(t, src, "a/z.7z", "archive-z")

	opts := testOptions(src, dst)
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 1 || res.Failed != 0 {
		t.Fatalf("result: %+v", res)
	}
	if !exists(filepath.Join(dst, "a", "z.7z")) {
		t.Error("uploaded file missing below the target directory")
	}

	// A second run must not treat the target tree as source material.
	res, err = Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res.Total != 0 {
		t.Fatalf("target tree was scanned: %+v", res)
	}
}

func TestRunWithSourceDir(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "root.zip", "archive-root")
	writeSourceFile(t, src, "a/z.7z", "archive-z")

	opts := testOptions(src, dst)
	opts.WithSourceDir = true

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 2 || res.Failed != 0 {
		t.Fatalf("result: %+v", res)
	}

	name := filepath.Base(src)
	if got := readFile(t, filepath.Join(dst, name, "root.zip")); got != "archive-root" {
		t.Errorf("root archive content = %q", got)
	}
	if got := readFile(t, filepath.Join(dst, name, "a", "z.7z")); got != "archive-z" {
		t.Errorf("nested archive content = %q", got)
	}
	for _, p := range []string{
		filepath.Join(dst, name, "root.zip.txt"),
		filepath.Join(dst, name, "a", "z.7z.txt"),
	} {
		if !exists(p) {
			t.Errorf("remote note missing: %s", p)
		}
	}
	if exists(filepath.Join(dst, "root.zip")) || exists(filepath.Join(dst, "a", "z.7z")) {
		t.Error("files must not be uploaded directly into the target root")
	}
	if exists(filepath.Join(src, "a", "z.7z")) {
		t.Error("source archive should be deleted after a verified upload")
	}
}

func TestRunWithSourceDirDryRunCreatesNothing(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a/z.7z", "archive-z")

	opts := testOptions(src, dst)
	opts.WithSourceDir = true
	opts.DryRun = true

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Planned != 1 || res.Failed != 0 {
		t.Fatalf("result: %+v", res)
	}
	if exists(filepath.Join(dst, filepath.Base(src))) {
		t.Error("dry run must not create the source-named directory")
	}
	if !exists(filepath.Join(src, "a", "z.7z")) {
		t.Error("dry run must not delete the source archive")
	}
}

func TestRunWithSourceDirRejectsTargetParentOfSource(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "a")
	writeSourceFile(t, src, "z.7z", "archive-z")

	opts := testOptions(src, parent)
	opts.WithSourceDir = true

	if _, err := Run(context.Background(), opts); err == nil {
		t.Fatal("expected an error when target/<source name> resolves to the source directory")
	}
	if !exists(filepath.Join(src, "z.7z")) {
		t.Error("source file must be kept")
	}
}

func TestSourceDirName(t *testing.T) {
	name, err := sourceDirName(filepath.Join(t.TempDir(), "game", "a"))
	if err != nil {
		t.Fatalf("sourceDirName: %v", err)
	}
	if name != "a" {
		t.Errorf("name = %q, want %q", name, "a")
	}

	root := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	if _, err := sourceDirName(root); err == nil {
		t.Errorf("sourceDirName(%q) should fail for a volume root", root)
	}
}

func TestRunListPrintsAllMatches(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "root.zip", "r")
	writeSourceFile(t, src, "[abc]/a.7z", "a")
	writeSourceFile(t, src, "other/b.rar", "b")
	writeSourceFile(t, src, "other/c.txt", "c")

	var buf bytes.Buffer
	opts := testOptions(src, "")
	opts.Target = ""
	opts.Depth = -1
	opts.Extensions = nil
	opts.List = true
	opts.ListOut = &buf

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "[abc]/a.7z\nother/b.rar\nroot.zip\n"
	if got := buf.String(); got != want {
		t.Errorf("list output = %q, want %q", got, want)
	}
	if res.Total != 3 {
		t.Errorf("Total = %d, want 3", res.Total)
	}
	if !exists(filepath.Join(src, "[abc]", "a.7z")) || !exists(filepath.Join(src, "root.zip")) {
		t.Error("--list must not delete or move source files")
	}
}

func TestRunListAppliesIncludePattern(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "[abc]/a.7z", "a")
	writeSourceFile(t, src, "[abc]/sub/b.zip", "b")
	writeSourceFile(t, src, "other/c.zip", "c")

	var buf bytes.Buffer
	opts := testOptions(src, "")
	opts.Target = ""
	opts.Depth = -1
	opts.Include = []string{"[[]*]"}
	opts.List = true
	opts.ListOut = &buf

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "[abc]/a.7z\n[abc]/sub/b.zip\n"
	if got := buf.String(); got != want {
		t.Errorf("list output = %q, want %q", got, want)
	}
	if res.Total != 2 {
		t.Errorf("Total = %d, want 2", res.Total)
	}
}

func TestRunListNeedsNoTarget(t *testing.T) {
	src := t.TempDir()
	writeSourceFile(t, src, "a.zip", "a")

	var buf bytes.Buffer
	opts := testOptions(src, "")
	opts.Target = ""
	opts.List = true
	opts.ListOut = &buf

	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := buf.String(); got != "a.zip\n" {
		t.Errorf("list output = %q", got)
	}
}

func TestRunPipelineUploadsAndVerifies(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a/one.zip", "one")
	writeSourceFile(t, src, "a/two.7z", "two")
	writeSourceFile(t, src, "b/three.rar", "three")

	opts := testOptions(src, dst)
	opts.Pipeline = true
	opts.Extensions = nil // use the built-in zip/7z/rar default

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 3 || res.Failed != 0 {
		t.Fatalf("result: %+v", res)
	}
	if res.UploadTime <= 0 {
		t.Errorf("upload timing was not recorded: %v", res.UploadTime)
	}
	for _, rel := range []string{"a/one.zip", "a/two.7z", "b/three.rar"} {
		if !exists(filepath.Join(dst, filepath.FromSlash(rel))) {
			t.Errorf("remote file missing: %s", rel)
		}
		if !exists(filepath.Join(dst, filepath.FromSlash(rel)+".txt")) {
			t.Errorf("remote note missing: %s", rel)
		}
		if exists(filepath.Join(src, filepath.FromSlash(rel))) {
			t.Errorf("source file should be deleted: %s", rel)
		}
		if !exists(filepath.Join(src, filepath.FromSlash(rel)+".txt")) {
			t.Errorf("local note missing: %s", rel)
		}
	}
}

func TestRunPipelineConflictKeepsLocalAndContinues(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a.zip", "local-a")
	writeSourceFile(t, src, "b.zip", "local-b")
	writeSourceFile(t, dst, "a.zip", "remote-conflict")

	opts := testOptions(src, dst)
	opts.Pipeline = true

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 1 || res.Failed != 1 {
		t.Fatalf("result: %+v", res)
	}
	if !exists(filepath.Join(src, "a.zip")) {
		t.Error("conflicting source must be kept")
	}
	if exists(filepath.Join(src, "b.zip")) {
		t.Error("successful source should be deleted")
	}
	if got := readFile(t, filepath.Join(dst, "b.zip")); got != "local-b" {
		t.Errorf("b.zip content = %q", got)
	}
}

func TestRunPipelineSkipsAlreadyUploaded(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a.zip", "a")
	writeSourceFile(t, src, "b.zip", "b")

	opts := testOptions(src, dst)
	opts.Pipeline = true
	opts.KeepSource = true
	if res, err := Run(context.Background(), opts); err != nil || res.Uploaded != 2 {
		t.Fatalf("first run: res=%+v err=%v", res, err)
	}

	opts.KeepSource = false
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Skipped != 2 || res.Uploaded != 0 || res.Failed != 0 {
		t.Fatalf("second run result: %+v", res)
	}
	if exists(filepath.Join(src, "a.zip")) || exists(filepath.Join(src, "b.zip")) {
		t.Error("sources should be deleted after a verified skip")
	}
}

func TestRunNoVerifyUploadsWithoutHashing(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a/one.zip", "one")
	writeSourceFile(t, src, "two.7z", "two")

	opts := testOptions(src, dst)
	opts.NoVerify = true
	opts.Pipeline = true // pipeline is ignored when verification is off

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 2 || res.Failed != 0 || !res.NoVerify {
		t.Fatalf("result: %+v", res)
	}
	if res.HashTime != 0 || res.VerifyTime != 0 {
		t.Errorf("no hashing expected: hash=%v verify=%v", res.HashTime, res.VerifyTime)
	}

	rec, err := report.Parse([]byte(readFile(t, filepath.Join(dst, "a", "one.zip.txt"))))
	if err != nil {
		t.Fatalf("parse sidecar: %v", err)
	}
	if !rec.NoVerify || rec.SHA256 != "" || rec.Verified {
		t.Errorf("unexpected no-verify record: %+v", rec)
	}
	if rec.Size != int64(len("one")) {
		t.Errorf("size = %d", rec.Size)
	}
	if exists(filepath.Join(src, "a", "one.zip")) || exists(filepath.Join(src, "two.7z")) {
		t.Error("sources should be deleted after a successful no-verify upload")
	}
	if !exists(filepath.Join(src, "a", "one.zip.txt")) {
		t.Error("local completion note missing")
	}
}

func TestRunNoVerifySkipsMatchingRecord(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "one.zip", "one")
	writeSourceFile(t, src, "two.zip", "two")

	opts := testOptions(src, dst)
	opts.NoVerify = true
	opts.KeepSource = true
	if res, err := Run(context.Background(), opts); err != nil || res.Uploaded != 2 {
		t.Fatalf("first run: res=%+v err=%v", res, err)
	}

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Skipped != 2 || res.Uploaded != 0 || res.Failed != 0 {
		t.Fatalf("second run result: %+v", res)
	}

	// Same size but a different file (and a different modification time) must
	// not be treated as already uploaded.
	p := filepath.Join(src, "one.zip")
	if err := os.WriteFile(p, []byte("ONE"), 0o644); err != nil {
		t.Fatal(err)
	}
	newMod := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p, newMod, newMod); err != nil {
		t.Fatal(err)
	}

	res, err = Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if res.Skipped != 1 || res.Failed != 1 || res.Uploaded != 0 {
		t.Fatalf("third run result: %+v", res)
	}
	if !exists(p) {
		t.Error("conflicting source must be kept")
	}
	if got := readFile(t, filepath.Join(dst, "one.zip")); got != "one" {
		t.Errorf("remote one.zip = %q, want untouched old content", got)
	}
}

func TestRunNoVerifyConflictNeedsForce(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a.zip", "local")
	writeSourceFile(t, dst, "a.zip", "remote")

	opts := testOptions(src, dst)
	opts.NoVerify = true

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Failed != 1 || res.Uploaded != 0 {
		t.Fatalf("result: %+v", res)
	}
	if !exists(filepath.Join(src, "a.zip")) {
		t.Error("conflicting source must be kept")
	}
	if got := readFile(t, filepath.Join(dst, "a.zip")); got != "remote" {
		t.Errorf("remote file changed without --force: %q", got)
	}

	opts.Force = true
	res, err = Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("force run: %v", err)
	}
	if res.Uploaded != 1 || res.Failed != 0 {
		t.Fatalf("force result: %+v", res)
	}
	if got := readFile(t, filepath.Join(dst, "a.zip")); got != "local" {
		t.Errorf("remote file = %q, want local content", got)
	}
	if exists(filepath.Join(src, "a.zip")) {
		t.Error("source should be deleted after a successful forced upload")
	}
}

func TestRunNoVerifyRejectsRecheck(t *testing.T) {
	opts := testOptions(t.TempDir(), filepath.Join(t.TempDir(), "target"))
	opts.NoVerify = true
	opts.Recheck = true
	if _, err := Run(context.Background(), opts); err == nil {
		t.Fatal("expected --no-verify and --recheck to be rejected")
	}
}

func TestRunHandlesEmptyArchive(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "empty.zip", "")

	res, err := Run(context.Background(), testOptions(src, dst))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 1 || res.Failed != 0 {
		t.Fatalf("result: %+v", res)
	}
	rec, err := report.Parse([]byte(readFile(t, filepath.Join(dst, "empty.zip.txt"))))
	if err != nil {
		t.Fatalf("parse sidecar: %v", err)
	}
	if rec.SHA256 != shaOf("") {
		t.Errorf("sha = %q, want %q", rec.SHA256, shaOf(""))
	}
	if exists(filepath.Join(src, "empty.zip")) {
		t.Error("empty archive should be deleted after a verified upload")
	}
}

func TestRunHandlesRarByDefault(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a/old.rar", "archive-rar")

	opts := testOptions(src, dst)
	opts.Extensions = nil // use the built-in zip/7z/rar default

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 1 || res.Failed != 0 {
		t.Fatalf("result: %+v", res)
	}
	if got := readFile(t, filepath.Join(dst, "a", "old.rar")); got != "archive-rar" {
		t.Errorf("remote content = %q", got)
	}
	rec, err := report.Parse([]byte(readFile(t, filepath.Join(dst, "a", "old.rar.txt"))))
	if err != nil {
		t.Fatalf("parse sidecar: %v", err)
	}
	if rec.SHA256 != shaOf("archive-rar") {
		t.Errorf("sha = %q", rec.SHA256)
	}
	if exists(filepath.Join(src, "a", "old.rar")) {
		t.Error("rar archive should be deleted after a verified upload")
	}
	if !exists(filepath.Join(src, "a", "old.rar.txt")) {
		t.Error("local completion note missing")
	}
}

func TestRunRecheckDetectsCorruption(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")
	writeSourceFile(t, src, "a/z.7z", "archive-z")

	opts := testOptions(src, dst)
	opts.KeepSource = true
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Same size, different content: only a real read back can notice this.
	if err := os.WriteFile(filepath.Join(dst, "a", "z.7z"), []byte("corrupt-z"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts.Recheck = true
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Failed != 1 || res.Skipped != 0 {
		t.Fatalf("result: %+v", res)
	}
	if !exists(filepath.Join(src, "a", "z.7z")) {
		t.Error("local file must be kept when the remote copy is corrupt")
	}
}
