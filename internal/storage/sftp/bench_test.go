package sftp

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fjf/NASArchiveSync/internal/storage"
)

// TestCipherThroughput measures upload and read-back throughput for every
// supported cipher. It is skipped unless NAS_SFTP_BENCH_HOST is set, so normal
// test runs never touch the network.
//
// Example:
//
//	$env:NAS_SFTP_BENCH_HOST="192.168.12.1"
//	$env:NAS_SFTP_BENCH_USER="root"
//	$env:NAS_SFTP_BENCH_DIR="/dev/shm/nassync-bench"
//	go test ./internal/storage/sftp -run TestCipherThroughput -v -timeout 30m
func TestCipherThroughput(t *testing.T) {
	host := os.Getenv("NAS_SFTP_BENCH_HOST")
	if host == "" {
		t.Skip("set NAS_SFTP_BENCH_HOST to run the network cipher benchmark")
	}
	user := envOr("NAS_SFTP_BENCH_USER", currentUser())
	dir := envOr("NAS_SFTP_BENCH_DIR", "/dev/shm/nassync-bench")
	size := envSize("NAS_SFTP_BENCH_SIZE", 2<<30)
	rounds := envInt("NAS_SFTP_BENCH_ROUNDS", 3)
	ciphers := SupportedCiphers
	if v := strings.TrimSpace(os.Getenv("NAS_SFTP_BENCH_CIPHERS")); v != "" {
		ciphers = strings.Split(v, ",")
	}

	target := storage.Target{Scheme: "sftp", Host: host, User: user, Path: dir}
	t.Logf("benchmark: %s@%s:%s size=%.1f GiB rounds=%d ciphers=%v",
		user, host, dir, float64(size)/(1<<30), rounds, ciphers)

	type sample struct {
		up, down float64
	}
	results := make(map[string][]sample, len(ciphers))

	for round := 1; round <= rounds; round++ {
		for _, cipher := range ciphers {
			b, err := Open(target, storage.Options{CreateRoot: true, Cipher: cipher})
			if err != nil {
				t.Fatalf("%s: open: %v", cipher, err)
			}
			up, down := measureTransfer(t, b, size)
			if err := b.Close(); err != nil {
				t.Logf("%s: close: %v", cipher, err)
			}
			results[cipher] = append(results[cipher], sample{up, down})
			t.Logf("round %d %-30s upload %7.1f MiB/s  read-back %7.1f MiB/s",
				round, cipher, up, down)
		}
	}

	fmt.Println("\nmedian results")
	fmt.Println("cipher                          upload MiB/s   read-back MiB/s")
	for _, cipher := range ciphers {
		ups := make([]float64, 0, len(results[cipher]))
		downs := make([]float64, 0, len(results[cipher]))
		for _, s := range results[cipher] {
			ups = append(ups, s.up)
			downs = append(downs, s.down)
		}
		sort.Float64s(ups)
		sort.Float64s(downs)
		fmt.Printf("%-30s %12.1f %16.1f\n", cipher, median(ups), median(downs))
	}
}

func measureTransfer(t *testing.T, b storage.Backend, size int64) (upMiB, downMiB float64) {
	t.Helper()
	const rel = "bench.bin"
	_ = b.Remove(rel)

	w, err := b.Create(rel)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	start := time.Now()
	n, err := io.CopyBuffer(w, io.LimitReader(zeroReader{}, size), make([]byte, 2<<20))
	if err == nil && n != size {
		err = fmt.Errorf("short write: %d != %d", n, size)
	}
	if err == nil {
		err = w.Sync()
	}
	if closeErr := w.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	up := time.Since(start)

	r, err := b.Open(rel)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	start = time.Now()
	n, err = io.CopyBuffer(io.Discard, r, make([]byte, 2<<20))
	if err == nil && n != size {
		err = fmt.Errorf("short read: %d != %d", n, size)
	}
	if closeErr := r.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("read-back: %v", err)
	}
	down := time.Since(start)

	_ = b.Remove(rel)

	mib := float64(size) / (1 << 20)
	return mib / up.Seconds(), mib / down.Seconds()
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { return len(p), nil }

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	return v[len(v)/2]
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envSize(key string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
