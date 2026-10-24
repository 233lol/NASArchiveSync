package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync/atomic"
	"testing"
)

const testHashChunk = 1 << 20

type fakeRangeReader struct {
	data  []byte
	fail  int64 // return an error for a chunk starting at this offset
	calls atomic.Int32
}

func (f *fakeRangeReader) OpenRange(rel string, offset, length int64) (io.ReadCloser, error) {
	f.calls.Add(1)
	if f.fail > 0 && offset == f.fail {
		return nil, errors.New("range read failed")
	}
	if offset < 0 || length <= 0 || offset+length > int64(len(f.data)) {
		return nil, errors.New("bad range")
	}
	return io.NopCloser(bytes.NewReader(f.data[offset : offset+length])), nil
}

func TestHashRemoteRangesMatchesSequential(t *testing.T) {
	data := make([]byte, 3*testHashChunk+12345)
	for i := range data {
		data[i] = byte(i*31 + 7)
	}
	want := sha256.Sum256(data)
	fake := &fakeRangeReader{data: data}

	for _, concurrency := range []int{1, 2, 3, 4, 8} {
		got, err := hashRemoteRangesChunked(context.Background(), fake, "f.zip", nil, int64(len(data)), concurrency, testHashChunk)
		if err != nil {
			t.Fatalf("concurrency=%d: %v", concurrency, err)
		}
		if got != hex.EncodeToString(want[:]) {
			t.Fatalf("concurrency=%d: hash = %s, want %s", concurrency, got, hex.EncodeToString(want[:]))
		}
	}
}

func TestHashRemoteRangesDoesNotSkipChunks(t *testing.T) {
	data := make([]byte, 2*testHashChunk)
	fake := &fakeRangeReader{data: data}
	if _, err := hashRemoteRangesChunked(context.Background(), fake, "f.zip", nil, int64(len(data)), 4, testHashChunk); err != nil {
		t.Fatal(err)
	}
	if got := fake.calls.Load(); got != 2 {
		t.Errorf("range reads = %d, want 2", got)
	}
}

func TestHashRemoteRangesPropagatesError(t *testing.T) {
	data := make([]byte, 4*testHashChunk)
	fake := &fakeRangeReader{data: data, fail: testHashChunk}
	if _, err := hashRemoteRangesChunked(context.Background(), fake, "f.zip", nil, int64(len(data)), 4, testHashChunk); err == nil {
		t.Fatal("expected an error")
	}
}

func TestHashRemoteRangesCanceled(t *testing.T) {
	data := make([]byte, 4*testHashChunk)
	fake := &fakeRangeReader{data: data}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hashRemoteRangesChunked(ctx, fake, "f.zip", nil, int64(len(data)), 4, testHashChunk); err == nil {
		t.Fatal("expected a context error")
	}
}
