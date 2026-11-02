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

	"github.com/fjf/NASArchiveSync/internal/storage"
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

type fakeHashBackend struct {
	fakeRangeReader
	size int64
}

func (b *fakeHashBackend) Kind() string               { return "fake" }
func (b *fakeHashBackend) Root() string               { return "fake" }
func (b *fakeHashBackend) Location(rel string) string { return rel }
func (b *fakeHashBackend) MkdirAll(string) error      { return nil }
func (b *fakeHashBackend) Open(string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(b.data)), nil
}
func (b *fakeHashBackend) Stat(rel string) (storage.FileInfo, error) {
	return storage.FileInfo{Name: rel, Size: b.size}, nil
}
func (b *fakeHashBackend) Create(string) (storage.Writer, error) {
	return nil, errors.New("not implemented")
}
func (b *fakeHashBackend) Remove(string) error         { return nil }
func (b *fakeHashBackend) Rename(string, string) error { return nil }
func (b *fakeHashBackend) Close() error                { return nil }

func TestHashRemoteFileRejectsLargerRemote(t *testing.T) {
	size := int64(parallelHashMinSize)
	data := make([]byte, size+1)
	backend := &fakeHashBackend{
		fakeRangeReader: fakeRangeReader{data: data},
		size:            int64(len(data)),
	}

	_, err := hashRemoteFile(context.Background(), backend, "f.zip", nil, size, 2, nil, nil)
	if err == nil {
		t.Fatal("expected a size mismatch error")
	}
	if calls := backend.calls.Load(); calls != 0 {
		t.Errorf("range reads = %d, want none before the size check", calls)
	}
}

func TestHashRemoteFileAcceptsMatchingSize(t *testing.T) {
	size := int64(parallelHashMinSize)
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*17 + 3)
	}
	want := sha256.Sum256(data)
	backend := &fakeHashBackend{
		fakeRangeReader: fakeRangeReader{data: data},
		size:            size,
	}

	got, err := hashRemoteFile(context.Background(), backend, "f.zip", nil, size, 2, nil, nil)
	if err != nil {
		t.Fatalf("hashRemoteFile: %v", err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Errorf("hash = %s, want %s", got, hex.EncodeToString(want[:]))
	}
}

func TestHashRemoteRangesMatchesSequential(t *testing.T) {
	data := make([]byte, 3*testHashChunk+12345)
	for i := range data {
		data[i] = byte(i*31 + 7)
	}
	want := sha256.Sum256(data)
	fake := &fakeRangeReader{data: data}

	for _, concurrency := range []int{1, 2, 3, 4, 8} {
		got, err := hashRemoteRangesChunked(context.Background(), fake, "f.zip", nil, int64(len(data)), concurrency, testHashChunk, nil, nil)
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
	if _, err := hashRemoteRangesChunked(context.Background(), fake, "f.zip", nil, int64(len(data)), 4, testHashChunk, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := fake.calls.Load(); got != 2 {
		t.Errorf("range reads = %d, want 2", got)
	}
}

func TestHashRemoteRangesPropagatesError(t *testing.T) {
	data := make([]byte, 4*testHashChunk)
	fake := &fakeRangeReader{data: data, fail: testHashChunk}
	if _, err := hashRemoteRangesChunked(context.Background(), fake, "f.zip", nil, int64(len(data)), 4, testHashChunk, nil, nil); err == nil {
		t.Fatal("expected an error")
	}
}

func TestChunkPoolReuse(t *testing.T) {
	p := newChunkPool(testHashChunk, 2)
	first := p.get()
	p.put(first)

	second := p.get()
	if &first[:1][0] != &second[:1][0] {
		t.Error("chunk pool did not reuse the returned buffer")
	}
}

func TestHashRemoteRangesReturnsBuffers(t *testing.T) {
	data := make([]byte, 4*testHashChunk)
	fake := &fakeRangeReader{data: data}
	p := newChunkPool(testHashChunk, 4)
	if _, err := hashRemoteRangesChunked(context.Background(), fake, "f.zip", nil, int64(len(data)), 4, testHashChunk, p, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(p.free); got != 4 {
		t.Errorf("pool holds %d buffers after verification, want 4", got)
	}
}

func TestHashRemoteRangesCanceled(t *testing.T) {
	data := make([]byte, 4*testHashChunk)
	fake := &fakeRangeReader{data: data}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hashRemoteRangesChunked(ctx, fake, "f.zip", nil, int64(len(data)), 4, testHashChunk, nil, nil); err == nil {
		t.Fatal("expected a context error")
	}
}
