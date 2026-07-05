// Package s3fs implements the ragfs FileSystem interface backed by an S3-
// compatible object store via the AWS Go SDK v2.
//
// It is the Go counterpart of crates/ragfs/src/plugins/s3fs/. The S3FS
// struct holds configuration (endpoint, bucket, region, credentials,
// prefix, readOnly) and a lazily-constructed aws-sdk-go-v2 S3 client.
// Data-plane methods map ragfs paths to S3 object keys under the
// configured prefix and perform real S3 operations (GET/PUT/DELETE/HEAD/
// LIST/COPY). S3 has no native directories: a directory is conventionally
// a zero-byte placeholder key with a trailing "/". Rename is COPY + DELETE
// and is not atomic. Chmod is a no-op because S3 has no POSIX mode.
//
// Callers may inject a custom S3Client via WithClient (e.g. for tests);
// otherwise the AWS SDK v2 adapter is constructed on first use.
package s3fs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
)

// S3ObjectInfo describes a single S3 object as seen by the HeadObject call.
type S3ObjectInfo struct {
	Size    int64
	ModTime time.Time
	IsDir   bool
}

// S3ListEntry is one row in a ListObjects result. Files populate Key/Size/
// ModTime; directories (common prefixes) populate Key with the trailing
// slash and IsDir=true.
type S3ListEntry struct {
	Key     string
	IsDir   bool
	Size    int64
	ModTime time.Time
}

// S3Client is the minimal interface S3FS uses to talk to S3. The aws-sdk-
// go-v2 adapter in this file implements it; tests may inject a fake.
type S3Client interface {
	PutObject(ctx context.Context, key string, r io.Reader, size int64) error
	GetObject(ctx context.Context, key string, w io.Writer) error
	HeadObject(ctx context.Context, key string) (*S3ObjectInfo, error)
	DeleteObject(ctx context.Context, key string) error
	ListObjects(ctx context.Context, prefix, delimiter string) ([]S3ListEntry, error)
	CopyObject(ctx context.Context, srcKey, dstKey string) error
}

// ErrS3NotFound is returned by S3Client methods when the target object is
// missing. S3FS converts it to domain.ErrNotFound so callers can use
// errors.Is uniformly.
var ErrS3NotFound = errors.New("s3 object not found")

// S3FS is an S3-backed ragfs FileSystem. The zero value is NOT usable;
// use New.
type S3FS struct {
	name      string
	endpoint  string
	bucket    string
	region    string
	accessKey string
	secretKey string
	prefix    string
	readOnly  bool

	// clientMu guards client construction so concurrent first-use calls
	// don't race to build the adapter twice.
	clientMu sync.Mutex
	client   S3Client
}

// New constructs an S3FS from config fields. The AWS SDK v2 client is
// constructed lazily on first data-plane call so New never fails.
func New(name, endpoint, bucket, region, accessKey, secretKey, prefix string, readOnly bool) *S3FS {
	return &S3FS{
		name:      name,
		endpoint:  endpoint,
		bucket:    bucket,
		region:    region,
		accessKey: accessKey,
		secretKey: secretKey,
		prefix:    prefix,
		readOnly:  readOnly,
	}
}

// WithClient injects an S3Client (used by tests; production callers let
// the AWS SDK v2 adapter be constructed on first use).
func (s *S3FS) WithClient(c S3Client) *S3FS {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	s.client = c
	return s
}

// ensureClient lazily constructs the aws-sdk-go-v2 S3 adapter on first
// use. Subsequent calls are no-ops.
func (s *S3FS) ensureClient(ctx context.Context) error {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	if s.client != nil {
		return nil
	}
	c, err := NewAWSClient(ctx, s.endpoint, s.region, s.accessKey, s.secretKey, s.bucket)
	if err != nil {
		return ragfs.WrapRAGFS(err)
	}
	s.client = c
	return nil
}

// --- ServicePlugin ---

func (s *S3FS) Name() string {
	if s.name == "" {
		return "s3fs"
	}
	return s.name
}

func (s *S3FS) Validate(*ragfs.PluginConfig) error { return nil }

func (s *S3FS) Initialize(ctx context.Context, _ *ragfs.PluginConfig) error {
	return s.ensureClient(ctx)
}

func (s *S3FS) HealthCheck(ctx context.Context) error {
	if err := s.ensureClient(ctx); err != nil {
		return err
	}
	// A ListObjects with the configured prefix and "/" delimiter probes
	// both bucket reachability and credential validity. We tolerate
	// ErrS3NotFound (empty prefix) as healthy.
	_, err := s.client.ListObjects(ctx, s.keyFor("/", true), "/")
	if err != nil && !errors.Is(err, ErrS3NotFound) {
		return ragfs.WrapRAGFS(err)
	}
	return nil
}

// --- FileSystem ---

func (s *S3FS) Create(ctx context.Context, p string, isDir bool) error {
	if s.readOnly {
		return ragfs.ErrReadOnly
	}
	if err := s.ensureClient(ctx); err != nil {
		return err
	}
	return s.wrapNotFound(s.client.PutObject(ctx, s.keyFor(p, isDir), nil, 0))
}

func (s *S3FS) Mkdir(ctx context.Context, p string, _ os.FileMode) error {
	if s.readOnly {
		return ragfs.ErrReadOnly
	}
	if err := s.ensureClient(ctx); err != nil {
		return err
	}
	// S3 directories are zero-byte placeholder keys with trailing "/".
	return s.wrapNotFound(s.client.PutObject(ctx, s.keyFor(p, true), nil, 0))
}

func (s *S3FS) Remove(ctx context.Context, p string, recursive bool) error {
	if s.readOnly {
		return ragfs.ErrReadOnly
	}
	if err := s.ensureClient(ctx); err != nil {
		return err
	}
	if recursive {
		// List all objects under the directory prefix and delete them.
		entries, err := s.client.ListObjects(ctx, s.keyFor(p, true), "")
		if err != nil {
			return ragfs.WrapRAGFS(err)
		}
		for _, e := range entries {
			if err := s.client.DeleteObject(ctx, e.Key); err != nil {
				return ragfs.WrapRAGFS(err)
			}
		}
		return nil
	}
	// Non-recursive: delete the file key, then best-effort delete the
	// directory placeholder (S3 has no "is this a dir?" check).
	key := s.keyFor(p, false)
	if err := s.client.DeleteObject(ctx, key); err != nil {
		return s.wrapNotFound(err)
	}
	_ = s.client.DeleteObject(ctx, s.keyFor(p, true))
	return nil
}

func (s *S3FS) Read(ctx context.Context, p string, w io.Writer) error {
	if err := s.ensureClient(ctx); err != nil {
		return err
	}
	return s.wrapNotFound(s.client.GetObject(ctx, s.keyFor(p, false), w))
}

func (s *S3FS) Write(ctx context.Context, p string, r io.Reader, _ os.FileMode) error {
	if s.readOnly {
		return ragfs.ErrReadOnly
	}
	if err := s.ensureClient(ctx); err != nil {
		return err
	}
	return s.wrapNotFound(s.client.PutObject(ctx, s.keyFor(p, false), r, -1))
}

func (s *S3FS) ReadDir(ctx context.Context, p string) ([]*ragfs.TreeEntry, error) {
	if err := s.ensureClient(ctx); err != nil {
		return nil, err
	}
	prefix := s.keyFor(p, true)
	entries, err := s.client.ListObjects(ctx, prefix, "/")
	if err != nil {
		return nil, ragfs.WrapRAGFS(err)
	}
	out := make([]*ragfs.TreeEntry, 0, len(entries))
	for _, e := range entries {
		name := s.entryName(e, prefix)
		if name == "" {
			continue
		}
		child := ragfs.Join(p, name)
		out = append(out, &ragfs.TreeEntry{
			Path:    child,
			RelPath: child,
			Info: &ragfs.FileInfo{
				Name:    name,
				Size:    e.Size,
				IsDir:   e.IsDir,
				ModTime: e.ModTime,
				Mode:    0o644,
			},
		})
	}
	return out, nil
}

func (s *S3FS) Stat(ctx context.Context, p string) (*ragfs.FileInfo, error) {
	if err := s.ensureClient(ctx); err != nil {
		return nil, err
	}
	// Try as a regular object first.
	info, err := s.client.HeadObject(ctx, s.keyFor(p, false))
	if err == nil {
		return &ragfs.FileInfo{
			Name:    ragfs.Base(p),
			Size:    info.Size,
			IsDir:   false,
			ModTime: info.ModTime,
			Mode:    0o644,
		}, nil
	}
	if !errors.Is(err, ErrS3NotFound) {
		return nil, ragfs.WrapRAGFS(err)
	}
	// Try as a directory placeholder key.
	info, err = s.client.HeadObject(ctx, s.keyFor(p, true))
	if err == nil {
		return &ragfs.FileInfo{
			Name:    ragfs.Base(p),
			Size:    0,
			IsDir:   true,
			ModTime: info.ModTime,
			Mode:    0o755,
		}, nil
	}
	if !errors.Is(err, ErrS3NotFound) {
		return nil, ragfs.WrapRAGFS(err)
	}
	// Try as a virtual directory: any objects under the prefix?
	entries, err := s.client.ListObjects(ctx, s.keyFor(p, true), "/")
	if err != nil {
		return nil, ragfs.WrapRAGFS(err)
	}
	if len(entries) > 0 {
		return &ragfs.FileInfo{
			Name:  ragfs.Base(p),
			Size:  0,
			IsDir: true,
			Mode:  0o755,
		}, nil
	}
	return nil, domain.ErrNotFound
}

func (s *S3FS) Rename(ctx context.Context, oldP, newP string) error {
	if s.readOnly {
		return ragfs.ErrReadOnly
	}
	if err := s.ensureClient(ctx); err != nil {
		return err
	}
	src := s.keyFor(oldP, false)
	dst := s.keyFor(newP, false)
	// S3 rename is COPY + DELETE (not atomic).
	if err := s.client.CopyObject(ctx, src, dst); err != nil {
		return s.wrapNotFound(err)
	}
	return s.wrapNotFound(s.client.DeleteObject(ctx, src))
}

func (s *S3FS) Copy(ctx context.Context, srcP, dstP string) error {
	if s.readOnly {
		return ragfs.ErrReadOnly
	}
	if err := s.ensureClient(ctx); err != nil {
		return err
	}
	return s.wrapNotFound(s.client.CopyObject(ctx, s.keyFor(srcP, false), s.keyFor(dstP, false)))
}

func (s *S3FS) Chmod(_ context.Context, _ string, _ os.FileMode) error {
	// S3 has no POSIX mode bits; chmod is a successful no-op.
	return nil
}

func (s *S3FS) Grep(ctx context.Context, pattern, p string, recursive bool) ([]ragfs.GrepMatch, error) {
	if err := s.ensureClient(ctx); err != nil {
		return nil, err
	}
	// Determine which keys to scan.
	prefix := s.keyFor(p, true)
	var delimiter string
	if !recursive {
		delimiter = "/"
	}
	entries, err := s.client.ListObjects(ctx, prefix, delimiter)
	if err != nil {
		return nil, ragfs.WrapRAGFS(err)
	}
	var matches []ragfs.GrepMatch
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		var buf bytes.Buffer
		if err := s.client.GetObject(ctx, e.Key, &buf); err != nil {
			return nil, ragfs.WrapRAGFS(err)
		}
		ms, err := grepBytes(pattern, s.pathFromKey(e.Key), buf.Bytes())
		if err != nil {
			return nil, ragfs.WrapRAGFS(err)
		}
		matches = append(matches, ms...)
	}
	return matches, nil
}

func (s *S3FS) TreeDirectory(ctx context.Context, p string, depth int) ([]*ragfs.TreeEntry, error) {
	if err := s.ensureClient(ctx); err != nil {
		return nil, err
	}
	if depth <= 0 {
		depth = 1
	}
	var out []*ragfs.TreeEntry
	if err := s.walkTree(ctx, p, depth, 1, &out); err != nil {
		return nil, ragfs.WrapRAGFS(err)
	}
	return out, nil
}

// --- internals ---

// walkTree recursively lists entries under p up to `depth` levels.
func (s *S3FS) walkTree(ctx context.Context, p string, depth, current int, out *[]*ragfs.TreeEntry) error {
	prefix := s.keyFor(p, true)
	entries, err := s.client.ListObjects(ctx, prefix, "/")
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := s.entryName(e, prefix)
		if name == "" {
			continue
		}
		child := ragfs.Join(p, name)
		*out = append(*out, &ragfs.TreeEntry{
			Path:    child,
			RelPath: child,
			Info: &ragfs.FileInfo{
				Name:    name,
				Size:    e.Size,
				IsDir:   e.IsDir,
				ModTime: e.ModTime,
				Mode:    0o644,
			},
		})
		if e.IsDir && current < depth {
			if err := s.walkTree(ctx, child, depth, current+1, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// keyFor maps a ragfs path to an S3 object key under the configured
// prefix. Trailing slash on `p` or isDir=true signals a directory
// placeholder.
func (s *S3FS) keyFor(p string, isDir bool) string {
	n := strings.TrimPrefix(ragfs.Normalize(p), "/")
	if isDir {
		if n != "" && !strings.HasSuffix(n, "/") {
			n += "/"
		}
	}
	if s.prefix == "" {
		return n
	}
	if n == "" {
		return s.prefix + "/"
	}
	return s.prefix + "/" + n
}

// pathFromKey converts an S3 object key back to a ragfs path, stripping
// the configured prefix and any trailing directory slash.
func (s *S3FS) pathFromKey(key string) string {
	n := key
	if s.prefix != "" {
		n = strings.TrimPrefix(n, s.prefix)
		n = strings.TrimPrefix(n, "/")
	}
	n = strings.TrimSuffix(n, "/")
	if n == "" {
		return "/"
	}
	return ragfs.Normalize("/" + n)
}

// entryName extracts the relative name of a list entry under prefix.
// Returns "" for the prefix placeholder itself (so callers can skip it).
func (s *S3FS) entryName(e S3ListEntry, prefix string) string {
	rel := strings.TrimPrefix(e.Key, prefix)
	if e.IsDir {
		rel = strings.TrimSuffix(rel, "/")
	}
	if rel == "" || rel == "/" {
		return ""
	}
	return rel
}

// wrapNotFound converts ErrS3NotFound into domain.ErrNotFound so callers
// can use errors.Is(err, domain.ErrNotFound) uniformly.
func (s *S3FS) wrapNotFound(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrS3NotFound) {
		return domain.ErrNotFound
	}
	return ragfs.WrapRAGFS(err)
}

// grepBytes scans content line-by-line for a literal pattern match.
func grepBytes(pattern, path string, content []byte) ([]ragfs.GrepMatch, error) {
	if pattern == "" {
		return nil, nil
	}
	var out []ragfs.GrepMatch
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	ln := 0
	for scanner.Scan() {
		ln++
		line := scanner.Text()
		if strings.Contains(line, pattern) {
			out = append(out, ragfs.GrepMatch{
				Path:    path,
				LineNo:  ln,
				Line:    line,
				Pattern: pattern,
			})
		}
	}
	return out, scanner.Err()
}

// --- aws-sdk-go-v2 adapter ---

// awsS3Adapter implements S3Client using the AWS Go SDK v2.
type awsS3Adapter struct {
	client *s3.Client
	bucket string
}

// NewAWSClient builds an S3Client backed by the AWS Go SDK v2. If
// accessKey is empty, the SDK's default credential chain (env vars,
// IMDS, etc.) is used. If endpoint is set, path-style addressing is
// enabled for MinIO and other S3-compatible stores.
func NewAWSClient(ctx context.Context, endpoint, region, accessKey, secretKey, bucket string) (S3Client, error) {
	var opts []func(*config.LoadOptions) error
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	if accessKey != "" {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		}
	})
	return &awsS3Adapter{client: client, bucket: bucket}, nil
}

func (a *awsS3Adapter) PutObject(ctx context.Context, key string, r io.Reader, _ int64) error {
	body := r
	if body == nil {
		body = bytes.NewReader(nil)
	}
	_, err := a.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
		Body:   body,
	})
	return normalizeS3Err(err)
}

func (a *awsS3Adapter) GetObject(ctx context.Context, key string, w io.Writer) error {
	out, err := a.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return normalizeS3Err(err)
	}
	defer out.Body.Close()
	if _, err := io.Copy(w, out.Body); err != nil {
		return err
	}
	return nil
}

func (a *awsS3Adapter) HeadObject(ctx context.Context, key string) (*S3ObjectInfo, error) {
	out, err := a.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, normalizeS3Err(err)
	}
	return &S3ObjectInfo{
		Size:    aws.ToInt64(out.ContentLength),
		ModTime: aws.ToTime(out.LastModified),
		IsDir:   strings.HasSuffix(key, "/"),
	}, nil
}

func (a *awsS3Adapter) DeleteObject(ctx context.Context, key string) error {
	_, err := a.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
	})
	return normalizeS3Err(err)
}

func (a *awsS3Adapter) ListObjects(ctx context.Context, prefix, delimiter string) ([]S3ListEntry, error) {
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(a.bucket),
		Prefix: aws.String(prefix),
	}
	if delimiter != "" {
		input.Delimiter = aws.String(delimiter)
	}
	var out []S3ListEntry
	paginator := s3.NewListObjectsV2Paginator(a.client, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, normalizeS3Err(err)
		}
		for i := range page.Contents {
			obj := &page.Contents[i]
			out = append(out, S3ListEntry{
				Key:     aws.ToString(obj.Key),
				Size:    aws.ToInt64(obj.Size),
				ModTime: aws.ToTime(obj.LastModified),
				IsDir:   false,
			})
		}
		for i := range page.CommonPrefixes {
			cp := &page.CommonPrefixes[i]
			out = append(out, S3ListEntry{
				Key:   aws.ToString(cp.Prefix),
				IsDir: true,
			})
		}
	}
	return out, nil
}

func (a *awsS3Adapter) CopyObject(ctx context.Context, srcKey, dstKey string) error {
	_, err := a.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(a.bucket),
		Key:        aws.String(dstKey),
		CopySource: aws.String(a.bucket + "/" + srcKey),
	})
	return normalizeS3Err(err)
}

// normalizeS3Err maps AWS SDK 404 errors to ErrS3NotFound so S3FS can
// convert them to domain.ErrNotFound uniformly.
func normalizeS3Err(err error) error {
	if err == nil {
		return nil
	}
	if isS3NotFound(err) {
		return ErrS3NotFound
	}
	return err
}

// isS3NotFound reports whether err is an S3 404 (NoSuchKey, NotFound, or
// any HTTP 404 from the smithy transport layer).
func isS3NotFound(err error) bool {
	var hse interface{ HTTPStatusCode() int }
	if errors.As(err, &hse) {
		return hse.HTTPStatusCode() == 404
	}
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	return false
}

// compile-time guard: S3FS satisfies ragfs.FileSystem.
var _ ragfs.FileSystem = (*S3FS)(nil)
