package app

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/fjf/NASArchiveSync/internal/storage"
	"github.com/fjf/NASArchiveSync/internal/xlog"
)

const (
	defaultRetryDelay = time.Second
	maxRetryDelay     = 30 * time.Second
)

// isTransientError reports whether retrying an operation could reasonably help.
// Authentication, path conflicts and local file errors are deliberately not
// retried.
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout() || netErr.Temporary()
	}
	for _, target := range []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		syscall.ECONNRESET,
		syscall.ECONNABORTED,
		syscall.EPIPE,
		syscall.ETIMEDOUT,
	} {
		if errors.Is(err, target) {
			return true
		}
	}

	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection reset",
		"connection refused",
		"broken pipe",
		"use of closed network connection",
		"i/o timeout",
		"temporary failure",
		"timeout",
		"eof",
		"http 408",
		"http 429",
		"http 500",
		"http 502",
		"http 503",
		"http 504",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func retryDelay(o Options, attempt int) time.Duration {
	delay := o.RetryDelay
	if delay <= 0 {
		delay = defaultRetryDelay
	}
	for i := 0; i < attempt && delay < maxRetryDelay; i++ {
		delay *= 2
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}
	}
	return delay
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryValue runs fn until it succeeds, fails permanently or the retry budget
// is exhausted. Before retrying it asks the backend to rebuild its session.
func retryValue[T any](ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, op string, fn func() (T, error)) (T, error) {
	var zero T
	attempts := o.Retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		value, err := fn()
		if err == nil {
			return value, nil
		}
		lastErr = err
		if !isTransientError(err) || attempt == attempts-1 {
			return zero, err
		}
		if log != nil {
			log.Warnf("%s失败，准备第 %d 次重试: %v", op, attempt+1, err)
		}
		if reconnector, ok := backend.(storage.Reconnector); ok {
			if reconnectErr := reconnector.Reconnect(); reconnectErr != nil && log != nil {
				log.Warnf("%s重连失败，将继续重试: %v", op, reconnectErr)
			}
		}
		if err := sleepContext(ctx, retryDelay(o, attempt)); err != nil {
			return zero, err
		}
	}
	return zero, lastErr
}

func retryDo(ctx context.Context, o Options, log *xlog.Logger, backend storage.Backend, op string, fn func() error) error {
	_, err := retryValue(ctx, o, log, backend, op, func() (struct{}, error) {
		return struct{}{}, fn()
	})
	return err
}
