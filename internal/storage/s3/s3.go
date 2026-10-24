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
	"strings"
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
	defaultRegion   = "us-east-1"
	defaultTimeout  = 30 * time.Second
	partSize        = 32 << 20   // 32 MiB, above the 5 MiB S3 minimum
	copyPartSize    = 1024 << 20 // 1 GiB per UploadPartCopy
	copyObjectLimit = 5 << 30    // CopyObject is limited to 5 GiB
)

// Backend stores objects under bucket/prefix.
type Backend struct {
	client     *awss3.Client
	httpClient *http.Client
	bucket     string
	prefix     string
	style      string
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
	if httpClient == nil && opt.S3Insecure {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit opt-in
		httpClient = &http.Client{Transport: tr}
	}
	if httpClient != nil {
		loadOpts = append(loadOpts, awsconfig.WithHTTPClient(httpClient))
	}

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
		client:     client,
		httpClient: idleCloser,
		bucket:     t.Bucket,
		prefix:     cleanPrefix(t.Path),
		style:      style,
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
	return &writer{
		client: b.client,
		bucket: b.bucket,
		key:    key,
		// Records are only a few hundred bytes, so start small and let the
		// buffer grow to partSize for real archives.
		buf: make([]byte, 0, 64*1024),
	}, nil
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
	client     *awss3.Client
	bucket     string
	key        string
	buf        []byte
	uploadID   string
	parts      []s3types.CompletedPart
	partNumber int32
	closed     bool
	err        error
}

func (w *writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("S3 writer 已关闭")
	}
	if w.err != nil {
		return 0, w.err
	}

	total := 0
	for len(p) > 0 {
		if len(w.buf) >= partSize {
			if err := w.flushPart(); err != nil {
				w.err = err
				return total, err
			}
		}
		space := partSize - len(w.buf)
		n := len(p)
		if n > space {
			n = space
		}
		w.buf = append(w.buf, p[:n]...)
		total += n
		p = p[n:]
		if len(w.buf) >= partSize {
			if err := w.flushPart(); err != nil {
				w.err = err
				return total, err
			}
		}
	}
	return total, nil
}

func (w *writer) Sync() error { return w.err }

func (w *writer) Close() error {
	if w.closed {
		return w.err
	}
	w.closed = true
	if w.err != nil {
		w.abort()
		return w.err
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
			w.err = err
			return err
		}
		return nil
	}

	if len(w.buf) > 0 {
		if err := w.flushPart(); err != nil {
			w.err = err
			w.abort()
			return err
		}
	}
	if _, err := w.client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:   aws.String(w.bucket),
		Key:      aws.String(w.key),
		UploadId: aws.String(w.uploadID),
		MultipartUpload: &s3types.CompletedMultipartUpload{
			Parts: w.parts,
		},
	}); err != nil {
		w.err = err
		w.abort()
		return err
	}
	return nil
}

func (w *writer) flushPart() error {
	if len(w.buf) == 0 {
		return nil
	}
	ctx := context.Background()
	if w.uploadID == "" {
		out, err := w.client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
			Bucket: aws.String(w.bucket),
			Key:    aws.String(w.key),
		})
		if err != nil {
			return fmt.Errorf("创建 S3 分片上传任务失败: %w", err)
		}
		w.uploadID = aws.ToString(out.UploadId)
	}

	w.partNumber++
	out, err := w.client.UploadPart(ctx, &awss3.UploadPartInput{
		Bucket:        aws.String(w.bucket),
		Key:           aws.String(w.key),
		UploadId:      aws.String(w.uploadID),
		PartNumber:    aws.Int32(w.partNumber),
		Body:          bytes.NewReader(w.buf),
		ContentLength: aws.Int64(int64(len(w.buf))),
	})
	if err != nil {
		return fmt.Errorf("上传 S3 分片失败（第 %d 片）: %w", w.partNumber, err)
	}
	w.parts = append(w.parts, s3types.CompletedPart{
		ETag:       out.ETag,
		PartNumber: aws.Int32(w.partNumber),
	})
	w.buf = w.buf[:0]
	return nil
}

func (w *writer) abort() {
	if w.uploadID == "" {
		return
	}
	_, _ = w.client.AbortMultipartUpload(context.Background(), &awss3.AbortMultipartUploadInput{
		Bucket:   aws.String(w.bucket),
		Key:      aws.String(w.key),
		UploadId: aws.String(w.uploadID),
	})
}

var _ storage.Writer = (*writer)(nil)
