package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fjf/NASArchiveSync/internal/humanize"
	"github.com/fjf/NASArchiveSync/internal/report"
	"github.com/fjf/NASArchiveSync/internal/scan"
	"github.com/fjf/NASArchiveSync/internal/storage"
	"github.com/fjf/NASArchiveSync/internal/xlog"
)

// planState caches read-only checks performed while printing a dry run plan.
type planState struct {
	rootChecked bool
	rootExists  bool
	dirs        map[string]bool
}

func processFile(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, file scan.File, fc fileContext, plan *planState, newHash, newUpload counterFactory, quiet bool) fileResult {
	st, err := inspectFile(ctx, o, log, backend, file)
	if err != nil {
		return fileResult{outcome: OutcomeFailed, file: file.Rel, err: err}
	}

	if o.DryRun {
		if err := planFile(o, log, backend, st, plan); err != nil {
			return fileResult{outcome: OutcomeFailed, file: file.Rel, size: st.size, err: err}
		}
		return fileResult{outcome: OutcomePlanned, file: file.Rel, size: st.size}
	}

	if o.NoVerify {
		if !o.Force && st.remoteExists {
			return handleSkipNoVerify(ctx, o, log, backend, st)
		}
		return uploadAndRecordNoVerify(ctx, o, log, backend, st, fc, newUpload)
	}

	if !o.Force && st.remoteExists {
		return handleSkip(ctx, o, log, backend, st, fc, newHash, quiet)
	}
	return uploadAndVerify(ctx, o, log, backend, st, fc, newHash, newUpload, quiet)
}

// recordTargetMatches reports whether a completion note belongs to the current
// target. Local notes live next to the source archive and are shared by every
// target, so a matching hash alone does not prove that this target already
// holds the archive.
func recordTargetMatches(rec *report.Record, targetPath string) bool {
	return rec != nil && rec.TargetPath != "" && rec.TargetPath == targetPath
}

// recordMatchesFile reports whether a completion note describes this exact
// source file. Without --no-verify the SHA256 decides; in no-verify mode there
// is no hash, so size and source modification time (stored to the second) are
// the only available evidence.
func recordMatchesFile(rec *report.Record, size int64, mod time.Time) bool {
	if rec == nil || rec.Size != size {
		return false
	}
	if rec.SourceModTime.IsZero() {
		return false
	}
	return rec.SourceModTime.Equal(mod.Truncate(time.Second))
}

func handleSkipNoVerify(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, st *fileState) fileResult {
	res := fileResult{file: st.file.Rel, size: st.size}

	remoteMatch := st.recErr == nil && recordTargetMatches(st.rec, st.paths.remotePath) &&
		recordMatchesFile(st.rec, st.size, st.info.ModTime())
	localMatch := st.localRecErr == nil && recordTargetMatches(st.localRec, st.paths.remotePath) &&
		recordMatchesFile(st.localRec, st.size, st.info.ModTime())

	// The note records the local size; the remote file itself must still match
	// it before a note is trusted. A size mismatch is cheap to detect and
	// cannot be explained by a valid upload.
	if st.remoteInfo.Size != st.size {
		res.outcome = OutcomeFailed
		res.err = conflictError(st.paths.remotePath, "大小与本地文件不同")
		return res
	}

	var content []byte
	remoteOK, localOK := false, false
	switch {
	case remoteMatch:
		content, remoteOK, localOK = st.recData, true, localMatch
	case localMatch && os.IsNotExist(st.recErr):
		// A local note for this exact target is the only evidence available
		// when the remote side has no note, as with --txt local.
		content, localOK = st.localRecData, true
	}
	if content == nil {
		reason := "已关闭校验且没有可用的上传记录，无法确认内容是否一致"
		if st.recErr == nil || localMatch {
			reason = "已关闭校验且上传记录与当前目标不一致，无法确认内容是否一致"
		}
		res.outcome = OutcomeFailed
		res.err = conflictError(st.paths.remotePath, reason)
		return res
	}

	if err := ensureRecords(ctx, o, log, backend, st.paths.sidecarRel, st.paths.localNote, content, remoteOK, localOK); err != nil {
		res.outcome = OutcomeFailed
		res.err = fmt.Errorf("补写上传记录失败，已保留本地源文件: %w", err)
		return res
	}
	log.Successf("远端已存在且记录一致，跳过上传（已关闭校验）: %s", st.paths.remotePath)
	deleteLocalFile(o, log, st.file.Path, st.size, st.info.ModTime())
	res.outcome = OutcomeSkipped
	return res
}

func uploadAndRecordNoVerify(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, st *fileState, fc fileContext, newUpload counterFactory) fileResult {
	res := fileResult{file: st.file.Rel, size: st.size}

	if st.paths.relDir != "" {
		if err := retryDo(ctx, o, log, backend, "创建远端目录", func() error {
			return backend.MkdirAll(st.paths.relDir)
		}); err != nil {
			res.outcome = OutcomeFailed
			res.err = fmt.Errorf("创建远端目录失败: %w", err)
			return res
		}
	}

	log.Debugf("上传 %s -> %s（已关闭校验）", st.file.Path, st.paths.remotePath)
	counter := newUpload(fc.label("上传", st.file.Rel), st.size)
	uploadStarted := time.Now()
	up, err := retryValue(ctx, o, log, backend, "上传", func() (uploadResult, error) {
		_, outcome, err := uploadFile(ctx, backend, st.file.Path, st.paths.remoteRel, st.size, o.Force, counter, false, o.limiter, !st.remoteExists)
		if err != nil {
			return uploadResult{}, err
		}
		return uploadResult{outcome: outcome}, nil
	})
	res.uploadTime = time.Since(uploadStarted)
	if err != nil {
		counter.Abort()
		cleanupTempFiles(backend, st.paths.remoteRel, st.paths.sidecarRel)
		res.outcome = OutcomeFailed
		res.err = err
		return res
	}
	counter.Finish()

	after, statErr := os.Stat(st.file.Path)
	if statErr != nil {
		rollbackUpload(backend, st.paths.remoteRel, st.paths.sidecarRel, up.outcome, log)
		res.outcome = OutcomeFailed
		res.err = fmt.Errorf("上传后检查源文件失败: %w", statErr)
		return res
	}
	if after.Size() != st.size || !after.ModTime().Equal(st.info.ModTime()) {
		rollbackUpload(backend, st.paths.remoteRel, st.paths.sidecarRel, up.outcome, log)
		res.outcome = OutcomeFailed
		res.err = fmt.Errorf("源文件在上传过程中发生变化，已保留本地文件: %s", st.file.Path)
		return res
	}

	record := report.Record{
		FileName:      filepath.Base(st.file.Path),
		TargetPath:    st.paths.remotePath,
		Size:          st.size,
		UploadedAt:    now(o),
		SourcePath:    st.file.Path,
		SourceModTime: st.info.ModTime(),
		Duration:      res.uploadTime,
		ToolVersion:   version(o),
		NoVerify:      true,
	}
	content := []byte(record.Render())
	if err := ensureRecords(ctx, o, log, backend, st.paths.sidecarRel, st.paths.localNote, content, false, false); err != nil {
		// The archive itself is complete, so keep it and drop the old backup; a
		// later run can repair the missing note from the remote file.
		up.outcome.commit(backend)
		res.outcome = OutcomeFailed
		res.err = fmt.Errorf("远端文件已上传，但保存上传记录失败，已保留本地源文件: %w", err)
		return res
	}
	up.outcome.commit(backend)

	log.Successf("上传完成（已关闭校验，未读回远端）: %s", st.paths.remotePath)
	deleteLocalFile(o, log, st.file.Path, st.size, st.info.ModTime())
	res.outcome = OutcomeUploaded
	return res
}

func handleSkip(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, st *fileState, fc fileContext, newHash counterFactory, quiet bool) fileResult {
	res := fileResult{file: st.file.Rel, size: st.size}

	hashCounter := newHash(fc.label("检查", st.file.Rel), st.size)
	hashStart := time.Now()
	localSHA, err := hashLocalFile(ctx, st.file.Path, hashCounter, st.size)
	res.hashTime = time.Since(hashStart)
	if err != nil {
		hashCounter.Abort()
		res.outcome = OutcomeFailed
		res.err = fmt.Errorf("计算本地 SHA256 失败: %w", err)
		return res
	}
	hashCounter.Finish()
	log.Debugf("本地 SHA256: %s", localSHA)

	remoteMatch := st.recErr == nil && recordTargetMatches(st.rec, st.paths.remotePath) &&
		st.rec.SHA256 == localSHA && st.rec.Size == st.size
	localMatch := st.localRecErr == nil && recordTargetMatches(st.localRec, st.paths.remotePath) &&
		st.localRec.SHA256 == localSHA && st.localRec.Size == st.size

	// A completion note records the source size, but the remote file can still
	// be truncated or replaced. Check the cheap evidence first.
	if st.remoteInfo.Size != st.size {
		res.outcome = OutcomeFailed
		res.err = conflictError(st.paths.remotePath, "大小与本地文件不同")
		return res
	}

	var content []byte
	remoteOK, localOK := false, false
	verified := false
	switch {
	case remoteMatch:
		content, remoteOK, localOK = st.recData, true, localMatch
	case localMatch && os.IsNotExist(st.recErr):
		// The local note belongs to this exact target and there is no remote
		// note (for example --txt local), so it is safe to reuse it.
		content, localOK = st.localRecData, true
	}

	if content == nil {
		// The remote side has no note, has an unreadable note, or has a note
		// that contradicts the local file. Read the remote copy back before
		// trusting either note. This also covers local notes written for a
		// different target.
		log.Debugf("没有匹配的上传记录，读回远端文件判断是否已上传: %s", st.paths.remotePath)
		checkCounter := newHash(fc.label("校验已存在", st.file.Rel), st.size)
		verifyStart := time.Now()
		remoteSHA, hashErr := retryValue(ctx, o, log, backend, "校验远端", func() (string, error) {
			return hashRemoteFile(ctx, backend, st.paths.remoteRel, checkCounter, st.size, o.VerifyConcurrency, o.verifyPool, o.limiter)
		})
		res.verifyTime += time.Since(verifyStart)
		if hashErr != nil {
			checkCounter.Abort()
			res.outcome = OutcomeFailed
			res.err = fmt.Errorf("读回远端文件校验失败: %w", hashErr)
			return res
		}
		checkCounter.Finish()
		verified = true
		if remoteSHA != localSHA {
			res.outcome = OutcomeFailed
			res.err = conflictError(st.paths.remotePath, "SHA256 与本地文件不同")
			return res
		}
		if localMatch {
			content, localOK = st.localRecData, true
		} else {
			uploadedAt := st.remoteInfo.ModTime
			if uploadedAt.IsZero() {
				uploadedAt = now(o)
			}
			content = []byte(report.Record{
				FileName:      filepath.Base(st.file.Path),
				TargetPath:    st.paths.remotePath,
				Size:          st.size,
				SHA256:        localSHA,
				UploadedAt:    uploadedAt,
				SourcePath:    st.file.Path,
				SourceModTime: st.info.ModTime(),
				ToolVersion:   version(o),
				Verified:      true,
			}.Render())
		}
	}

	if content == nil {
		res.outcome = OutcomeFailed
		res.err = conflictError(st.paths.remotePath, "没有可用的上传记录")
		return res
	}

	if o.Recheck && !verified {
		checkCounter := newHash(fc.label("重新校验", st.file.Rel), st.size)
		verifyStart := time.Now()
		remoteSHA, hashErr := retryValue(ctx, o, log, backend, "重新校验远端", func() (string, error) {
			return hashRemoteFile(ctx, backend, st.paths.remoteRel, checkCounter, st.size, o.VerifyConcurrency, o.verifyPool, o.limiter)
		})
		res.verifyTime += time.Since(verifyStart)
		if hashErr != nil {
			checkCounter.Abort()
			res.outcome = OutcomeFailed
			res.err = fmt.Errorf("重新校验远端文件失败: %w", hashErr)
			return res
		}
		checkCounter.Finish()
		if remoteSHA != localSHA {
			res.outcome = OutcomeFailed
			res.err = fmt.Errorf("远端文件与上传记录不一致（SHA256 不同），未删除本地文件: %s", st.paths.remotePath)
			return res
		}
	}

	if err := ensureRecords(ctx, o, log, backend, st.paths.sidecarRel, st.paths.localNote, content, remoteOK, localOK); err != nil {
		res.outcome = OutcomeFailed
		res.err = fmt.Errorf("补写上传记录失败，已保留本地源文件: %w", err)
		return res
	}
	if !quiet {
		log.Successf("远端已存在且校验一致，跳过上传: %s", st.paths.remotePath)
	}
	deleteLocalFile(o, log, st.file.Path, st.size, st.info.ModTime())
	res.outcome = OutcomeSkipped
	return res
}

func planFile(o Options, log *xlog.Logger, backend storage.Backend, st *fileState, plan *planState) error {
	file := st.file
	relDir := st.paths.relDir
	remoteRel := st.paths.remoteRel
	remoteExists := st.remoteExists
	hasRecord := st.remoteInfo.Size == st.size &&
		((st.recErr == nil && recordTargetMatches(st.rec, st.paths.remotePath) &&
			st.rec.Size == st.size) ||
			(os.IsNotExist(st.recErr) && st.localRecErr == nil &&
				recordTargetMatches(st.localRec, st.paths.remotePath) && st.localRec.Size == st.size))
	willUpload := !remoteExists || o.Force
	willSkip := remoteExists && !o.Force && hasRecord
	willConflict := false
	if o.NoVerify && remoteExists && !o.Force {
		// Without hashes the same size and source mtime decide, so the dry run
		// can predict the outcome exactly.
		remoteMatch := st.recErr == nil && recordTargetMatches(st.rec, st.paths.remotePath) &&
			recordMatchesFile(st.rec, st.size, st.info.ModTime())
		localMatch := st.localRecErr == nil && recordTargetMatches(st.localRec, st.paths.remotePath) &&
			recordMatchesFile(st.localRec, st.size, st.info.ModTime())
		willSkip = st.remoteInfo.Size == st.size &&
			(remoteMatch || (localMatch && os.IsNotExist(st.recErr)))
		willConflict = !willSkip
	}

	if !plan.rootChecked {
		plan.rootChecked = true
		_, err := backend.Stat("")
		plan.rootExists = err == nil
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("检查目标根目录失败: %w", err)
		}
		if !plan.rootExists {
			log.Infof("[计划] 创建目标根目录: %s", backend.Location(""))
		}
	}
	if relDir != "" {
		dirExists, checked := plan.dirs[relDir]
		if !checked {
			_, err := backend.Stat(relDir)
			if err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("检查目标目录失败: %w", err)
			}
			dirExists = err == nil
			plan.dirs[relDir] = dirExists
		}
		if !dirExists {
			log.Infof("[计划] 创建目录: %s", backend.Location(relDir))
		}
	}

	switch {
	case willUpload && remoteExists:
		log.Infof("[计划] 目标已存在，将覆盖上传: %s", backend.Location(remoteRel))
	case willUpload:
		log.Infof("[计划] 上传: %s (%s) -> %s", file.Path, humanize.Bytes(st.size), backend.Location(remoteRel))
	case willSkip:
		log.Infof("[计划] 目标已存在且存在本目标的上传记录，正式运行将按记录判断是否跳过: %s", backend.Location(remoteRel))
	case willConflict:
		log.Infof("[计划] 目标已存在但没有本目标的上传记录，正式运行将报冲突并保留源文件（可加 --force 覆盖）: %s", backend.Location(remoteRel))
	default:
		// Verification is enabled and there is no usable note: the real run
		// reads the remote file back and decides between skipping and a
		// conflict.
		log.Infof("[计划] 目标已存在但没有本目标的上传记录，正式运行将读回远端判断：一致则跳过，不一致则报冲突: %s", backend.Location(remoteRel))
	}

	switch {
	case o.NoVerify:
		log.Infof("[计划] 不计算 SHA256，上传后不读回校验（--no-verify）")
	case willSkip && o.Recheck:
		log.Infof("[计划] 跳过前重新读回校验 SHA256（--recheck）")
	case willSkip:
		log.Infof("[计划] 跳过时不再读回校验（未指定 --recheck）")
	case willUpload:
		log.Infof("[计划] 上传后读回校验 SHA256")
	default:
		log.Infof("[计划] 读回远端校验 SHA256 以判断是否已上传")
	}

	if !willConflict {
		prefix := ""
		if remoteExists && !o.Force && !hasRecord {
			prefix = "若判定已上传，"
		}
		switch o.Txt {
		case TxtBoth:
			log.Infof("[计划] %s写入远端记录: %s", prefix, backend.Location(report.SidecarName(remoteRel)))
			log.Infof("[计划] %s写入本地记录: %s", prefix, file.Path+".txt")
		case TxtRemote:
			log.Infof("[计划] %s写入远端记录: %s", prefix, backend.Location(report.SidecarName(remoteRel)))
		case TxtLocal:
			log.Infof("[计划] %s写入本地记录: %s", prefix, file.Path+".txt")
		default:
			log.Infof("[计划] 不生成上传记录 txt")
		}
	}

	if !o.KeepSource {
		switch {
		case willUpload || willSkip:
			log.Infof("[计划] 删除源文件: %s", file.Path)
		case willConflict:
			log.Infof("[计划] 冲突时保留源文件: %s", file.Path)
		default:
			log.Infof("[计划] 若判定已上传则删除源文件: %s", file.Path)
		}
	}
	return nil
}
