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
	"sync"
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
	maxCopyBufferSize = 2 * 1024 * 1024
	minCopyBufferSize = 32 * 1024
	hashBarThreshold  = 16 * 1024 * 1024
	recordSizeLimit   = 1 << 20
)

// SMBOptions carries SMB credentials.
type SMBOptions struct {
	User     string
	Password string
	Domain   string
	Port     int
}

// SFTPOptions carries SSH/SFTP credentials and tuning.
type SFTPOptions struct {
	User           string
	Password       string
	KeyPath        string
	KeyPassphrase  string
	Port           int
	Cipher         string
	KnownHostsPath string
	Insecure       bool
}

// S3Options carries S3 credentials and endpoint settings.
type S3Options struct {
	Endpoint     string
	Region       string
	AccessKey    string
	SecretKey    string
	SessionToken string
	Style        string
	Insecure     bool
	PartSize     int64
	Concurrency  int
}

// WebDAVOptions carries WebDAV credentials. The server address and path are
// part of --target.
type WebDAVOptions struct {
	User     string
	Password string
	Insecure bool
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
	// List only prints the matched archive files. No target is required and no
	// storage backend is opened or modified.
	List       bool
	Include    []string
	Exclude    []string
	Extensions []string
	DryRun     bool
	KeepSource bool
	Force      bool
	Recheck    bool
	// Pipeline overlaps the upload of the next file with the verification of
	// the previous one. The CLI enables it by default.
	Pipeline bool
	// NoVerify disables SHA256 entirely: the archive is uploaded without
	// hashing and is not read back. Completion notes are marked as unverified.
	NoVerify bool
	// VerifyConcurrency is the number of parallel range reads used when a
	// backend supports them. Values below 2 read the remote file sequentially.
	VerifyConcurrency int
	CaseSensitive     bool
	SMB               SMBOptions
	SFTP              SFTPOptions
	S3                S3Options
	WebDAV            WebDAVOptions
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
	ListOut          io.Writer
	Now              func() time.Time

	// sourcePrefix is the source directory name used when WithSourceDir is set.
	// It is derived in Run and prefixes every remote relative path.
	sourcePrefix string
	// verifyPool reuses range-read buffers across files in one run.
	verifyPool *chunkPool
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
	// HashTime, UploadTime and VerifyTime accumulate per-file phase times.
	// With --pipeline, upload and verify overlap, so their sum can exceed
	// Duration.
	HashTime   time.Duration
	UploadTime time.Duration
	VerifyTime time.Duration
	NoVerify   bool
	DryRun     bool
	Failures   []string
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

	res := Result{DryRun: o.DryRun, NoVerify: o.NoVerify}

	if o.NoVerify && o.Recheck {
		return res, fmt.Errorf("--recheck 需要读取远端并计算 SHA256，不能与 --no-verify 同时使用")
	}

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

	if o.WithSourceDir && !o.List {
		name, nameErr := sourceDirName(srcRoot)
		if nameErr != nil {
			return res, nameErr
		}
		o.sourcePrefix = name
	}

	scanOpts := scan.Options{
		Root:          srcRoot,
		MaxDepth:      o.Depth,
		Include:       o.Include,
		Exclude:       o.Exclude,
		Extensions:    o.Extensions,
		CaseSensitive: o.CaseSensitive,
	}

	var target storage.Target
	nativeAccess := false
	if !o.List {
		target, err = storage.ParseTarget(o.Target)
		if err != nil {
			return res, err
		}

		// A user name in an SMB URL means the built-in client should be used,
		// just like an explicit --smb-user.
		if target.Scheme == "smb" && target.User != "" {
			o.SMBAuthRequested = true
		}
		nativeAccess, err = useNativeSMB(target, o)
		if err != nil {
			return res, err
		}
		if nativeAccess && o.SMBAuthRequested {
			log.Warnf("系统级 SMB 访问会复用 Windows 已登录凭据，--smb-user/--smb-pass 等参数将被忽略")
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
	}

	files, warnings, err := scan.Scan(scanOpts)
	if err != nil {
		return res, err
	}
	for _, warning := range warnings {
		log.Warnf("%v", warning)
	}
	res.Total = len(files)

	if o.List {
		out := o.ListOut
		if out == nil {
			out = os.Stdout
		}
		for _, file := range files {
			fmt.Fprintln(out, file.Rel)
		}
		res.Duration = time.Since(started)
		return res, nil
	}

	log.Infof("源目录: %s", srcRoot)
	if o.DryRun {
		log.Infof("演练模式: 只检查并打印计划，不会上传、写记录或删除文件")
	}
	log.Infof("共找到 %d 个待处理归档文件", len(files))
	if len(files) == 0 {
		res.Duration = time.Since(started)
		return res, nil
	}

	// Only reuse verification buffers when more than one file will be read
	// back: for a single file the pool would just keep memory alive longer.
	if len(files) > 1 && o.VerifyConcurrency > 1 {
		workers := o.VerifyConcurrency
		if workers > maxVerifyConcurrency {
			workers = maxVerifyConcurrency
		}
		o.verifyPool = newChunkPool(parallelHashChunkSize, workers)
	}

	backendOpts := storage.Options{
		User:       o.SMB.User,
		Password:   o.SMB.Password,
		Domain:     o.SMB.Domain,
		Port:       o.SMB.Port,
		CreateRoot: !o.DryRun,
		// Several files will reuse the same buffer pool; a single file is
		// faster to release when the writer is done.
		ReuseBuffers: len(files) > 1,
	}
	switch target.Scheme {
	case "smb":
		// A user name in the target URL is a convenient fallback for the
		// built-in client; explicit --smb-user still wins.
		if backendOpts.User == "" {
			backendOpts.User = target.User
		}
		if backendOpts.Password == "" {
			backendOpts.Password = target.Password
		}
	case "sftp":
		backendOpts.User = o.SFTP.User
		backendOpts.Password = o.SFTP.Password
		backendOpts.Port = o.SFTP.Port
		backendOpts.KeyPath = o.SFTP.KeyPath
		backendOpts.KeyPassphrase = o.SFTP.KeyPassphrase
		backendOpts.Cipher = o.SFTP.Cipher
		backendOpts.KnownHostsPath = o.SFTP.KnownHostsPath
		backendOpts.InsecureHostKey = o.SFTP.Insecure
		if o.SFTP.Insecure {
			log.Warnf("已禁用 SFTP 主机密钥校验（--sftp-insecure），连接可能被中间人攻击")
		}
	case "s3":
		backendOpts.S3Endpoint = o.S3.Endpoint
		backendOpts.S3Region = o.S3.Region
		backendOpts.S3AccessKey = o.S3.AccessKey
		backendOpts.S3SecretKey = o.S3.SecretKey
		backendOpts.S3SessionToken = o.S3.SessionToken
		backendOpts.S3Style = o.S3.Style
		backendOpts.S3Insecure = o.S3.Insecure
		backendOpts.S3PartSize = o.S3.PartSize
		backendOpts.S3Concurrency = o.S3.Concurrency
		if o.S3.Insecure {
			log.Warnf("已禁用 S3 HTTPS 证书校验（--s3-insecure），连接可能被中间人攻击")
		}
	case "webdav":
		backendOpts.User = o.WebDAV.User
		backendOpts.Password = o.WebDAV.Password
		backendOpts.WebDAVInsecure = o.WebDAV.Insecure
		if o.WebDAV.Insecure {
			log.Warnf("已禁用 WebDAV HTTPS 证书校验（--webdav-insecure），连接可能被中间人攻击")
		}
	}
	uploadBackend, err := backends.Open(target, backendOpts, nativeAccess)
	if err != nil {
		return res, err
	}
	defer uploadBackend.Close()

	log.Infof("目标目录: %s (%s)", uploadBackend.Location(""), uploadBackend.Kind())
	if o.sourcePrefix != "" {
		log.Infof("上传目录: %s（--with-source-dir：使用源目录同名文件夹）", uploadBackend.Location(o.sourcePrefix))
	}
	if o.NoVerify {
		log.Warnf("已关闭校验: 不计算 SHA256，上传后不读回远端文件，上传记录将标记为未校验")
	}

	if !o.DryRun && o.Pipeline && !o.NoVerify {
		verifyBackend, openErr := backends.Open(target, backendOpts, nativeAccess)
		if openErr != nil {
			return res, openErr
		}
		defer verifyBackend.Close()
		runPipeline(ctx, o, log, uploadBackend, verifyBackend, files, &res)
	} else {
		runSequential(ctx, o, log, uploadBackend, files, &res)
	}

	res.Duration = time.Since(started)
	if ctxErr := ctx.Err(); ctxErr != nil {
		log.Warnf("已中断")
		return res, ctxErr
	}
	return res, nil
}

func runSequential(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, files []scan.File, res *Result) {
	plan := &planState{dirs: make(map[string]bool)}
	newHash := func(label string, total int64) progress.Counter {
		return progress.New(o.ProgressOut, label, total, progressEnabled(o, total >= hashBarThreshold))
	}
	newUpload := func(label string, total int64) progress.Counter {
		return progress.New(o.ProgressOut, label, total, progressEnabled(o, true))
	}

	for i, file := range files {
		if ctx.Err() != nil {
			return
		}
		log.Infof("[%d/%d] %s (%s)", i+1, len(files), file.Rel, humanize.Bytes(file.Size))
		fr := processFile(ctx, o, log, backend, file, fileContext{index: i, total: len(files)}, plan, newHash, newUpload, false)
		aggregateResult(res, fr, log)
	}
}

func runPipeline(ctx context.Context, o Options, log *xlog.Logger, uploadBackend, verifyBackend storage.Backend, files []scan.File, res *Result) {
	multi := progress.NewMulti(o.ProgressOut,
		[]string{"上传 等待开始", "校验 等待上传"},
		[]int64{0, 0},
		!o.NoProgress,
	)
	quiet := multi.Enabled()
	runLog := log
	if quiet {
		runLog = log.WithOutput(progress.NewPauseWriter(log.Output(), multi))
	}
	log.Infof("流水线模式: 上传下一个文件时并行校验上一个文件")

	hashFactory := func(label string, total int64) progress.Counter {
		if !quiet {
			return progress.Discard
		}
		line := multi.Line(0)
		line.Reset(label, total)
		return line
	}
	uploadFactory := hashFactory
	verifyFactory := func(label string, total int64) progress.Counter {
		if !quiet {
			return progress.Discard
		}
		line := multi.Line(1)
		line.Reset(label, total)
		return line
	}

	verifyCh := make(chan *uploadedTask, 1)
	results := make(chan fileResult, len(files))
	var producerWG, verifierWG sync.WaitGroup

	producerWG.Add(1)
	go func() {
		defer producerWG.Done()
		defer close(verifyCh)
		for i, file := range files {
			if ctx.Err() != nil {
				return
			}
			fc := fileContext{index: i, total: len(files), pipeline: true}
			if !quiet {
				log.Infof("[%d/%d] %s (%s)", i+1, len(files), file.Rel, humanize.Bytes(file.Size))
			}

			st, err := inspectFile(ctx, o, runLog, uploadBackend, file)
			if err != nil {
				results <- fileResult{outcome: OutcomeFailed, file: file.Rel, err: err}
				continue
			}
			if !o.Force && st.remoteExists {
				results <- handleSkip(ctx, o, runLog, uploadBackend, st, fc, hashFactory, quiet)
				continue
			}

			sha, uploadDur, err := uploadOnly(ctx, o, runLog, uploadBackend, st, fc, uploadFactory)
			if err != nil {
				results <- fileResult{outcome: OutcomeFailed, file: file.Rel, size: st.size, err: err, uploadTime: uploadDur}
				continue
			}

			task := &uploadedTask{state: st, localSHA: sha, uploadDuration: uploadDur, index: i}
			select {
			case verifyCh <- task:
			case <-ctx.Done():
				cleanupRemote(uploadBackend, st.paths.remoteRel, st.paths.sidecarRel)
				return
			}
		}
	}()

	verifierWG.Add(1)
	go func() {
		defer verifierWG.Done()
		for task := range verifyCh {
			fc := fileContext{index: task.index, total: len(files), pipeline: true}
			results <- verifyUploaded(ctx, o, runLog, verifyBackend, task.state, task.localSHA, task.uploadDuration, fc, verifyFactory, quiet)
		}
	}()

	go func() {
		producerWG.Wait()
		verifierWG.Wait()
		close(results)
	}()

	// Aggregate as results arrive so failures are reported immediately instead
	// of being held back until every file has been processed.
	for fr := range results {
		aggregateResult(res, fr, runLog)
	}
	multi.Finish()
}

func aggregateResult(res *Result, fr fileResult, log *xlog.Logger) {
	res.HashTime += fr.hashTime
	res.UploadTime += fr.uploadTime
	res.VerifyTime += fr.verifyTime
	switch fr.outcome {
	case OutcomeUploaded:
		res.Uploaded++
		res.Bytes += fr.size
	case OutcomeSkipped:
		res.Skipped++
	case OutcomePlanned:
		res.Planned++
	case OutcomeFailed:
		res.Failed++
		res.Failures = append(res.Failures, fr.file)
		if fr.err != nil {
			log.Errorf("%s: %v", fr.file, fr.err)
		} else {
			log.Errorf("%s: 未知错误", fr.file)
		}
	}
}

type filePaths struct {
	remoteRel   string
	relDir      string
	sidecarRel  string
	remotePath  string
	sidecarPath string
	localNote   string
}

type fileState struct {
	file         scan.File
	info         os.FileInfo
	size         int64
	paths        filePaths
	remoteExists bool
	remoteInfo   storage.FileInfo
	recData      []byte
	rec          *report.Record
	recErr       error
	localRecData []byte
	localRec     *report.Record
	localRecErr  error
}

type fileResult struct {
	outcome    Outcome
	file       string
	size       int64
	err        error
	hashTime   time.Duration
	uploadTime time.Duration
	verifyTime time.Duration
}

type uploadedTask struct {
	state          *fileState
	localSHA       string
	uploadDuration time.Duration
	index          int
}

type fileContext struct {
	index    int
	total    int
	pipeline bool
}

func (fc fileContext) label(phase, rel string) string {
	if fc.pipeline {
		return fmt.Sprintf("%s [%d/%d] %s", phase, fc.index+1, fc.total, rel)
	}
	return phase + " " + rel
}

type counterFactory func(label string, total int64) progress.Counter

func pathsFor(o Options, backend storage.Backend, file scan.File) filePaths {
	remoteRel := file.Rel
	if o.sourcePrefix != "" {
		remoteRel = path.Join(o.sourcePrefix, file.Rel)
	}
	sidecarRel := report.SidecarName(remoteRel)
	return filePaths{
		remoteRel:   remoteRel,
		relDir:      relDirOf(remoteRel),
		sidecarRel:  sidecarRel,
		remotePath:  backend.Location(remoteRel),
		sidecarPath: backend.Location(sidecarRel),
		localNote:   file.Path + ".txt",
	}
}

func inspectFile(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, file scan.File) (*fileState, error) {
	info, err := os.Stat(file.Path)
	if err != nil {
		return nil, fmt.Errorf("读取源文件失败: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("源路径是目录而不是文件")
	}

	st := &fileState{
		file:  file,
		info:  info,
		size:  info.Size(),
		paths: pathsFor(o, backend, file),
	}

	remoteInfo, statErr := backend.Stat(st.paths.remoteRel)
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("检查远端目标失败: %w", statErr)
	}
	st.remoteExists = statErr == nil
	st.remoteInfo = remoteInfo
	if st.remoteExists && remoteInfo.IsDir {
		return nil, fmt.Errorf("远端同名路径是目录: %s", st.paths.remotePath)
	}

	st.recData, st.rec, st.recErr = readRecord(ctx, backend, st.paths.sidecarRel)
	if st.recErr != nil && !os.IsNotExist(st.recErr) {
		log.Debugf("读取远端上传记录 %s 失败: %v", st.paths.sidecarPath, st.recErr)
	}
	st.localRecData, st.localRec, st.localRecErr = readLocalRecord(file.Path)
	if st.localRecErr != nil && !os.IsNotExist(st.localRecErr) {
		log.Debugf("读取本地上传记录 %s 失败: %v", st.paths.localNote, st.localRecErr)
	}
	return st, nil
}

func processFile(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, file scan.File, fc fileContext, plan *planState, newHash, newUpload counterFactory, quiet bool) fileResult {
	st, err := inspectFile(ctx, o, log, backend, file)
	if err != nil {
		return fileResult{outcome: OutcomeFailed, file: file.Rel, err: err}
	}

	if o.DryRun {
		if err := planFile(o, log, backend, file, st.size, st.paths.relDir, st.paths.remoteRel, st.remoteExists, plan); err != nil {
			return fileResult{outcome: OutcomeFailed, file: file.Rel, size: st.size, err: err}
		}
		return fileResult{outcome: OutcomePlanned, file: file.Rel, size: st.size}
	}

	if o.NoVerify {
		if !o.Force && st.remoteExists {
			return handleSkipNoVerify(o, log, backend, st)
		}
		return uploadAndRecordNoVerify(ctx, o, log, backend, st, fc, newUpload)
	}

	if !o.Force && st.remoteExists {
		return handleSkip(ctx, o, log, backend, st, fc, newHash, quiet)
	}
	return uploadAndVerify(ctx, o, log, backend, st, fc, newHash, newUpload, quiet)
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

func handleSkipNoVerify(o Options, log *xlog.Logger, backend storage.Backend, st *fileState) fileResult {
	res := fileResult{file: st.file.Rel, size: st.size}

	remoteMatch := st.recErr == nil && recordMatchesFile(st.rec, st.size, st.info.ModTime())
	localMatch := st.localRecErr == nil && recordMatchesFile(st.localRec, st.size, st.info.ModTime())

	var content []byte
	remoteOK, localOK := false, false
	switch {
	case remoteMatch:
		content, remoteOK, localOK = st.recData, true, localMatch
	case localMatch:
		content, localOK = st.localRecData, true
	}
	if content == nil {
		res.outcome = OutcomeFailed
		res.err = conflictError(st.paths.remotePath, "已关闭校验且没有可用的上传记录，无法确认内容是否一致")
		return res
	}

	if err := ensureRecords(o, backend, st.paths.sidecarRel, st.paths.localNote, content, remoteOK, localOK); err != nil {
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
		if err := backend.MkdirAll(st.paths.relDir); err != nil {
			res.outcome = OutcomeFailed
			res.err = fmt.Errorf("创建远端目录失败: %w", err)
			return res
		}
	}

	log.Debugf("上传 %s -> %s（已关闭校验）", st.file.Path, st.paths.remotePath)
	counter := newUpload(fc.label("上传", st.file.Rel), st.size)
	uploadStarted := time.Now()
	_, err := uploadFile(ctx, backend, st.file.Path, st.paths.remoteRel, st.size, o.Force, counter, false)
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
		cleanupRemote(backend, st.paths.remoteRel, st.paths.sidecarRel)
		res.outcome = OutcomeFailed
		res.err = fmt.Errorf("上传后检查源文件失败: %w", statErr)
		return res
	}
	if after.Size() != st.size || !after.ModTime().Equal(st.info.ModTime()) {
		cleanupRemote(backend, st.paths.remoteRel, st.paths.sidecarRel)
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
	if err := ensureRecords(o, backend, st.paths.sidecarRel, st.paths.localNote, content, false, false); err != nil {
		res.outcome = OutcomeFailed
		res.err = fmt.Errorf("远端文件已上传，但保存上传记录失败，已保留本地源文件: %w", err)
		return res
	}

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

	remoteMatch := st.recErr == nil && st.rec.SHA256 == localSHA && st.rec.Size == st.size
	localMatch := st.localRecErr == nil && st.localRec.SHA256 == localSHA && st.localRec.Size == st.size

	var content []byte
	remoteOK, localOK := false, false
	switch {
	case remoteMatch:
		content, remoteOK, localOK = st.recData, true, localMatch
	case localMatch:
		content, localOK = st.localRecData, true
	}

	if content == nil {
		if st.remoteInfo.Size != st.size {
			res.outcome = OutcomeFailed
			res.err = conflictError(st.paths.remotePath, "大小与本地文件不同")
			return res
		}
		// No usable note on either side (for example --txt none): read the
		// remote file back so a repeated run can still be idempotent.
		log.Debugf("没有匹配的上传记录，读回远端文件判断是否已上传: %s", st.paths.remotePath)
		checkCounter := newHash(fc.label("校验已存在", st.file.Rel), st.size)
		verifyStart := time.Now()
		remoteSHA, hashErr := hashRemoteFile(ctx, backend, st.paths.remoteRel, checkCounter, st.size, o.VerifyConcurrency, o.verifyPool)
		res.verifyTime += time.Since(verifyStart)
		if hashErr != nil {
			checkCounter.Abort()
			res.outcome = OutcomeFailed
			res.err = fmt.Errorf("读回远端文件校验失败: %w", hashErr)
			return res
		}
		checkCounter.Finish()
		if remoteSHA != localSHA {
			res.outcome = OutcomeFailed
			res.err = conflictError(st.paths.remotePath, "SHA256 与本地文件不同")
			return res
		}
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
	} else if o.Recheck {
		checkCounter := newHash(fc.label("重新校验", st.file.Rel), st.size)
		verifyStart := time.Now()
		remoteSHA, hashErr := hashRemoteFile(ctx, backend, st.paths.remoteRel, checkCounter, st.size, o.VerifyConcurrency, o.verifyPool)
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

	if err := ensureRecords(o, backend, st.paths.sidecarRel, st.paths.localNote, content, remoteOK, localOK); err != nil {
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

func uploadAndVerify(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, st *fileState, fc fileContext, newHash, newUpload counterFactory, quiet bool) fileResult {
	localSHA, uploadDur, err := uploadOnly(ctx, o, log, backend, st, fc, newUpload)
	if err != nil {
		return fileResult{outcome: OutcomeFailed, file: st.file.Rel, size: st.size, err: err, uploadTime: uploadDur}
	}
	return verifyUploaded(ctx, o, log, backend, st, localSHA, uploadDur, fc, newHash, quiet)
}

func uploadOnly(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, st *fileState, fc fileContext, newUpload counterFactory) (string, time.Duration, error) {
	if st.paths.relDir != "" {
		if err := backend.MkdirAll(st.paths.relDir); err != nil {
			return "", 0, fmt.Errorf("创建远端目录失败: %w", err)
		}
	}

	log.Debugf("上传 %s -> %s", st.file.Path, st.paths.remotePath)
	counter := newUpload(fc.label("上传", st.file.Rel), st.size)
	uploadStarted := time.Now()
	localSHA, err := uploadFile(ctx, backend, st.file.Path, st.paths.remoteRel, st.size, o.Force, counter, true)
	uploadDuration := time.Since(uploadStarted)
	if err != nil {
		counter.Abort()
		// The previous remote copy (if any) and its record are still valid: only
		// clean up temporary files here.
		cleanupTempFiles(backend, st.paths.remoteRel, st.paths.sidecarRel)
		return "", uploadDuration, err
	}
	counter.Finish()

	// The hash was computed while uploading. Make sure the source did not
	// change underneath us, otherwise the uploaded bytes may be a mix.
	after, statErr := os.Stat(st.file.Path)
	if statErr != nil {
		cleanupRemote(backend, st.paths.remoteRel, st.paths.sidecarRel)
		return "", uploadDuration, fmt.Errorf("上传后检查源文件失败: %w", statErr)
	}
	if after.Size() != st.size || !after.ModTime().Equal(st.info.ModTime()) {
		cleanupRemote(backend, st.paths.remoteRel, st.paths.sidecarRel)
		return "", uploadDuration, fmt.Errorf("源文件在上传过程中发生变化，已保留本地文件: %s", st.file.Path)
	}
	log.Debugf("上传耗时: %s", humanize.Duration(uploadDuration))
	return localSHA, uploadDuration, nil
}

func verifyUploaded(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, st *fileState, localSHA string, uploadDur time.Duration, fc fileContext, newHash counterFactory, quiet bool) fileResult {
	res := fileResult{file: st.file.Rel, size: st.size, uploadTime: uploadDur}

	counter := newHash(fc.label("校验远端", st.file.Rel), st.size)
	verifyStart := time.Now()
	remoteSHA, err := hashRemoteFile(ctx, backend, st.paths.remoteRel, counter, st.size, o.VerifyConcurrency, o.verifyPool)
	res.verifyTime = time.Since(verifyStart)
	if err != nil {
		counter.Abort()
		cleanupRemote(backend, st.paths.remoteRel, st.paths.sidecarRel)
		res.outcome = OutcomeFailed
		res.err = fmt.Errorf("读取远端文件校验失败: %w", err)
		return res
	}
	counter.Finish()

	if remoteSHA != localSHA {
		cleanupRemote(backend, st.paths.remoteRel, st.paths.sidecarRel)
		res.outcome = OutcomeFailed
		res.err = fmt.Errorf("SHA256 不一致（本地 %s，远端 %s），已删除远端文件并保留本地文件",
			shortHash(localSHA), shortHash(remoteSHA))
		return res
	}
	log.Debugf("远端 SHA256: %s", remoteSHA)

	record := report.Record{
		FileName:      filepath.Base(st.file.Path),
		TargetPath:    st.paths.remotePath,
		Size:          st.size,
		SHA256:        localSHA,
		UploadedAt:    now(o),
		SourcePath:    st.file.Path,
		SourceModTime: st.info.ModTime(),
		Duration:      uploadDur,
		ToolVersion:   version(o),
		Verified:      true,
	}
	content := []byte(record.Render())
	if err := ensureRecords(o, backend, st.paths.sidecarRel, st.paths.localNote, content, false, false); err != nil {
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

func uploadFile(ctx context.Context, backend storage.Backend, localPath, remoteRel string, size int64, overwrite bool, sink progress.Sink, computeHash bool) (string, error) {
	src, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("打开源文件失败: %w", err)
	}
	defer src.Close()

	tmpRel := remoteRel + ".nassync.tmp"
	_ = backend.Remove(tmpRel)

	dst, err := backend.Create(tmpRel)
	if err != nil {
		return "", fmt.Errorf("创建远端临时文件失败: %w", err)
	}
	if sized, ok := dst.(storage.SizedWriter); ok {
		sized.SetSize(size)
	}

	// Hash the bytes while they are copied, so the local file only has to be
	// read once for a new upload. --no-verify skips hashing entirely.
	h := sha256.New()
	var reader io.Reader = progress.NewReader(ctxReader{ctx: ctx, r: src}, sink)
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
		_ = backend.Remove(tmpRel)
		return "", fmt.Errorf("上传失败: %w", copyErr)
	}

	if overwrite {
		_ = backend.Remove(remoteRel)
	}
	if err := backend.Rename(tmpRel, remoteRel); err != nil {
		_ = backend.Remove(tmpRel)
		return "", fmt.Errorf("重命名远端临时文件失败: %w", err)
	}
	if !computeHash {
		return "", nil
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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

func hashLocalFile(ctx context.Context, p string, sink progress.Sink, size int64) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return hashReader(ctx, f, sink, size)
}

func hashRemoteFile(ctx context.Context, backend storage.Backend, rel string, sink progress.Sink, size int64, concurrency int, pool *chunkPool) (string, error) {
	if rr, ok := backend.(storage.RangeReader); ok && concurrency > 1 && size >= parallelHashMinSize {
		return hashRemoteRanges(ctx, rr, rel, sink, size, concurrency, pool)
	}
	f, err := backend.Open(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return hashReader(ctx, f, sink, size)
}

func hashReader(ctx context.Context, r io.Reader, sink progress.Sink, size int64) (string, error) {
	h := sha256.New()
	if _, err := io.CopyBuffer(h, progress.NewReader(ctxReader{ctx: ctx, r: r}, sink), make([]byte, bufferSizeFor(size))); err != nil {
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
	if r.NoVerify {
		fmt.Fprintln(w, "校验       : 已关闭（不计算 SHA256，不读回远端）")
	}
	if !r.DryRun {
		if r.HashTime > 0 {
			fmt.Fprintf(w, "本地哈希   : %s\n", humanize.Duration(r.HashTime))
		}
		if r.UploadTime > 0 {
			fmt.Fprintf(w, "上传累计   : %s\n", humanize.Duration(r.UploadTime))
		}
		if r.VerifyTime > 0 {
			fmt.Fprintf(w, "校验累计   : %s\n", humanize.Duration(r.VerifyTime))
		}
	}
	fmt.Fprintf(w, "总耗时     : %s\n", humanize.Duration(r.Duration))
	if len(r.Failures) > 0 {
		fmt.Fprintln(w, "失败文件:")
		for _, f := range r.Failures {
			fmt.Fprintf(w, "  - %s\n", f)
		}
	}
}
