// Package app implements the upload pipeline: scan, hash, upload, verify,
// write a completion note and finally delete the local archive.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fjf/NASArchiveSync/internal/backends"
	"github.com/fjf/NASArchiveSync/internal/humanize"
	"github.com/fjf/NASArchiveSync/internal/progress"
	"github.com/fjf/NASArchiveSync/internal/report"
	"github.com/fjf/NASArchiveSync/internal/scan"
	"github.com/fjf/NASArchiveSync/internal/storage"
	"github.com/fjf/NASArchiveSync/internal/xlog"
)

const (
	copyBufferSize   = 512 * 1024
	hashBarThreshold = 16 * 1024 * 1024
	recordSizeLimit  = 1 << 20
)

// SMBOptions carries SMB credentials.
type SMBOptions struct {
	User     string
	Password string
	Domain   string
	Port     int
}

// Options configures a Run.
type Options struct {
	Source string
	Target string
	Depth  int
	// WithSourceDir uploads below a folder named after the source directory
	// instead of directly into the target root. For example source C:\game\a
	// and target \\nas\share\game produces \\nas\share\game\a\....
	WithSourceDir bool
	Include       []string
	Exclude       []string
	Extensions    []string
	DryRun        bool
	KeepSource    bool
	Force         bool
	Recheck       bool
	CaseSensitive bool
	SMB           SMBOptions
	// SMBMode is "auto" (default), "native" or "builtin". Native means the
	// operating system's SMB redirector is used and existing Windows
	// credentials are reused.
	SMBMode          string
	SMBAuthRequested bool
	Txt              TxtMode
	NoProgress       bool
	Version          string
	Logger           *xlog.Logger
	ProgressOut      io.Writer
	Now              func() time.Time

	// sourcePrefix is the source directory name used when WithSourceDir is set.
	// It is derived in Run and prefixes every remote relative path.
	sourcePrefix string
}

// Outcome describes what happened to a single archive.
type Outcome int

const (
	OutcomeFailed Outcome = iota
	OutcomeUploaded
	OutcomeSkipped
	OutcomePlanned
)

// TxtMode controls where the completion note (txt) is written.
type TxtMode string

const (
	TxtBoth   TxtMode = "both"
	TxtLocal  TxtMode = "local"
	TxtRemote TxtMode = "remote"
	TxtNone   TxtMode = "none"
)

// ParseTxtMode validates a --txt value. An empty value means "both".
func ParseTxtMode(s string) (TxtMode, error) {
	switch TxtMode(strings.ToLower(strings.TrimSpace(s))) {
	case "", TxtBoth:
		return TxtBoth, nil
	case TxtLocal:
		return TxtLocal, nil
	case TxtRemote:
		return TxtRemote, nil
	case TxtNone:
		return TxtNone, nil
	default:
		return "", fmt.Errorf("上传记录保存方式无效: %q（可选 both、local、remote、none）", s)
	}
}

func (m TxtMode) WritesLocal() bool  { return m == TxtBoth || m == TxtLocal }
func (m TxtMode) WritesRemote() bool { return m == TxtBoth || m == TxtRemote }

// planState caches read-only checks performed while printing a dry run plan.
type planState struct {
	rootChecked bool
	rootExists  bool
	dirs        map[string]bool
}

// Result is the summary of a Run.
type Result struct {
	Total    int
	Uploaded int
	Skipped  int
	Planned  int
	Failed   int
	Bytes    int64
	Duration time.Duration
	DryRun   bool
	Failures []string
}

// Run executes the whole pipeline. Per file errors are collected in the
// returned Result; a non nil error means the run could not start or was
// interrupted.
func Run(ctx context.Context, o Options) (Result, error) {
	started := time.Now()
	log := o.Logger
	if log == nil {
		log = xlog.New(io.Discard, false, false)
	}

	res := Result{DryRun: o.DryRun}

	txtMode, err := ParseTxtMode(string(o.Txt))
	if err != nil {
		return res, err
	}
	o.Txt = txtMode

	srcRoot, err := filepath.Abs(o.Source)
	if err != nil {
		return res, fmt.Errorf("解析源目录失败: %w", err)
	}
	fi, err := os.Stat(srcRoot)
	if err != nil {
		return res, fmt.Errorf("访问源目录失败: %w", err)
	}
	if !fi.IsDir() {
		return res, fmt.Errorf("源路径不是目录: %s", srcRoot)
	}

	if o.WithSourceDir {
		name, nameErr := sourceDirName(srcRoot)
		if nameErr != nil {
			return res, nameErr
		}
		o.sourcePrefix = name
	}

	target, err := storage.ParseTarget(o.Target)
	if err != nil {
		return res, err
	}

	nativeAccess, err := useNativeSMB(target, o)
	if err != nil {
		return res, err
	}
	if nativeAccess && o.SMBAuthRequested {
		log.Warnf("系统级 SMB 访问会复用 Windows 已登录凭据，--smb-user/--smb-pass 等参数将被忽略")
	}

	scanOpts := scan.Options{
		Root:          srcRoot,
		MaxDepth:      o.Depth,
		Include:       o.Include,
		Exclude:       o.Exclude,
		Extensions:    o.Extensions,
		CaseSensitive: o.CaseSensitive,
	}
	if target.Scheme == "local" {
		if localRoot, absErr := filepath.Abs(target.Path); absErr == nil {
			if samePath(localRoot, srcRoot) {
				return res, fmt.Errorf("本地目标目录不能与源目录相同: %s", localRoot)
			}
			// With --with-source-dir the upload root is target/<source name>;
			// reject the case where that resolves back to the source directory,
			// which would overwrite the archives being uploaded.
			if o.sourcePrefix != "" {
				effective := filepath.Join(localRoot, o.sourcePrefix)
				if samePath(effective, srcRoot) {
					return res, fmt.Errorf("在目标目录下创建源目录同名文件夹后会与源目录相同: %s（请改用上一级目标目录或去掉 --with-source-dir）", effective)
				}
			}
			if isWithin(localRoot, srcRoot) {
				scanOpts.SkipDirs = append(scanOpts.SkipDirs, localRoot)
				log.Debugf("目标目录位于源目录内，已跳过: %s", localRoot)
			}
		}
	}

	files, warnings, err := scan.Scan(scanOpts)
	if err != nil {
		return res, err
	}
	for _, warning := range warnings {
		log.Warnf("%v", warning)
	}
	res.Total = len(files)

	log.Infof("源目录: %s", srcRoot)
	if o.DryRun {
		log.Infof("演练模式: 只检查并打印计划，不会上传、写记录或删除文件")
	}
	log.Infof("共找到 %d 个待处理归档文件", len(files))
	if len(files) == 0 {
		res.Duration = time.Since(started)
		return res, nil
	}

	backend, err := backends.Open(target, storage.Options{
		User:       o.SMB.User,
		Password:   o.SMB.Password,
		Domain:     o.SMB.Domain,
		Port:       o.SMB.Port,
		CreateRoot: !o.DryRun,
	}, nativeAccess)
	if err != nil {
		return res, err
	}
	defer backend.Close()

	log.Infof("目标目录: %s (%s)", backend.Location(""), backend.Kind())
	if o.sourcePrefix != "" {
		log.Infof("上传目录: %s（--with-source-dir：使用源目录同名文件夹）", backend.Location(o.sourcePrefix))
	}

	plan := &planState{dirs: make(map[string]bool)}
	for i, file := range files {
		if ctxErr := ctx.Err(); ctxErr != nil {
			log.Warnf("已中断，剩余 %d 个文件未处理", len(files)-i)
			res.Duration = time.Since(started)
			return res, ctxErr
		}

		log.Infof("[%d/%d] %s (%s)", i+1, len(files), file.Rel, humanize.Bytes(file.Size))
		outcome, procErr := processFile(ctx, o, log, backend, file, plan)
		if procErr != nil {
			res.Failed++
			res.Failures = append(res.Failures, file.Rel)
			log.Errorf("%s: %v", file.Rel, procErr)
			continue
		}
		switch outcome {
		case OutcomeUploaded:
			res.Uploaded++
			res.Bytes += file.Size
		case OutcomeSkipped:
			res.Skipped++
		case OutcomePlanned:
			res.Planned++
		}
	}

	res.Duration = time.Since(started)
	return res, nil
}

func processFile(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, file scan.File, plan *planState) (Outcome, error) {
	info, err := os.Stat(file.Path)
	if err != nil {
		return OutcomeFailed, fmt.Errorf("读取源文件失败: %w", err)
	}
	if info.IsDir() {
		return OutcomeFailed, fmt.Errorf("源路径是目录而不是文件")
	}
	size := info.Size()

	remoteRel := file.Rel
	if o.sourcePrefix != "" {
		remoteRel = path.Join(o.sourcePrefix, file.Rel)
	}
	relDir := relDirOf(remoteRel)
	sidecarRel := report.SidecarName(remoteRel)
	remotePath := backend.Location(remoteRel)
	sidecarPath := backend.Location(sidecarRel)
	localNote := file.Path + ".txt"

	remoteInfo, statErr := backend.Stat(remoteRel)
	if statErr != nil && !os.IsNotExist(statErr) {
		return OutcomeFailed, fmt.Errorf("检查远端目标失败: %w", statErr)
	}
	remoteExists := statErr == nil
	if remoteExists && remoteInfo.IsDir {
		return OutcomeFailed, fmt.Errorf("远端同名路径是目录: %s", remotePath)
	}

	recData, rec, recErr := readRecord(ctx, backend, sidecarRel)
	if recErr != nil && !os.IsNotExist(recErr) {
		log.Debugf("读取远端上传记录 %s 失败: %v", sidecarPath, recErr)
	}
	localRecData, localRec, localRecErr := readLocalRecord(file.Path)
	if localRecErr != nil && !os.IsNotExist(localRecErr) {
		log.Debugf("读取本地上传记录 %s 失败: %v", localNote, localRecErr)
	}

	if o.DryRun {
		if err := planFile(o, log, backend, file, size, relDir, remoteRel, remoteExists, plan); err != nil {
			return OutcomeFailed, err
		}
		return OutcomePlanned, nil
	}

	log.Debugf("计算本地 SHA256: %s", file.Path)
	hashBar := progress.New(o.ProgressOut, "计算 SHA256 "+file.Rel, size, progressEnabled(o, size >= hashBarThreshold))
	localSHA, err := hashLocalFile(ctx, file.Path, hashBar)
	if err != nil {
		hashBar.Abort()
		return OutcomeFailed, fmt.Errorf("计算本地 SHA256 失败: %w", err)
	}
	hashBar.Finish()
	log.Debugf("本地 SHA256: %s", localSHA)

	// The remote file already exists: skip when its content matches, otherwise
	// refuse to overwrite unless --force was given.
	if !o.Force && remoteExists {
		remoteMatch := recErr == nil && rec.SHA256 == localSHA && rec.Size == size
		localMatch := localRecErr == nil && localRec.SHA256 == localSHA && localRec.Size == size

		var content []byte
		remoteOK, localOK := false, false
		switch {
		case remoteMatch:
			content, remoteOK, localOK = recData, true, localMatch
		case localMatch:
			content, localOK = localRecData, true
		}

		if content == nil {
			if remoteInfo.Size != size {
				return OutcomeFailed, conflictError(remotePath, "大小与本地文件不同")
			}
			// No usable note on either side (for example --txt none): read the
			// remote file back so a repeated run can still be idempotent.
			log.Debugf("没有匹配的上传记录，读回远端文件判断是否已上传: %s", remotePath)
			checkBar := progress.New(o.ProgressOut, "校验已存在文件 "+file.Rel, size, progressEnabled(o, size >= hashBarThreshold))
			remoteSHA, hashErr := hashRemoteFile(ctx, backend, remoteRel, checkBar)
			if hashErr != nil {
				checkBar.Abort()
				return OutcomeFailed, fmt.Errorf("读回远端文件校验失败: %w", hashErr)
			}
			checkBar.Finish()
			if remoteSHA != localSHA {
				return OutcomeFailed, conflictError(remotePath, "SHA256 与本地文件不同")
			}
			uploadedAt := remoteInfo.ModTime
			if uploadedAt.IsZero() {
				uploadedAt = now(o)
			}
			content = []byte(report.Record{
				FileName:      filepath.Base(file.Path),
				TargetPath:    remotePath,
				Size:          size,
				SHA256:        localSHA,
				UploadedAt:    uploadedAt,
				SourcePath:    file.Path,
				SourceModTime: info.ModTime(),
				ToolVersion:   version(o),
				Verified:      true,
			}.Render())
		} else if o.Recheck {
			checkBar := progress.New(o.ProgressOut, "重新校验远端 "+file.Rel, size, progressEnabled(o, size >= hashBarThreshold))
			remoteSHA, hashErr := hashRemoteFile(ctx, backend, remoteRel, checkBar)
			if hashErr != nil {
				checkBar.Abort()
				return OutcomeFailed, fmt.Errorf("重新校验远端文件失败: %w", hashErr)
			}
			checkBar.Finish()
			if remoteSHA != localSHA {
				return OutcomeFailed, fmt.Errorf("远端文件与上传记录不一致（SHA256 不同），未删除本地文件: %s", remotePath)
			}
		}

		if err := ensureRecords(o, backend, sidecarRel, localNote, content, remoteOK, localOK); err != nil {
			return OutcomeFailed, fmt.Errorf("补写上传记录失败，已保留本地源文件: %w", err)
		}
		log.Successf("远端已存在且校验一致，跳过上传: %s", remotePath)
		deleteLocalFile(o, log, file.Path, size, info.ModTime())
		return OutcomeSkipped, nil
	}

	if relDir != "" {
		if err := backend.MkdirAll(relDir); err != nil {
			return OutcomeFailed, fmt.Errorf("创建远端目录失败: %w", err)
		}
	}

	log.Debugf("上传 %s -> %s", file.Path, remotePath)
	uploadBar := progress.New(o.ProgressOut, "上传 "+file.Rel, size, progressEnabled(o, true))
	uploadStarted := time.Now()
	if err := uploadFile(ctx, backend, file.Path, remoteRel, size, o.Force, uploadBar); err != nil {
		uploadBar.Abort()
		// The previous remote copy (if any) and its record are still valid: only
		// clean up temporary files here.
		cleanupTempFiles(backend, remoteRel, sidecarRel)
		return OutcomeFailed, err
	}
	uploadBar.Finish()
	uploadDuration := time.Since(uploadStarted)

	verifyBar := progress.New(o.ProgressOut, "校验远端 "+file.Rel, size, progressEnabled(o, size >= hashBarThreshold))
	remoteSHA, err := hashRemoteFile(ctx, backend, remoteRel, verifyBar)
	if err != nil {
		verifyBar.Abort()
		cleanupRemote(backend, remoteRel, sidecarRel)
		return OutcomeFailed, fmt.Errorf("读取远端文件校验失败: %w", err)
	}
	verifyBar.Finish()

	if remoteSHA != localSHA {
		cleanupRemote(backend, remoteRel, sidecarRel)
		return OutcomeFailed, fmt.Errorf("SHA256 不一致（本地 %s，远端 %s），已删除远端文件并保留本地文件",
			shortHash(localSHA), shortHash(remoteSHA))
	}
	log.Debugf("远端 SHA256: %s", remoteSHA)

	record := report.Record{
		FileName:      filepath.Base(file.Path),
		TargetPath:    remotePath,
		Size:          size,
		SHA256:        localSHA,
		UploadedAt:    now(o),
		SourcePath:    file.Path,
		SourceModTime: info.ModTime(),
		Duration:      uploadDuration,
		ToolVersion:   version(o),
		Verified:      true,
	}
	content := []byte(record.Render())
	if err := ensureRecords(o, backend, sidecarRel, localNote, content, false, false); err != nil {
		return OutcomeFailed, fmt.Errorf("远端文件已上传并校验通过，但保存上传记录失败，已保留本地源文件: %w", err)
	}

	log.Successf("上传完成并校验一致: %s", remotePath)
	deleteLocalFile(o, log, file.Path, size, info.ModTime())
	return OutcomeUploaded, nil
}

func planFile(o Options, log *xlog.Logger, backend storage.Backend, file scan.File, size int64, relDir, remoteRel string, remoteExists bool, plan *planState) error {
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
	if remoteExists {
		log.Infof("[计划] 目标已存在，将校验/覆盖: %s", backend.Location(remoteRel))
	} else {
		log.Infof("[计划] 上传: %s (%s) -> %s", file.Path, humanize.Bytes(size), backend.Location(remoteRel))
	}
	log.Infof("[计划] 上传后读回校验 SHA256")
	switch o.Txt {
	case TxtBoth:
		log.Infof("[计划] 写入远端记录: %s", backend.Location(report.SidecarName(remoteRel)))
		log.Infof("[计划] 写入本地记录: %s", file.Path+".txt")
	case TxtRemote:
		log.Infof("[计划] 写入远端记录: %s", backend.Location(report.SidecarName(remoteRel)))
	case TxtLocal:
		log.Infof("[计划] 写入本地记录: %s", file.Path+".txt")
	default:
		log.Infof("[计划] 不生成上传记录 txt")
	}
	if !o.KeepSource {
		log.Infof("[计划] 删除源文件: %s", file.Path)
	}
	return nil
}

func uploadFile(ctx context.Context, backend storage.Backend, localPath, remoteRel string, size int64, overwrite bool, bar *progress.Bar) error {
	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("打开源文件失败: %w", err)
	}
	defer src.Close()

	tmpRel := remoteRel + ".nassync.tmp"
	_ = backend.Remove(tmpRel)

	dst, err := backend.Create(tmpRel)
	if err != nil {
		return fmt.Errorf("创建远端临时文件失败: %w", err)
	}

	written, copyErr := io.CopyBuffer(dst, progress.NewReader(ctxReader{ctx: ctx, r: src}, bar), make([]byte, copyBufferSize))
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
		_ = backend.Remove(tmpRel)
		return fmt.Errorf("上传失败: %w", copyErr)
	}

	if overwrite {
		_ = backend.Remove(remoteRel)
	}
	if err := backend.Rename(tmpRel, remoteRel); err != nil {
		_ = backend.Remove(tmpRel)
		return fmt.Errorf("重命名远端临时文件失败: %w", err)
	}
	return nil
}

func writeRecord(backend storage.Backend, rel, content string) error {
	dir := relDirOf(rel)
	if dir != "" {
		if err := backend.MkdirAll(dir); err != nil {
			return err
		}
	}
	tmpRel := rel + ".tmp"
	_ = backend.Remove(tmpRel)

	dst, err := backend.Create(tmpRel)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(dst, content)
	if writeErr == nil {
		writeErr = dst.Sync()
	}
	if closeErr := dst.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = backend.Remove(tmpRel)
		return writeErr
	}

	// Renaming over an existing file is not portable, remove it first.
	_ = backend.Remove(rel)
	if err := backend.Rename(tmpRel, rel); err != nil {
		_ = backend.Remove(tmpRel)
		return err
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
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
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
func ensureRecords(o Options, backend storage.Backend, sidecarRel, localNote string, content []byte, remoteOK, localOK bool) error {
	if o.Txt.WritesRemote() && !remoteOK {
		if err := writeRecord(backend, sidecarRel, string(content)); err != nil {
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

func hashLocalFile(ctx context.Context, p string, bar *progress.Bar) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return hashReader(ctx, f, bar)
}

func hashRemoteFile(ctx context.Context, backend storage.Backend, rel string, bar *progress.Bar) (string, error) {
	f, err := backend.Open(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return hashReader(ctx, f, bar)
}

func hashReader(ctx context.Context, r io.Reader, bar *progress.Bar) (string, error) {
	h := sha256.New()
	if _, err := io.CopyBuffer(h, progress.NewReader(ctxReader{ctx: ctx, r: r}, bar), make([]byte, copyBufferSize)); err != nil {
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

func progressEnabled(o Options, enabled bool) bool {
	return enabled && !o.NoProgress
}

func now(o Options) time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func version(o Options) string {
	if o.Version == "" {
		return "dev"
	}
	return o.Version
}

func relDirOf(rel string) string {
	dir := path.Dir(rel)
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// sourceDirName derives the folder name used by --with-source-dir. Volume and
// file system roots cannot provide a meaningful name.
func sourceDirName(srcRoot string) (string, error) {
	name := filepath.Base(filepath.Clean(srcRoot))
	if name == "" || name == "." || name == string(filepath.Separator) || name == `\` || name == "/" || strings.HasSuffix(name, ":") {
		return "", fmt.Errorf("无法从源目录推导文件夹名称: %s（请换一个源目录，或去掉 --with-source-dir）", srcRoot)
	}
	return name, nil
}

func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// useNativeSMB decides whether an SMB target is accessed through the operating
// system's SMB redirector (reusing existing Windows credentials) or through the
// built-in client.
func useNativeSMB(target storage.Target, o Options) (bool, error) {
	if target.Scheme != "smb" {
		return false, nil
	}
	mode := strings.ToLower(strings.TrimSpace(o.SMBMode))
	if mode == "" {
		mode = "auto"
	}
	switch mode {
	case "auto":
		if target.Port != 0 && target.Port != 445 {
			// The system redirector cannot express a custom port.
			return false, nil
		}
		return !o.SMBAuthRequested, nil
	case "native":
		if target.Port != 0 && target.Port != 445 {
			return false, fmt.Errorf("系统级 SMB 访问不支持自定义端口 %d，请改用 --smb-mode builtin", target.Port)
		}
		return true, nil
	case "builtin":
		return false, nil
	default:
		return false, fmt.Errorf("SMB 访问方式无效: %q（可选 auto、native、builtin）", o.SMBMode)
	}
}

func isWithin(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func samePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if a == b {
		return true
	}
	return runtime.GOOS == "windows" && strings.EqualFold(a, b)
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
		return c.r.Read(p)
	}
}

// PrintResult writes the final summary to w.
func PrintResult(w io.Writer, r Result) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "──────── 处理结果 ────────")
	fmt.Fprintf(w, "扫描文件   : %d\n", r.Total)
	if r.DryRun {
		fmt.Fprintf(w, "计划处理   : %d\n", r.Planned)
	} else {
		fmt.Fprintf(w, "上传成功   : %d (%s)\n", r.Uploaded, humanize.Bytes(r.Bytes))
		fmt.Fprintf(w, "跳过       : %d\n", r.Skipped)
	}
	fmt.Fprintf(w, "失败       : %d\n", r.Failed)
	fmt.Fprintf(w, "总耗时     : %s\n", humanize.Duration(r.Duration))
	if len(r.Failures) > 0 {
		fmt.Fprintln(w, "失败文件:")
		for _, f := range r.Failures {
			fmt.Fprintf(w, "  - %s\n", f)
		}
	}
}
