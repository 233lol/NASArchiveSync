package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fjf/NASArchiveSync/internal/humanize"
	"github.com/fjf/NASArchiveSync/internal/report"
	"github.com/fjf/NASArchiveSync/internal/storage"
	"github.com/fjf/NASArchiveSync/internal/xlog"
)

func uploadAndVerify(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, st *fileState, fc fileContext, newHash, newUpload counterFactory, quiet bool) fileResult {
	up, err := uploadOnly(ctx, o, log, backend, st, fc, newUpload)
	if err != nil {
		return fileResult{outcome: OutcomeFailed, file: st.file.Rel, size: st.size, err: err, uploadTime: up.duration}
	}
	return verifyUploaded(ctx, o, log, backend, st, up, fc, newHash, quiet)
}

func uploadOnly(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, st *fileState, fc fileContext, newUpload counterFactory) (uploadResult, error) {
	if st.paths.relDir != "" {
		if err := retryDo(ctx, o, log, backend, "创建远端目录", func() error {
			return backend.MkdirAll(st.paths.relDir)
		}); err != nil {
			return uploadResult{}, fmt.Errorf("创建远端目录失败: %w", err)
		}
	}

	log.Debugf("上传 %s -> %s", st.file.Path, st.paths.remotePath)
	counter := newUpload(fc.label("上传", st.file.Rel), st.size)
	uploadStarted := time.Now()
	up, err := retryValue(ctx, o, log, backend, "上传", func() (uploadResult, error) {
		sha, outcome, err := uploadFile(ctx, backend, st.file.Path, st.paths.remoteRel, st.size, o.Force, counter, true, o.limiter, !st.remoteExists)
		if err != nil {
			return uploadResult{}, err
		}
		return uploadResult{sha: sha, outcome: outcome}, nil
	})
	up.duration = time.Since(uploadStarted)
	if err != nil {
		counter.Abort()
		// The previous remote copy (if any) and its record are still valid: only
		// clean up temporary files here.
		cleanupTempFiles(backend, st.paths.remoteRel, st.paths.sidecarRel)
		return up, err
	}
	counter.Finish()

	// The hash was computed while uploading. Make sure the source did not
	// change underneath us, otherwise the uploaded bytes may be a mix.
	after, statErr := os.Stat(st.file.Path)
	if statErr != nil {
		rollbackUpload(backend, st.paths.remoteRel, st.paths.sidecarRel, up.outcome, log)
		return up, fmt.Errorf("上传后检查源文件失败: %w", statErr)
	}
	if after.Size() != st.size || !after.ModTime().Equal(st.info.ModTime()) {
		rollbackUpload(backend, st.paths.remoteRel, st.paths.sidecarRel, up.outcome, log)
		return up, fmt.Errorf("源文件在上传过程中发生变化，已保留本地文件: %s", st.file.Path)
	}
	log.Debugf("上传耗时: %s", humanize.Duration(up.duration))
	return up, nil
}

func verifyUploaded(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, st *fileState, up uploadResult, fc fileContext, newHash counterFactory, quiet bool) fileResult {
	res := fileResult{file: st.file.Rel, size: st.size, uploadTime: up.duration}

	counter := newHash(fc.label("校验远端", st.file.Rel), st.size)
	verifyStart := time.Now()
	remoteSHA, err := retryValue(ctx, o, log, backend, "校验远端", func() (string, error) {
		return hashRemoteFile(ctx, backend, st.paths.remoteRel, counter, st.size, o.VerifyConcurrency, o.verifyPool, o.limiter)
	})
	res.verifyTime = time.Since(verifyStart)
	if err != nil {
		counter.Abort()
		rollbackUpload(backend, st.paths.remoteRel, st.paths.sidecarRel, up.outcome, log)
		res.outcome = OutcomeFailed
		res.err = fmt.Errorf("读取远端文件校验失败: %w", err)
		return res
	}
	counter.Finish()

	if remoteSHA != up.sha {
		rollbackUpload(backend, st.paths.remoteRel, st.paths.sidecarRel, up.outcome, log)
		res.outcome = OutcomeFailed
		res.err = fmt.Errorf("SHA256 不一致（本地 %s，远端 %s），已清理本次上传的远端文件并保留本地文件",
			shortHash(up.sha), shortHash(remoteSHA))
		return res
	}
	// The new copy is verified: the previous remote version is no longer
	// needed, even if writing the completion note below fails.
	up.outcome.commit(backend)
	log.Debugf("远端 SHA256: %s", remoteSHA)

	record := report.Record{
		FileName:      filepath.Base(st.file.Path),
		TargetPath:    st.paths.remotePath,
		Size:          st.size,
		SHA256:        up.sha,
		UploadedAt:    now(o),
		SourcePath:    st.file.Path,
		SourceModTime: st.info.ModTime(),
		Duration:      up.duration,
		ToolVersion:   version(o),
		Verified:      true,
	}
	content := []byte(record.Render())
	if err := ensureRecords(ctx, o, log, backend, st.paths.sidecarRel, st.paths.localNote, content, false, false); err != nil {
		res.outcome = OutcomeFailed
		res.err = fmt.Errorf("远端文件已上传并校验通过，但保存上传记录失败，已保留本地源文件: %w", err)
		return res
	}

	if !quiet {
		log.Successf("上传完成并校验一致: %s", st.paths.remotePath)
	}
	deleteLocalFile(o, log, st.file.Path, st.size, st.info.ModTime())
	res.outcome = OutcomeUploaded
	return res
}
