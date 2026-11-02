package app

import (
	"context"
	"io"
	"sync"
	"time"
)

// bandwidthLimiter spreads reads over time so several concurrent workers share
// one upload/verification budget. Reservations are serialized, which keeps the
// aggregate rate stable even when range reads run in parallel.
type bandwidthLimiter struct {
	perByte time.Duration

	mu   sync.Mutex
	next time.Time
}

func newBandwidthLimiter(bytesPerSec int64) *bandwidthLimiter {
	if bytesPerSec <= 0 {
		return nil
	}
	perByte := time.Duration(float64(time.Second) / float64(bytesPerSec))
	if perByte < time.Nanosecond {
		perByte = time.Nanosecond
	}
	return &bandwidthLimiter{perByte: perByte}
}

func (l *bandwidthLimiter) wait(ctx context.Context, n int) error {
	if l == nil || n <= 0 {
		return nil
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	l.next = l.next.Add(time.Duration(n) * l.perByte)
	wait := time.Until(l.next)
	l.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	return sleepContext(ctx, wait)
}

type limitedReader struct {
	ctx     context.Context
	r       io.Reader
	limiter *bandwidthLimiter
}

func newLimitedReader(ctx context.Context, r io.Reader, limiter *bandwidthLimiter) io.Reader {
	if limiter == nil {
		return r
	}
	return &limitedReader{ctx: ctx, r: r, limiter: limiter}
}

func (r *limitedReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		if waitErr := r.limiter.wait(r.ctx, n); waitErr != nil && err == nil {
			err = waitErr
		}
	}
	return n, err
}
