// Package app implements the upload pipeline: scan, hash, upload, verify,
// write a completion note and finally delete the local archive.
package app

import (
	"context"
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
	maxCopyBufferSize = 2 * 1024 * 1024
	minCopyBufferSize = 32 * 1024
	hashBarThreshold  = 16 * 1024 * 1024
	recordSizeLimit   = 1 << 20
)

// Outcome describes what happened to a single archive.
type Outcome int

const (
	OutcomeFailed Outcome = iota
	OutcomeUploaded
	OutcomeSkipped
	OutcomeVerified
	OutcomePlanned
)

// Result is the summary of a Run.
type Result struct {
	Total      int
	Uploaded   int
	Skipped    int
	Verified   int
	Planned    int
	Failed     int
	RemovedTmp int
	Bytes      int64
	Duration   time.Duration
	// HashTime, UploadTime and VerifyTime accumulate per-file phase times.
	// With --pipeline, upload and verify overlap, so their sum can exceed
	// Duration.
	HashTime   time.Duration
	UploadTime time.Duration
	VerifyTime time.Duration
	NoVerify   bool
	DryRun     bool
	VerifyOnly bool
	CleanTemp  bool
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

	res := Result{DryRun: o.DryRun, NoVerify: o.NoVerify, VerifyOnly: o.VerifyOnly, CleanTemp: o.CleanTemp}
	o.limiter = newBandwidthLimiter(o.LimitBytesPerSec)

	if err := o.Validate(); err != nil {
		return res, err
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
	if len(files) == 0 && !o.CleanTemp {
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
		Timeout:    o.Timeout,
		IOTimeout:  o.IOTimeout,
		Context:    ctx,
		CreateRoot: !o.DryRun && !o.VerifyOnly && !o.CleanTemp,
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
	openBackend := func() (storage.Backend, error) {
		return backends.Open(target, backendOpts, nativeAccess)
	}
	uploadBackend, err := openBackend()
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

	// A case-insensitive target cannot hold both a.7z and A.7z. Detect this
	// before uploading so --force cannot silently replace one with the other.
	if !o.CleanTemp && !o.VerifyOnly && caseInsensitiveTarget(uploadBackend) {
		if groups := pathCollisionGroups(files, o.sourcePrefix); len(groups) > 0 {
			return res, pathCollisionError(groups)
		}
	}

	if o.CleanTemp {
		removed, cleanupErr := cleanRemoteTemps(ctx, o, log, uploadBackend)
		res.RemovedTmp = removed
		if cleanupErr != nil {
			return res, cleanupErr
		}
		if !o.VerifyOnly {
			res.Duration = time.Since(started)
			return res, nil
		}
	}
	if o.VerifyOnly {
		if err := runVerifyOnly(ctx, o, log, uploadBackend, openBackend, files, &res); err != nil {
			return res, err
		}
		res.Duration = time.Since(started)
		if ctxErr := ctx.Err(); ctxErr != nil {
			log.Warnf("已中断")
			return res, ctxErr
		}
		return res, nil
	}

	workers := o.Concurrency
	if workers < 1 {
		workers = 1
	}
	switch {
	case workers > 1 && !o.DryRun:
		if openErr := runConcurrent(ctx, o, log, uploadBackend, openBackend, files, &res); openErr != nil {
			return res, openErr
		}
	case !o.DryRun && o.Pipeline && !o.NoVerify:
		verifyBackend, openErr := openBackend()
		if openErr != nil {
			return res, openErr
		}
		defer verifyBackend.Close()
		runPipeline(ctx, o, log, uploadBackend, verifyBackend, files, &res)
	default:
		runSequential(ctx, o, log, uploadBackend, files, &res)
	}

	res.Duration = time.Since(started)
	if ctxErr := ctx.Err(); ctxErr != nil {
		log.Warnf("已中断")
		return res, ctxErr
	}
	return res, nil
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

type uploadResult struct {
	sha      string
	outcome  replaceOutcome
	duration time.Duration
}

type uploadedTask struct {
	state  *fileState
	upload uploadResult
	index  int
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
	return retryValue(ctx, o, log, backend, "检查远端", func() (*fileState, error) {
		return inspectFileOnce(ctx, o, log, backend, file)
	})
}

func inspectFileOnce(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, file scan.File) (*fileState, error) {
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
	rel, err := filepath.Rel(resolvedPath(parent), resolvedPath(child))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func samePath(a, b string) bool {
	a = resolvedPath(a)
	b = resolvedPath(b)
	if a == b {
		return true
	}
	return runtime.GOOS == "windows" && strings.EqualFold(a, b)
}

// resolvedPath follows symbolic links and junctions component by component.
// Unlike filepath.EvalSymlinks it also resolves paths that do not exist yet
// (the missing components are simply appended) and it understands Windows
// junctions, which EvalSymlinks leaves untouched.
func resolvedPath(p string) string {
	p = filepath.Clean(p)
	vol := filepath.VolumeName(p)
	rest := strings.TrimPrefix(p, vol)
	rest = strings.TrimLeft(rest, `/\`)
	if rest == "" {
		if vol != "" {
			return filepath.Clean(vol + string(filepath.Separator))
		}
		return p
	}

	current := vol
	if current == "" {
		if filepath.IsAbs(p) {
			current = string(filepath.Separator)
		}
	} else if !strings.HasSuffix(current, string(filepath.Separator)) {
		current += string(filepath.Separator)
	}

	links := 0
	for _, segment := range strings.Split(rest, string(filepath.Separator)) {
		next := filepath.Join(current, segment)
		target, err := os.Readlink(next)
		if err != nil {
			current = next
			continue
		}
		links++
		if links > 64 {
			return p
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(next), target)
		}
		current = target
	}
	return filepath.Clean(current)
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
	switch {
	case r.VerifyOnly:
		fmt.Fprintf(w, "校验通过   : %d (%s)\n", r.Verified, humanize.Bytes(r.Bytes))
	case r.DryRun:
		fmt.Fprintf(w, "计划处理   : %d\n", r.Planned)
	case !r.CleanTemp || r.Uploaded > 0 || r.Skipped > 0 || r.Planned > 0:
		fmt.Fprintf(w, "上传成功   : %d (%s)\n", r.Uploaded, humanize.Bytes(r.Bytes))
		fmt.Fprintf(w, "跳过       : %d\n", r.Skipped)
	}
	if r.CleanTemp {
		fmt.Fprintf(w, "清理临时   : %d\n", r.RemovedTmp)
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
