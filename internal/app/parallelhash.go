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
	// maxVerifyConcurrency bounds range-read workers and the buffer pool.
	maxVerifyConcurrency = 16
)

// chunkPool keeps range-read buffers alive between files. A plain channel is
// used instead of sync.Pool because sync.Pool is emptied at every garbage
// collection, which would make each file allocate its buffers again.
type chunkPool struct {
	free chan []byte
	size int
}

func newChunkPool(size, n int) *chunkPool {
	if n < 1 {
		n = 1
	}
	return &chunkPool{free: make(chan []byte, n), size: size}
}

func (p *chunkPool) get() []byte {
	select {
	case b := <-p.free:
		return b[:0]
	default:
		return make([]byte, 0, p.size)
	}
}

// take removes n buffers from the pool, allocating any that are missing.
func (p *chunkPool) take(n int) [][]byte {
	bufs := make([][]byte, n)
	for i := range bufs {
		bufs[i] = p.get()
	}
	return bufs
}

func (p *chunkPool) put(b []byte) {
	if b == nil || cap(b) < p.size {
		return
	}
	select {
	case p.free <- b[:0]:
	default:
	}
}

// hashRemoteRanges computes the SHA256 of a remote file by fetching fixed-size
// byte ranges concurrently. Chunks are still hashed strictly in order, so the
// result is exactly the same as reading the file sequentially.
func hashRemoteRanges(ctx context.Context, rr storage.RangeReader, rel string, sink progress.Sink, size int64, concurrency int, pool *chunkPool) (string, error) {
	return hashRemoteRangesChunked(ctx, rr, rel, sink, size, concurrency, parallelHashChunkSize, pool)
}

func hashRemoteRangesChunked(ctx context.Context, rr storage.RangeReader, rel string, sink progress.Sink, size int64, concurrency int, chunkSize int64, pool *chunkPool) (string, error) {
	nChunks := int((size + chunkSize - 1) / chunkSize)

	workers := concurrency
	if workers > maxVerifyConcurrency {
		workers = maxVerifyConcurrency
	}
	if workers > nChunks {
		workers = nChunks
	}
	if int64(workers)*chunkSize > parallelHashMaxBuffered {
		workers = int(parallelHashMaxBuffered / chunkSize)
	}
	if workers < 1 {
		workers = 1
	}
	if pool == nil {
		pool = newChunkPool(int(chunkSize), workers)
	}

	// Every worker holds one buffer while it waits for a job, and buffers are
	// only recycled after their bytes have been hashed. Taking the buffer
	// first means a worker can never hold a chunk it cannot download, and
	// limits the verification to exactly `workers` buffers.
	free := make(chan []byte, workers)
	for _, b := range pool.take(workers) {
		free <- b
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
			for {
				var buf []byte
				select {
				case buf = <-free:
				case <-ctx.Done():
					return
				}
				idx, ok := <-jobs
				if !ok {
					free <- buf
					return
				}

				start := int64(idx) * chunkSize
				length := chunkSize
				if start+length > size {
					length = size - start
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
				drainBuffers(free, pool)
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
			drainBuffers(free, pool)
			return "", res.err
		}
		_, _ = h.Write(res.data)
		free <- res.data[:0]
		next++
	}

	cancel()
	wg.Wait()
	drainBuffers(free, pool)
	if firstErr != nil {
		return "", firstErr
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// drainBuffers returns every free buffer to the shared pool.
func drainBuffers(free chan []byte, pool *chunkPool) {
	for {
		select {
		case b := <-free:
			pool.put(b)
		default:
			return
		}
	}
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
