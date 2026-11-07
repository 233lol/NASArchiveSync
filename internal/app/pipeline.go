package app

import (
	"context"
	"sync"

	"github.com/fjf/NASArchiveSync/internal/humanize"
	"github.com/fjf/NASArchiveSync/internal/progress"
	"github.com/fjf/NASArchiveSync/internal/scan"
	"github.com/fjf/NASArchiveSync/internal/storage"
	"github.com/fjf/NASArchiveSync/internal/xlog"
)

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

type concurrentJob struct {
	index int
	file  scan.File
}

func runConcurrent(ctx context.Context, o Options, log *xlog.Logger, first storage.Backend, open func() (storage.Backend, error), files []scan.File, res *Result) error {
	workers := o.Concurrency
	if workers < 1 {
		workers = 1
	}
	if workers > 16 {
		workers = 16
	}
	if workers > len(files) {
		workers = len(files)
	}
	if workers < 1 {
		workers = 1
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

	mode := "每个文件上传后在同一会话校验"
	if o.NoVerify {
		mode = "每个文件只上传，不校验"
	}
	log.Infof("并发模式: %d 个文件同时处理（%s）", workers, mode)
	jobs := make(chan concurrentJob)
	results := make(chan fileResult, len(files))
	var wg sync.WaitGroup
	for i, backend := range backends {
		wg.Add(1)
		go func(index int, backend storage.Backend) {
			defer wg.Done()
			workerOpts := o
			workerOpts.ProgressOut = nil
			if o.VerifyConcurrency > 1 && len(files) > 1 {
				verifyWorkers := o.VerifyConcurrency
				if verifyWorkers > maxVerifyConcurrency {
					verifyWorkers = maxVerifyConcurrency
				}
				workerOpts.verifyPool = newChunkPool(parallelHashChunkSize, verifyWorkers)
			}
			plan := &planState{dirs: make(map[string]bool)}
			discardCounter := func(string, int64) progress.Counter { return progress.Discard }
			for job := range jobs {
				if ctx.Err() != nil {
					return
				}
				log.Infof("[%d/%d] %s (%s)", job.index+1, len(files), job.file.Rel, humanize.Bytes(job.file.Size))
				result := processFile(ctx, workerOpts, log, backend, job.file, fileContext{index: job.index, total: len(files)}, plan, discardCounter, discardCounter, false)
				results <- result
			}
		}(i, backend)
	}

	go func() {
		defer close(jobs)
		for index, file := range files {
			select {
			case jobs <- concurrentJob{index: index, file: file}:
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

			up, err := uploadOnly(ctx, o, runLog, uploadBackend, st, fc, uploadFactory)
			if err != nil {
				results <- fileResult{outcome: OutcomeFailed, file: file.Rel, size: st.size, err: err, uploadTime: up.duration}
				continue
			}

			task := &uploadedTask{state: st, upload: up, index: i}
			select {
			case verifyCh <- task:
			case <-ctx.Done():
				rollbackUpload(uploadBackend, st.paths.remoteRel, st.paths.sidecarRel, up.outcome, runLog)
				return
			}
		}
	}()

	verifierWG.Add(1)
	go func() {
		defer verifierWG.Done()
		for task := range verifyCh {
			fc := fileContext{index: task.index, total: len(files), pipeline: true}
			results <- verifyUploaded(ctx, o, runLog, verifyBackend, task.state, task.upload, fc, verifyFactory, quiet)
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
	case OutcomeVerified:
		res.Verified++
		res.Bytes += fr.size
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
