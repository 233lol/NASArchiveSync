// Package s3 implements the storage.Backend interface on top of an S3
// compatible object store. It works with AWS S3 and services such as Garage,
// MinIO and Ceph RGW in either path-style or virtual-hosted-style mode.
package s3

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/fjf/NASArchiveSync/internal/storage"
)

const (
	defaultRegion      = "us-east-1"
	defaultTimeout     = 30 * time.Second
	defaultPartSize    = 16 << 20 // 16 MiB, above the 5 MiB S3 minimum
	minPartSize        = 5 << 20
	maxPartSize        = 512 << 20
	defaultConcurrency = 8
	maxConcurrency     = 16
	maxParts           = 9000       // S3 allows 10000; keep headroom
	copyPartSize       = 1024 << 20 // 1 GiB per UploadPartCopy
	copyObjectLimit    = 5 << 30    // CopyObject is limited to 5 GiB
)

// Backend stores objects under bucket/prefix.
type Backend struct {
	client      *awss3.Client
	httpClient  *http.Client
	bucket      string
	prefix      string
	style       string
	partSize    int
	concurrency int
}

// Open connects to the S3 endpoint and verifies that the bucket is reachable.
// Buckets are never created automatically.
func Open(t storage.Target, opt storage.Options) (storage.Backend, error) {
	return openWithHTTPClient(t, opt, nil)
}

// openWithHTTPClient is Open with an optional custom HTTP client. Tests use it
// to reach a local httptest server while still exercising virtual-host URLs.
func openWithHTTPClient(t storage.Target, opt storage.Options, httpClient *http.Client) (storage.Backend, error) {
	if strings.TrimSpace(t.Bucket) == "" {
		return nil, errors.New("S3 bucket 不能为空")
	}

	endpoint, err := normalizeEndpoint(opt.S3Endpoint)
	if err != nil {
		return nil, err
	}
	style, err := resolveStyle(opt.S3Style, endpoint)
	if err != nil {
		return nil, err
	}

	region := strings.TrimSpace(opt.S3Region)
	if region == "" {
		region = defaultRegion
	}
	partSize := int(opt.S3PartSize)
	if partSize <= 0 {
		partSize = defaultPartSize
	}
	if partSize < minPartSize {
		partSize = minPartSize
	}
	if partSize > maxPartSize {
		partSize = maxPartSize
	}
	concurrency := opt.S3Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	if concurrency > maxConcurrency {
		concurrency = maxConcurrency
	}
	accessKey := strings.TrimSpace(opt.S3AccessKey)
	secretKey := opt.S3SecretKey
	if (accessKey == "") != (secretKey == "") {
		return nil, errors.New("--s3-access-key 和 --s3-secret-key 必须同时提供")
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(region),
	}
	if accessKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, opt.S3SessionToken),
		))
	}
	if httpClient == nil {
		httpClient = newHTTPClient(opt.S3Insecure, maxConcurrency*2)
	}
	loadOpts = append(loadOpts, awsconfig.WithHTTPClient(httpClient))

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("加载 S3 配置失败: %w", err)
	}
	client := awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		o.UsePathStyle = style == "path"
		// Garage and some other S3 compatible services reject the newer
		// optional checksum headers, so only send them when an operation
		// actually requires them.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})

	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String(t.Bucket)}); err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("S3 bucket %q 不存在（本工具不会自动创建 bucket）: %w", t.Bucket, err)
		}
		return nil, fmt.Errorf("访问 S3 bucket %q 失败: %w", t.Bucket, err)
	}

	var idleCloser *http.Client
	if c, ok := cfg.HTTPClient.(*http.Client); ok {
		idleCloser = c
	}
	return &Backend{
		client:      client,
		httpClient:  idleCloser,
		bucket:      t.Bucket,
		prefix:      cleanPrefix(t.Path),
		style:       style,
		partSize:    partSize,
		concurrency: concurrency,
	}, nil
}

func normalizeEndpoint(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("解析 S3 endpoint 失败: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("S3 endpoint 只支持 http/https: %s", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("S3 endpoint 缺少主机名: %s", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

func newHTTPClient(insecure bool, maxConns int) *http.Client {
	var tr *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	} else {
		tr = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	if maxConns < 8 {
		maxConns = 8
	}
	tr.MaxIdleConns = maxConns * 2
	tr.MaxIdleConnsPerHost = maxConns
	tr.MaxConnsPerHost = maxConns
	tr.IdleConnTimeout = 90 * time.Second
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.ExpectContinueTimeout = time.Second
	tr.WriteBufferSize = 64 << 10
	tr.ReadBufferSize = 64 << 10
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit opt-in
	}
	return &http.Client{Transport: tr}
}

func resolveStyle(requested, endpoint string) (string, error) {
	switch s := strings.ToLower(strings.TrimSpace(requested)); s {
	case "", "auto":
		return autoStyle(endpoint), nil
	case "path", "virtual":
		return s, nil
	default:
		return "", fmt.Errorf("--s3-style 必须是 auto、path 或 virtual，收到 %q", requested)
	}
}

// autoStyle picks path style for IP addresses and single-label host names,
// because virtual-hosted style needs a wildcard DNS name such as
// bucket.example.com. Domain endpoints use virtual-hosted style.
func autoStyle(endpoint string) string {
	if endpoint == "" {
		return "virtual"
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "path"
	}
	host := u.Hostname()
	if host == "" || strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil {
		return "path"
	}
	if !strings.Contains(host, ".") {
		return "path"
	}
	return "virtual"
}

func cleanPrefix(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, `\`, "/"))
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return strings.Trim(path.Clean("/"+p), "/")
}

func (b *Backend) Kind() string {
	if b.style == "virtual" {
		return "s3-virtual"
	}
	return "s3-path"
}

func (b *Backend) Root() string {
	if b.prefix == "" {
		return "s3://" + b.bucket
	}
	return "s3://" + b.bucket + "/" + b.prefix
}

func (b *Backend) Location(rel string) string {
	clean, err := storage.CleanRel(rel)
	if err != nil || clean == "" {
		return b.Root()
	}
	return "s3://" + b.bucket + "/" + storage.JoinRel(b.prefix, clean)
}

// MkdirAll is a no-op: object stores have no real directories. It still
// validates the relative path so callers get the same safety checks.
func (b *Backend) MkdirAll(rel string) error {
	_, err := b.resolve(rel)
	return err
}

func (b *Backend) Stat(rel string) (storage.FileInfo, error) {
	if rel == "" {
		// Open already verified the bucket; the prefix itself is virtual.
		return storage.FileInfo{Name: b.bucket, IsDir: true}, nil
	}
	key, err := b.resolve(rel)
	if err != nil {
		return storage.FileInfo{}, err
	}
	if key == "" {
		return storage.FileInfo{Name: b.bucket, IsDir: true}, nil
	}

	ctx := context.Background()
	out, err := b.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return storage.FileInfo{
			Name:    path.Base(key),
			Size:    aws.ToInt64(out.ContentLength),
			IsDir:   strings.HasSuffix(key, "/") || aws.ToString(out.ContentType) == "application/x-directory",
			ModTime: aws.ToTime(out.LastModified),
		}, nil
	}
	if !isNotFound(err) {
		return storage.FileInfo{}, err
	}

	// No object at the exact key. Treat it as a directory if any child exists.
	list, listErr := b.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
		Bucket:  aws.String(b.bucket),
		Prefix:  aws.String(key + "/"),
		MaxKeys: aws.Int32(1),
	})
	if listErr != nil {
		return storage.FileInfo{}, listErr
	}
	if len(list.Contents) > 0 {
		return storage.FileInfo{Name: path.Base(key), IsDir: true}, nil
	}
	return storage.FileInfo{}, os.ErrNotExist
}

func (b *Backend) Open(rel string) (io.ReadCloser, error) {
	key, err := b.resolve(rel)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, fmt.Errorf("%w: %s", os.ErrNotExist, b.Root())
	}
	out, err := b.client.GetObject(context.Background(), &awss3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", os.ErrNotExist, b.Location(rel))
		}
		return nil, err
	}
	return out.Body, nil
}

// OpenRange reads length bytes starting at offset using an HTTP Range request.
// The verifier uses it to read large objects over several connections. Servers
// that ignore the Range header would return the whole object, so the response
// length is checked to avoid hashing the same bytes twice.
func (b *Backend) OpenRange(rel string, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("无效的 S3 读取范围: offset=%d length=%d", offset, length)
	}
	key, err := b.resolve(rel)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, fmt.Errorf("%w: %s", os.ErrNotExist, b.Root())
	}
	out, err := b.client.GetObject(context.Background(), &awss3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", os.ErrNotExist, b.Location(rel))
		}
		return nil, err
	}
	if out.ContentLength != nil && *out.ContentLength != length {
		_ = out.Body.Close()
		return nil, fmt.Errorf("S3 服务器未按请求返回字节范围（请求 %d 字节，实际 %d）: %s",
			length, *out.ContentLength, b.Location(rel))
	}
	return out.Body, nil
}

func (b *Backend) Create(rel string) (storage.Writer, error) {
	if rel == "" {
		return nil, errors.New("拒绝写入 bucket 根对象")
	}
	key, err := b.resolve(rel)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, errors.New("拒绝写入 bucket 根对象")
	}
	return newWriter(b.client, b.bucket, key, b.partSize, b.concurrency), nil
}

func (b *Backend) Remove(rel string) error {
	if rel == "" {
		return errors.New("拒绝删除 bucket 根")
	}
	key, err := b.resolve(rel)
	if err != nil {
		return err
	}
	if key == "" {
		return errors.New("拒绝删除 bucket 根")
	}
	_, err = b.client.DeleteObject(context.Background(), &awss3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil && isNotFound(err) {
		return nil
	}
	return err
}

// Rename copies the object and deletes the source, because S3 has no native
// rename. Objects over 5 GiB use a multipart copy.
func (b *Backend) Rename(oldRel, newRel string) error {
	if oldRel == "" || newRel == "" {
		return errors.New("拒绝在 bucket 根上重命名")
	}
	oldKey, err := b.resolve(oldRel)
	if err != nil {
		return err
	}
	newKey, err := b.resolve(newRel)
	if err != nil {
		return err
	}
	if oldKey == "" || newKey == "" {
		return errors.New("拒绝在 bucket 根上重命名")
	}

	ctx := context.Background()
	head, err := b.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(oldKey),
	})
	if err != nil {
		return fmt.Errorf("读取 S3 源对象失败: %w", err)
	}
	if aws.ToInt64(head.ContentLength) <= copyObjectLimit {
		if _, err := b.client.CopyObject(ctx, &awss3.CopyObjectInput{
			Bucket:            aws.String(b.bucket),
			Key:               aws.String(newKey),
			CopySource:        aws.String(copySource(b.bucket, oldKey)),
			MetadataDirective: s3types.MetadataDirectiveCopy,
		}); err != nil {
			return fmt.Errorf("复制 S3 对象失败: %w", err)
		}
	} else if err := b.multipartCopy(ctx, oldKey, newKey, head); err != nil {
		return err
	}

	if _, err := b.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(oldKey),
	}); err != nil {
		return fmt.Errorf("复制成功但删除 S3 临时对象失败: %w", err)
	}
	return nil
}

func (b *Backend) multipartCopy(ctx context.Context, srcKey, dstKey string, head *awss3.HeadObjectOutput) error {
	create, err := b.client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket:      aws.String(b.bucket),
		Key:         aws.String(dstKey),
		ContentType: head.ContentType,
		Metadata:    head.Metadata,
	})
	if err != nil {
		return fmt.Errorf("创建 S3 分片复制任务失败: %w", err)
	}
	uploadID := aws.ToString(create.UploadId)
	abort := func() {
		_, _ = b.client.AbortMultipartUpload(context.Background(), &awss3.AbortMultipartUploadInput{
			Bucket:   aws.String(b.bucket),
			Key:      aws.String(dstKey),
			UploadId: aws.String(uploadID),
		})
	}

	size := aws.ToInt64(head.ContentLength)
	var parts []s3types.CompletedPart
	var partNumber int32
	for start := int64(0); start < size; start += copyPartSize {
		partNumber++
		end := start + copyPartSize - 1
		if end >= size {
			end = size - 1
		}
		out, err := b.client.UploadPartCopy(ctx, &awss3.UploadPartCopyInput{
			Bucket:          aws.String(b.bucket),
			Key:             aws.String(dstKey),
			UploadId:        aws.String(uploadID),
			PartNumber:      aws.Int32(partNumber),
			CopySource:      aws.String(copySource(b.bucket, srcKey)),
			CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", start, end)),
		})
		if err != nil {
			abort()
			return fmt.Errorf("S3 分片复制失败（第 %d 片）: %w", partNumber, err)
		}
		var etag *string
		if out.CopyPartResult != nil {
			etag = out.CopyPartResult.ETag
		}
		parts = append(parts, s3types.CompletedPart{
			ETag:       etag,
			PartNumber: aws.Int32(partNumber),
		})
	}

	if _, err := b.client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:   aws.String(b.bucket),
		Key:      aws.String(dstKey),
		UploadId: aws.String(uploadID),
		MultipartUpload: &s3types.CompletedMultipartUpload{
			Parts: parts,
		},
	}); err != nil {
		abort()
		return fmt.Errorf("完成 S3 分片复制失败: %w", err)
	}
	return nil
}

func (b *Backend) Close() error {
	if b.httpClient != nil {
		b.httpClient.CloseIdleConnections()
	}
	return nil
}

func (b *Backend) resolve(rel string) (string, error) {
	clean, err := storage.CleanRel(rel)
	if err != nil {
		return "", err
	}
	return storage.JoinRel(b.prefix, clean), nil
}

func copySource(bucket, key string) string {
	segments := strings.Split(key, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return bucket + "/" + strings.Join(segments, "/")
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket", "404":
			return true
		}
	}
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	var nf *s3types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	return false
}

type writer struct {
	client      *awss3.Client
	bucket      string
	key         string
	partSize    int
	concurrency int

	buf []byte

	// uploadID, parts and firstErr are guarded by mu. partNumber is only
	// touched by the goroutine calling Write/Close.
	uploadID   string
	parts      []s3types.CompletedPart
	partNumber int32
	closed     bool
	started    bool
	mu         sync.Mutex
	firstErr   error

	sem       chan struct{}
	free      chan []byte
	wg        sync.WaitGroup
	abortOnce sync.Once
}

func newWriter(client *awss3.Client, bucket, key string, partSize, concurrency int) *writer {
	if partSize < minPartSize {
		partSize = minPartSize
	}
	if concurrency < 1 {
		concurrency = defaultConcurrency
	}
	return &writer{
		client:      client,
		bucket:      bucket,
		key:         key,
		partSize:    partSize,
		concurrency: concurrency,
		buf:         make([]byte, 0, 64*1024),
		sem:         make(chan struct{}, concurrency),
		free:        make(chan []byte, concurrency),
	}
}

// SetSize grows the part size for very large objects so the S3 10000-part
// limit is never reached. It is called before the first Write.
func (w *writer) SetSize(size int64) {
	if size <= 0 || w.started {
		return
	}
	part := int64(w.partSize)
	if size > int64(maxParts)*part {
		part = (size + int64(maxParts) - 1) / int64(maxParts)
		const mib = int64(1 << 20)
		part = ((part + mib - 1) / mib) * mib
		if part > maxPartSize {
			part = maxPartSize
		}
		w.partSize = int(part)
		// Very large files need large parts; keep memory bounded by lowering
		// the number of in-flight buffers instead of allocating gigabytes.
		limit := w.concurrency
		switch {
		case w.partSize >= 256<<20:
			limit = 2
		case w.partSize >= 64<<20:
			limit = 4
		}
		if limit < w.concurrency {
			w.concurrency = limit
			w.sem = make(chan struct{}, limit)
			w.free = make(chan []byte, limit)
		}
	}
}

func (w *writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("S3 writer 已关闭")
	}
	if err := w.currentErr(); err != nil {
		return 0, err
	}
	w.started = true

	total := 0
	for len(p) > 0 {
		if len(w.buf) >= w.partSize {
			if err := w.dispatchPart(true); err != nil {
				return total, err
			}
		}
		space := w.partSize - len(w.buf)
		n := len(p)
		if n > space {
			n = space
		}
		w.buf = append(w.buf, p[:n]...)
		total += n
		p = p[n:]
		if len(w.buf) >= w.partSize {
			if err := w.dispatchPart(true); err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

func (w *writer) Sync() error {
	if err := w.currentErr(); err != nil {
		return err
	}
	w.wg.Wait()
	return w.currentErr()
}

func (w *writer) Close() error {
	if w.closed {
		return w.currentErr()
	}
	w.closed = true
	if err := w.currentErr(); err != nil {
		w.abort()
		return err
	}

	ctx := context.Background()
	if w.uploadID == "" {
		// Small object: a single PutObject is cheaper than multipart.
		_, err := w.client.PutObject(ctx, &awss3.PutObjectInput{
			Bucket:        aws.String(w.bucket),
			Key:           aws.String(w.key),
			Body:          bytes.NewReader(w.buf),
			ContentLength: aws.Int64(int64(len(w.buf))),
		})
		if err != nil {
			w.setErr(err)
			return err
		}
		return nil
	}

	if len(w.buf) > 0 {
		if err := w.dispatchPart(false); err != nil {
			w.abort()
			return err
		}
	}
	w.wg.Wait()
	if err := w.currentErr(); err != nil {
		w.abort()
		return err
	}

	w.mu.Lock()
	parts := append([]s3types.CompletedPart(nil), w.parts...)
	uploadID := w.uploadID
	w.mu.Unlock()
	sort.Slice(parts, func(i, j int) bool {
		return aws.ToInt32(parts[i].PartNumber) < aws.ToInt32(parts[j].PartNumber)
	})

	if _, err := w.client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:   aws.String(w.bucket),
		Key:      aws.String(w.key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &s3types.CompletedMultipartUpload{
			Parts: parts,
		},
	}); err != nil {
		w.setErr(err)
		w.abort()
		return err
	}
	return nil
}

// dispatchPart hands the current buffer to a background worker. When refill is
// true it immediately obtains a fresh buffer so Write can keep reading while
// the part uploads in parallel.
func (w *writer) dispatchPart(refill bool) error {
	if len(w.buf) == 0 {
		return nil
	}

	// Backpressure: cap the number of in-flight parts and therefore the
	// number of full-size buffers held in memory.
	w.sem <- struct{}{}
	if err := w.currentErr(); err != nil {
		<-w.sem
		return err
	}

	w.mu.Lock()
	if w.uploadID == "" {
		out, err := w.client.CreateMultipartUpload(context.Background(), &awss3.CreateMultipartUploadInput{
			Bucket: aws.String(w.bucket),
			Key:    aws.String(w.key),
		})
		if err != nil {
			w.mu.Unlock()
			<-w.sem
			w.setErr(fmt.Errorf("创建 S3 分片上传任务失败: %w", err))
			return w.currentErr()
		}
		w.uploadID = aws.ToString(out.UploadId)
	}
	w.partNumber++
	partNumber := w.partNumber
	uploadID := w.uploadID
	w.wg.Add(1)
	w.mu.Unlock()

	buf := w.buf
	if refill {
		w.buf = w.takeBuffer()
	} else {
		w.buf = nil
	}

	go func() {
		defer w.wg.Done()
		defer func() {
			<-w.sem
			w.putBuffer(buf)
		}()

		out, err := w.client.UploadPart(context.Background(), &awss3.UploadPartInput{
			Bucket:        aws.String(w.bucket),
			Key:           aws.String(w.key),
			UploadId:      aws.String(uploadID),
			PartNumber:    aws.Int32(partNumber),
			Body:          bytes.NewReader(buf),
			ContentLength: aws.Int64(int64(len(buf))),
		})
		if err != nil {
			w.setErr(fmt.Errorf("上传 S3 分片失败（第 %d 片）: %w", partNumber, err))
			return
		}
		w.mu.Lock()
		w.parts = append(w.parts, s3types.CompletedPart{
			ETag:       out.ETag,
			PartNumber: aws.Int32(partNumber),
		})
		w.mu.Unlock()
	}()
	return nil
}

func (w *writer) takeBuffer() []byte {
	select {
	case b := <-w.free:
		if cap(b) < w.partSize {
			return make([]byte, 0, w.partSize)
		}
		return b[:0]
	default:
		return make([]byte, 0, w.partSize)
	}
}

func (w *writer) putBuffer(b []byte) {
	if b == nil {
		return
	}
	select {
	case w.free <- b[:0]:
	default:
	}
}

func (w *writer) currentErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.firstErr
}

func (w *writer) setErr(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	if w.firstErr == nil {
		w.firstErr = err
	}
	w.mu.Unlock()
}

func (w *writer) abort() {
	w.mu.Lock()
	uploadID := w.uploadID
	w.mu.Unlock()
	if uploadID == "" {
		return
	}
	w.abortOnce.Do(func() {
		_, _ = w.client.AbortMultipartUpload(context.Background(), &awss3.AbortMultipartUploadInput{
			Bucket:   aws.String(w.bucket),
			Key:      aws.String(w.key),
			UploadId: aws.String(uploadID),
		})
	})
}

var _ storage.Writer = (*writer)(nil)
var _ storage.SizedWriter = (*writer)(nil)
