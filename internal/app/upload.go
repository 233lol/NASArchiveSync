package app

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/fjf/NASArchiveSync/internal/progress"
	"github.com/fjf/NASArchiveSync/internal/report"
	"github.com/fjf/NASArchiveSync/internal/storage"
	"github.com/fjf/NASArchiveSync/internal/xlog"
)

// uniqueTempRel returns a temporary sibling name that cannot collide with a
// concurrent run working on the same source and target.
func uniqueTempRel(rel string) string {
	var suffix [8]byte
	if _, err := cryptorand.Read(suffix[:]); err == nil {
		return fmt.Sprintf("%s.nassync.%d.%s.tmp", rel, os.Getpid(), hex.EncodeToString(suffix[:]))
	}
	return fmt.Sprintf("%s.nassync.%d.%d.tmp", rel, os.Getpid(), time.Now().UnixNano())
}

func backendReplacesOnRename(backend storage.Backend) bool {
	r, ok := backend.(storage.RenameReplacer)
	return ok && r.RenameReplaces()
}

// replaceOutcome describes a rename that had to move the previous destination
// aside because the backend cannot atomically replace it. The caller decides
// when the old copy may be discarded (commit) or must be restored (rollback).
type replaceOutcome struct {
	backupRel string
}

// commit drops the backup of the previous remote file.
func (r replaceOutcome) commit(backend storage.Backend) {
	if r.backupRel == "" {
		return
	}
	_ = backend.Remove(r.backupRel)
}

// rollback removes the newly written file and restores the previous one.
func (r replaceOutcome) rollback(backend storage.Backend, newRel string) error {
	if r.backupRel == "" {
		return nil
	}
	if err := backend.Remove(newRel); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除新远端文件失败（旧文件保留在 %s）: %w", r.backupRel, err)
	}
	if err := backend.Rename(r.backupRel, newRel); err != nil {
		return fmt.Errorf("恢复旧远端文件失败（旧文件保留在 %s）: %w", r.backupRel, err)
	}
	return nil
}

// rollbackUpload undoes a failed upload. When the previous remote copy was
// moved aside it is restored, which also keeps its old completion note valid;
// otherwise the new copy and its note are removed as before.
func rollbackUpload(backend storage.Backend, remoteRel, sidecarRel string, outcome replaceOutcome, log *xlog.Logger) {
	if outcome.backupRel == "" {
		cleanupRemote(backend, remoteRel, sidecarRel)
		return
	}
	if err := outcome.rollback(backend, remoteRel); err != nil {
		log.Warnf("%v", err)
	}
}

// renameReplacing moves tmpRel to rel and immediately discards any backup. It is
// used for small completion notes, where the previous note is expendable.
func renameReplacing(backend storage.Backend, tmpRel, rel string) error {
	outcome, err := renameReplacingKeepBackup(backend, tmpRel, rel)
	if err != nil {
		return err
	}
	outcome.commit(backend)
	return nil
}

// renameReplacingKeepBackup is like renameReplacing but leaves the previous
// destination at the returned backup path when the backend cannot replace it.
// The caller must call commit or rollback on the result.
func renameReplacingKeepBackup(backend storage.Backend, tmpRel, rel string) (replaceOutcome, error) {
	if backendReplacesOnRename(backend) {
		if err := backend.Rename(tmpRel, rel); err != nil {
			return replaceOutcome{}, err
		}
		return replaceOutcome{}, nil
	}

	backupRel := tmpRel + ".bak"
	_ = backend.Remove(backupRel)
	backedUp := false
	if _, err := backend.Stat(rel); err == nil {
		if err := backend.Rename(rel, backupRel); err != nil {
			return replaceOutcome{}, fmt.Errorf("备份旧远端文件失败: %w", err)
		}
		backedUp = true
	} else if !os.IsNotExist(err) {
		return replaceOutcome{}, fmt.Errorf("检查旧远端文件失败: %w", err)
	}

	if err := backend.Rename(tmpRel, rel); err != nil {
		if backedUp {
			if restoreErr := backend.Rename(backupRel, rel); restoreErr != nil {
				return replaceOutcome{}, fmt.Errorf("重命名失败且恢复旧文件失败（旧文件保留在 %s）: %w", backupRel, err)
			}
		}
		return replaceOutcome{}, err
	}
	if backedUp {
		return replaceOutcome{backupRel: backupRel}, nil
	}
	return replaceOutcome{}, nil
}

func uploadFile(ctx context.Context, backend storage.Backend, localPath, remoteRel string, size int64, overwrite bool, sink progress.Sink, computeHash bool, limiter *bandwidthLimiter, direct bool) (string, replaceOutcome, error) {
	src, err := os.Open(localPath)
	if err != nil {
		return "", replaceOutcome{}, fmt.Errorf("打开源文件失败: %w", err)
	}
	defer src.Close()

	tmpRel := ""
	directWrite := false
	var dst storage.Writer
	if direct {
		if creator, ok := backend.(storage.AtomicCreator); ok {
			dst, err = creator.CreateAtomic(remoteRel)
			if err != nil {
				return "", replaceOutcome{}, fmt.Errorf("创建远端文件失败: %w", err)
			}
			directWrite = true
		}
	}
	if !directWrite {
		tmpRel = uniqueTempRel(remoteRel)
		_ = backend.Remove(tmpRel)
		dst, err = backend.Create(tmpRel)
		if err != nil {
			return "", replaceOutcome{}, fmt.Errorf("创建远端临时文件失败: %w", err)
		}
	}
	if sized, ok := dst.(storage.SizedWriter); ok {
		sized.SetSize(size)
	}

	// Hash the bytes while they are copied, so the local file only has to be
	// read once for a new upload. --no-verify skips hashing entirely.
	h := sha256.New()
	var reader io.Reader = progress.NewReader(ctxReader{ctx: ctx, r: src}, sink)
	reader = newLimitedReader(ctx, reader, limiter)
	if computeHash {
		reader = io.TeeReader(reader, h)
	}
	// The SFTP client pipelines writes only when it can tell how many bytes are
	// coming. A LimitedReader also guards against a source that grew after it
	// was stat'ed.
	limited := &io.LimitedReader{R: reader, N: size}
	written, copyErr := io.CopyBuffer(dst, limited, make([]byte, bufferSizeFor(size)))
	if copyErr == nil && written != size {
		copyErr = fmt.Errorf("写入字节数不符: 已写入 %d，期望 %d", written, size)
	}
	if copyErr == nil {
		copyErr = dst.Sync()
	}
	if closeErr := dst.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		if !directWrite {
			_ = backend.Remove(tmpRel)
		}
		return "", replaceOutcome{}, fmt.Errorf("上传失败: %w", copyErr)
	}

	if directWrite {
		if !computeHash {
			return "", replaceOutcome{}, nil
		}
		return hex.EncodeToString(h.Sum(nil)), replaceOutcome{}, nil
	}
	var outcome replaceOutcome
	if overwrite {
		outcome, err = renameReplacingKeepBackup(backend, tmpRel, remoteRel)
	} else {
		err = backend.Rename(tmpRel, remoteRel)
	}
	if err != nil {
		_ = backend.Remove(tmpRel)
		return "", replaceOutcome{}, fmt.Errorf("重命名远端临时文件失败: %w", err)
	}
	if !computeHash {
		return "", outcome, nil
	}
	return hex.EncodeToString(h.Sum(nil)), outcome, nil
}

func bufferSizeFor(size int64) int {
	n := maxCopyBufferSize
	if size > 0 && size < int64(n) {
		n = int(size)
	}
	if n < minCopyBufferSize {
		n = minCopyBufferSize
	}
	return n
}

func writeRecord(backend storage.Backend, rel, content string) error {
	dir := relDirOf(rel)
	if dir != "" {
		if err := backend.MkdirAll(dir); err != nil {
			return err
		}
	}
	if creator, ok := backend.(storage.AtomicCreator); ok {
		dst, err := creator.CreateAtomic(rel)
		if err != nil {
			return err
		}
		return writeRecordContent(dst, content, nil)
	}

	tmpRel := uniqueTempRel(rel)
	_ = backend.Remove(tmpRel)
	dst, err := backend.Create(tmpRel)
	if err != nil {
		return err
	}
	if err := writeRecordContent(dst, content, func() error {
		return backend.Remove(tmpRel)
	}); err != nil {
		return err
	}

	if err := renameReplacing(backend, tmpRel, rel); err != nil {
		_ = backend.Remove(tmpRel)
		return err
	}
	return nil
}

func writeRecordContent(dst storage.Writer, content string, cleanup func() error) error {
	_, writeErr := io.WriteString(dst, content)
	if writeErr == nil {
		writeErr = dst.Sync()
	}
	if closeErr := dst.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		if cleanup != nil {
			_ = cleanup()
		}
		return writeErr
	}
	return nil
}

func readRecord(ctx context.Context, backend storage.Backend, rel string) ([]byte, *report.Record, error) {
	src, err := backend.Open(rel)
	if err != nil {
		return nil, nil, err
	}
	defer src.Close()

	data, err := io.ReadAll(io.LimitReader(ctxReader{ctx: ctx, r: src}, recordSizeLimit))
	if err != nil {
		return nil, nil, err
	}
	rec, err := report.Parse(data)
	if err != nil {
		return nil, nil, err
	}
	return data, rec, nil
}

// writeLocalRecord stores a copy of the completion note next to the source
// archive. It is written atomically and survives deletion of the archive.
func writeLocalRecord(p string, data []byte) error {
	tmp := uniqueTempRel(p)
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	backup := tmp + ".bak"
	_ = os.Remove(backup)
	backedUp := false
	if _, err := os.Stat(p); err == nil {
		if err := os.Rename(p, backup); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		backedUp = true
	} else if !os.IsNotExist(err) {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		if backedUp {
			_ = os.Rename(backup, p)
		}
		_ = os.Remove(tmp)
		return err
	}
	if backedUp {
		_ = os.Remove(backup)
	}
	return nil
}

func readLocalRecord(archivePath string) ([]byte, *report.Record, error) {
	f, err := os.Open(archivePath + ".txt")
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, recordSizeLimit))
	if err != nil {
		return nil, nil, err
	}
	rec, err := report.Parse(data)
	if err != nil {
		return nil, nil, err
	}
	return data, rec, nil
}

// ensureRecords writes the completion note to the sides selected by --txt.
// remoteOK/localOK tell it that an up to date note already exists there.
func ensureRecords(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, sidecarRel, localNote string, content []byte, remoteOK, localOK bool) error {
	if o.Txt.WritesRemote() && !remoteOK {
		if err := retryDo(ctx, o, log, backend, "写入远端记录", func() error {
			return writeRecord(backend, sidecarRel, string(content))
		}); err != nil {
			return fmt.Errorf("写入远端上传记录失败: %w", err)
		}
	}
	if o.Txt.WritesLocal() && !localOK {
		if err := writeLocalRecord(localNote, content); err != nil {
			return fmt.Errorf("写入本地上传记录失败: %w", err)
		}
	}
	return nil
}

func conflictError(remotePath, reason string) error {
	return fmt.Errorf("目标已存在但%s: %s（确认无误后可加 --force 覆盖）", reason, remotePath)
}

func hashLocalFile(ctx context.Context, p string, sink progress.Sink, size int64) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return hashReader(ctx, f, sink, size, nil)
}

func hashRemoteFile(ctx context.Context, backend storage.Backend, rel string, sink progress.Sink, size int64, concurrency int, pool *chunkPool, limiter *bandwidthLimiter) (string, error) {
	if rr, ok := backend.(storage.RangeReader); ok && concurrency > 1 && size >= parallelHashMinSize {
		// Range hashing only fetches the bytes the local file claims to have.
		// Refuse to hash a remote file with a different size, otherwise extra
		// trailing bytes on the server would be silently ignored.
		info, err := backend.Stat(rel)
		if err != nil {
			return "", err
		}
		if info.IsDir {
			return "", fmt.Errorf("远端路径是目录: %s", backend.Location(rel))
		}
		if info.Size != size {
			return "", fmt.Errorf("远端文件大小不符（本地 %d，远端 %d）: %s", size, info.Size, backend.Location(rel))
		}
		return hashRemoteRanges(ctx, rr, rel, sink, size, concurrency, pool, limiter)
	}
	f, err := backend.Open(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return hashReader(ctx, f, sink, size, limiter)
}

func hashReader(ctx context.Context, r io.Reader, sink progress.Sink, size int64, limiter *bandwidthLimiter) (string, error) {
	h := sha256.New()
	reader := io.Reader(progress.NewReader(ctxReader{ctx: ctx, r: r}, sink))
	reader = newLimitedReader(ctx, reader, limiter)
	if _, err := io.CopyBuffer(h, reader, make([]byte, bufferSizeFor(size))); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func cleanupRemote(backend storage.Backend, remoteRel, sidecarRel string) {
	_ = backend.Remove(remoteRel)
	_ = backend.Remove(sidecarRel)
	cleanupTempFiles(backend, remoteRel, sidecarRel)
}

func cleanupTempFiles(backend storage.Backend, remoteRel, sidecarRel string) {
	_ = backend.Remove(remoteRel + ".nassync.tmp")
	_ = backend.Remove(sidecarRel + ".tmp")
}

func deleteLocalFile(o Options, log *xlog.Logger, p string, size int64, modTime time.Time) {
	if o.KeepSource {
		return
	}
	// Refuse to delete a file that changed while it was being uploaded: the
	// verified remote copy no longer represents the local file.
	fi, err := os.Stat(p)
	if err != nil {
		log.Warnf("检查本地文件失败，未删除: %v", err)
		return
	}
	if fi.Size() != size || !fi.ModTime().Equal(modTime) {
		log.Warnf("源文件在上传过程中发生变化，已保留本地文件: %s", p)
		return
	}
	if err := os.Remove(p); err != nil {
		log.Warnf("删除本地文件失败（远端已完成，可稍后手动删除）: %v", err)
		return
	}
	log.Debugf("已删除本地文件: %s", p)
}
