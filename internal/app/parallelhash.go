package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sync"

	"github.com/fjf/NASArchiveSync/internal/progress"
	"github.com/fjf/NASArchiveSync/internal/storage"
)

const (
	// parallelHashMinSize is the file size above which range reads are worth
	// the extra requests. Smaller files are read over one connection.
	parallelHashMinSize = 32 << 20
	// parallelHashChunkSize is the size of each range request.
	parallelHashChunkSize = 16 << 20
	// parallelHashMaxBuffered caps the memory used by in-flight chunks,
	// regardless of how many workers were requested.
	parallelHashMaxBuffered = 256 << 20
)

// hashRemoteRanges computes the SHA256 of a remote file by fetching fixed-size
// byte ranges concurrently. Chunks are still hashed strictly in order, so the
// result is exactly the same as reading the file sequentially.
func hashRemoteRanges(ctx context.Context, rr storage.RangeReader, rel string, sink progress.Sink, size int64, concurrency int) (string, error) {
	return hashRemoteRangesChunked(ctx, rr, rel, sink, size, concurrency, parallelHashChunkSize)
}

func hashRemoteRangesChunked(ctx context.Context, rr storage.RangeReader, rel string, sink progress.Sink, size int64, concurrency int, chunkSize int64) (string, error) {
	nChunks := int((size + chunkSize - 1) / chunkSize)

	workers := concurrency
	if workers > nChunks {
		workers = nChunks
	}
	if int64(workers)*chunkSize > parallelHashMaxBuffered {
		workers = int(parallelHashMaxBuffered / chunkSize)
	}
	if workers < 1 {
		workers = 1
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type rangeResult struct {
		index int
		data  []byte
		err   error
	}

	jobs := make(chan int, workers)
	done := make(chan rangeResult, workers)
	free := make(chan []byte, workers)
	for i := 0; i < workers; i++ {
		free <- make([]byte, 0, chunkSize)
	}

	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	fail := func(err error) {
		if err == nil {
			return
		}
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				start := int64(idx) * chunkSize
				length := chunkSize
				if start+length > size {
					length = size - start
				}

				var buf []byte
				select {
				case buf = <-free:
				case <-ctx.Done():
					return
				}
				if int64(cap(buf)) < length {
					buf = make([]byte, length)
				} else {
					buf = buf[:length]
				}

				n, err := readRange(ctx, rr, rel, start, length, buf, sink)
				if err == nil && int64(n) != length {
					err = io.ErrUnexpectedEOF
				}
				if err != nil {
					fail(err)
				}

				select {
				case done <- rangeResult{index: idx, data: buf, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for i := 0; i < nChunks; i++ {
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()

	h := sha256.New()
	pending := make(map[int]rangeResult, workers)
	next := 0
	for next < nChunks {
		res, ok := pending[next]
		if ok {
			delete(pending, next)
		} else {
			select {
			case res = <-done:
			case <-ctx.Done():
				cancel()
				wg.Wait()
				if firstErr != nil {
					return "", firstErr
				}
				return "", ctx.Err()
			}
			if res.index != next {
				pending[res.index] = res
				continue
			}
		}

		if res.err != nil {
			cancel()
			wg.Wait()
			return "", res.err
		}
		_, _ = h.Write(res.data)
		select {
		case free <- res.data[:0]:
		default:
		}
		next++
	}

	cancel()
	wg.Wait()
	if firstErr != nil {
		return "", firstErr
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readRange fetches one range into buf and reports progress as bytes arrive.
func readRange(ctx context.Context, rr storage.RangeReader, rel string, offset, length int64, buf []byte, sink progress.Sink) (int, error) {
	rc, err := rr.OpenRange(rel, offset, length)
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	var r io.Reader = ctxReader{ctx: ctx, r: rc}
	if sink != nil {
		r = progress.NewReader(r, sink)
	}
	return io.ReadFull(r, buf)
}
