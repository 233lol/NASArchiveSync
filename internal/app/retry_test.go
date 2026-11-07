package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fjf/NASArchiveSync/internal/storage"
)

type retryTestBackend struct {
	storage.Backend
	reconnects int
}

func (b *retryTestBackend) Reconnect() error {
	b.reconnects++
	return nil
}

func TestRetryValueRetriesTransientFailures(t *testing.T) {
	backend := &retryTestBackend{}
	calls := 0
	got, err := retryValue(context.Background(), Options{Retries: 2, RetryDelay: time.Millisecond}, nil, backend, "test", func() (int, error) {
		calls++
		if calls < 3 {
			return 0, errors.New("connection reset by peer")
		}
		return 42, nil
	})
	if err != nil {
		t.Fatalf("retryValue: %v", err)
	}
	if got != 42 || calls != 3 {
		t.Errorf("got=%d calls=%d, want 42 and 3", got, calls)
	}
	if backend.reconnects != 2 {
		t.Errorf("reconnects=%d, want 2", backend.reconnects)
	}
}

func TestRetryValueStopsOnPermanentFailure(t *testing.T) {
	backend := &retryTestBackend{}
	calls := 0
	_, err := retryValue(context.Background(), Options{Retries: 3, RetryDelay: time.Millisecond}, nil, backend, "test", func() (int, error) {
		calls++
		return 0, errors.New("authentication failed")
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 || backend.reconnects != 0 {
		t.Errorf("calls=%d reconnects=%d, want 1 and 0", calls, backend.reconnects)
	}
}

func TestLimitedReaderPassesData(t *testing.T) {
	const text = "limited reader payload"
	limiter := newBandwidthLimiter(1 << 20)
	reader := newLimitedReader(context.Background(), strings.NewReader(text), limiter)
	buf := make([]byte, len(text))
	n, err := reader.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != text {
		t.Errorf("read %q, want %q", buf[:n], text)
	}
}

func TestBandwidthLimiterDisabled(t *testing.T) {
	if newBandwidthLimiter(0) != nil {
		t.Fatal("zero rate should disable the limiter")
	}
	if newBandwidthLimiter(-1) != nil {
		t.Fatal("negative rate should disable the limiter")
	}
}

type statusTestError struct{ code int }

func (e statusTestError) Error() string       { return "backend request failed" }
func (e statusTestError) HTTPStatusCode() int { return e.code }

type apiTestError struct{ code string }

func (e apiTestError) Error() string     { return "backend api error" }
func (e apiTestError) ErrorCode() string { return e.code }

func TestIsTransientErrorClassifiesStatusCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"smithy 503", statusTestError{503}, true},
		{"smithy 429", statusTestError{429}, true},
		{"smithy 403", statusTestError{403}, false},
		{"webdav 500", &storage.HTTPError{Code: 500}, true},
		{"webdav 404", &storage.HTTPError{Code: 404}, false},
		{"api slowdown", apiTestError{"SlowDown"}, true},
		{"api missing key", apiTestError{"NoSuchKey"}, false},
		{"sftp connection lost", errors.New("ssh: connection lost"), true},
		{"auth failure", errors.New("authentication failed"), false},
		{"canceled", context.Canceled, false},
	}
	for _, tc := range tests {
		if got := isTransientError(tc.err); got != tc.want {
			t.Errorf("%s: isTransientError = %v, want %v", tc.name, got, tc.want)
		}
	}
}
