package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fjf/NASArchiveSync/internal/humanize"
	"github.com/fjf/NASArchiveSync/internal/progress"
	"github.com/fjf/NASArchiveSync/internal/scan"
	"github.com/fjf/NASArchiveSync/internal/storage"
	"github.com/fjf/NASArchiveSync/internal/xlog"
)

// runVerifyOnly checks every local archive against the remote copy. It never
// uploads, writes records or deletes source files. With --concurrency > 1 it
// opens one read-only backend per worker, just like the upload path.
func runVerifyOnly(ctx context.Context, o Options, log *xlog.Logger, first storage.Backend, open func() (storage.Backend, error), files []scan.File, res *Result) error {
	workers := o.Concurrency
	if workers < 1 {
		workers = 1
	}
	if workers > len(files) {
		workers = len(files)
	}
	if workers <= 1 {
		runVerifyOnlySequential(ctx, o, log, first, files, res)
		return nil
	}

	backends := make([]storage.Backend, workers)
	backends[0] = first
	for i := 1; i < workers; i++ {
		backend, err := open()
		if err != nil {
			for _, opened := range backends[1:i] {
				_ = opened.Close()
			}
			return err
		}
		backends[i] = backend
	}
	defer func() {
		for _, backend := range backends[1:] {
			_ = backend.Close()
		}
	}()

	log.Infof("并发校验: %d 个文件同时处理", workers)
	jobs := make(chan int)
	results := make(chan fileResult, len(files))
	var wg sync.WaitGroup
	for _, backend := range backends {
		wg.Add(1)
		go func(backend storage.Backend) {
			defer wg.Done()
			discard := func(string, int64) progress.Counter { return progress.Discard }
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				file := files[index]
				log.Infof("[%d/%d] 校验 %s (%s)", index+1, len(files), file.Rel, humanize.Bytes(file.Size))
				results <- verifyFile(ctx, o, log, backend, file, discard)
			}
		}(backend)
	}

	go func() {
		defer close(jobs)
		for index := range files {
			select {
			case jobs <- index:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()
	for result := range results {
		aggregateResult(res, result, log)
	}
	return nil
}

func runVerifyOnlySequential(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, files []scan.File, res *Result) {
	counter := func(label string, total int64) progress.Counter {
		return progress.New(o.ProgressOut, label, total, progressEnabled(o, true))
	}

	for i, file := range files {
		if ctx.Err() != nil {
			return
		}
		log.Infof("[%d/%d] 校验 %s (%s)", i+1, len(files), file.Rel, humanize.Bytes(file.Size))
		result := verifyFile(ctx, o, log, backend, file, counter)
		aggregateResult(res, result, log)
	}
}

func verifyFile(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, file scan.File, newCounter func(string, int64) progress.Counter) fileResult {
	st, err := inspectFile(ctx, o, log, backend, file)
	if err != nil {
		return fileResult{outcome: OutcomeFailed, file: file.Rel, err: err}
	}
	if !st.remoteExists {
		return fileResult{outcome: OutcomeFailed, file: file.Rel, size: st.size, err: fmt.Errorf("远端文件不存在: %s", st.paths.remotePath)}
	}
	if st.remoteInfo.IsDir {
		return fileResult{outcome: OutcomeFailed, file: file.Rel, size: st.size, err: fmt.Errorf("远端同名路径是目录: %s", st.paths.remotePath)}
	}
	if st.remoteInfo.Size != st.size {
		return fileResult{
			outcome: OutcomeFailed,
			file:    file.Rel,
			size:    st.size,
			err:     fmt.Errorf("大小不一致（本地 %d，远端 %d）: %s", st.size, st.remoteInfo.Size, st.paths.remotePath),
		}
	}

	localCounter := newCounter("本地 "+file.Rel, st.size)
	hashStart := time.Now()
	localSHA, err := hashLocalFile(ctx, st.file.Path, localCounter, st.size)
	hashTime := time.Since(hashStart)
	if err != nil {
		localCounter.Abort()
		return fileResult{outcome: OutcomeFailed, file: file.Rel, size: st.size, hashTime: hashTime, err: fmt.Errorf("计算本地 SHA256 失败: %w", err)}
	}
	localCounter.Finish()

	remoteCounter := newCounter("校验 "+file.Rel, st.size)
	verifyStart := time.Now()
	remoteSHA, err := retryValue(ctx, o, log, backend, "校验远端", func() (string, error) {
		return hashRemoteFile(ctx, backend, st.paths.remoteRel, remoteCounter, st.size, o.VerifyConcurrency, o.verifyPool, o.limiter)
	})
	verifyTime := time.Since(verifyStart)
	if err != nil {
		remoteCounter.Abort()
		return fileResult{outcome: OutcomeFailed, file: file.Rel, size: st.size, hashTime: hashTime, verifyTime: verifyTime, err: fmt.Errorf("读取远端文件校验失败: %w", err)}
	}
	remoteCounter.Finish()

	if localSHA != remoteSHA {
		return fileResult{
			outcome:    OutcomeFailed,
			file:       file.Rel,
			size:       st.size,
			hashTime:   hashTime,
			verifyTime: verifyTime,
			err:        fmt.Errorf("SHA256 不一致（本地 %s，远端 %s）: %s", shortHash(localSHA), shortHash(remoteSHA), st.paths.remotePath),
		}
	}
	log.Successf("校验一致: %s", st.paths.remotePath)
	return fileResult{outcome: OutcomeVerified, file: file.Rel, size: st.size, hashTime: hashTime, verifyTime: verifyTime}
}

// cleanRemoteTemps removes stale NASArchiveSync temporary files below the
// target. It only recognises names created by this tool and never removes an
// archive or a completion note.
func cleanRemoteTemps(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend) (int, error) {
	lister, ok := backend.(storage.Lister)
	if !ok {
		return 0, fmt.Errorf("当前存储后端不支持目录列表，无法清理临时文件")
	}
	removed, err := cleanTempDir(ctx, o, log, backend, lister, "", 0)
	if err != nil {
		return removed, err
	}
	if removed == 0 {
		log.Infof("没有发现需要清理的临时文件: %s", backend.Location(""))
	} else {
		log.Successf("已清理 %d 个临时文件: %s", removed, backend.Location(""))
	}
	return removed, nil
}

func cleanTempDir(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, lister storage.Lister, rel string, depth int) (int, error) {
	if depth > 64 {
		return 0, fmt.Errorf("目录层次过深，已停止递归: %s", backend.Location(rel))
	}
	entries, err := retryValue(ctx, o, log, backend, "列出目录", func() ([]storage.FileInfo, error) {
		return lister.List(rel)
	})
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("列出目录失败 %s: %w", backend.Location(rel), err)
	}

	removed := 0
	for _, entry := range entries {
		if entry.Name == "." || entry.Name == ".." {
			continue
		}
		child := storage.JoinRel(rel, entry.Name)
		if entry.IsDir {
			count, err := cleanTempDir(ctx, o, log, backend, lister, child, depth+1)
			removed += count
			if err != nil {
				return removed, err
			}
			continue
		}
		if !isTempFileName(entry.Name) {
			continue
		}
		if err := retryDo(ctx, o, log, backend, "清理临时文件", func() error {
			return backend.Remove(child)
		}); err != nil {
			return removed, fmt.Errorf("删除临时文件失败 %s: %w", backend.Location(child), err)
		}
		removed++
		log.Infof("已清理临时文件: %s", backend.Location(child))
	}
	return removed, nil
}

// isTempFileName recognises the fixed legacy name and the current unique name.
// Backups (`...tmp.bak`) are deliberately excluded: they may hold the last
// known-good copy of a file.
func isTempFileName(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".nassync.tmp") {
		return true
	}
	return strings.Contains(lower, ".nassync.") && strings.HasSuffix(lower, ".tmp")
}
