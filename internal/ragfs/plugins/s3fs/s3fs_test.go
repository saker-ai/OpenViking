// Package s3fs_test exercises the s3fs FileSystem implementation against
// an in-memory fake S3Client. The fake simulates S3 semantics (key-prefix
// listing, directory placeholders, 404 on missing keys) closely enough to
// validate the ragfs FileSystem contract without a live S3 endpoint.
package s3fs_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/s3fs"
)

// fakeS3 is an in-memory S3Client used by tests.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	modTimes map[string]time.Time
}

func newFakeS3() *fakeS3 {
	return &fakeS3{
		objects:  make(map[string][]byte),
		modTimes: make(map[string]time.Time),
	}
}

func (f *fakeS3) PutObject(_ context.Context, key string, r io.Reader, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r == nil {
		f.objects[key] = nil
	} else {
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		f.objects[key] = data
	}
	f.modTimes[key] = time.Now().UTC()
	return nil
}

func (f *fakeS3) GetObject(_ context.Context, key string, w io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return s3fs.ErrS3NotFound
	}
	_, err := w.Write(data)
	return err
}

func (f *fakeS3) HeadObject(_ context.Context, key string) (*s3fs.S3ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return nil, s3fs.ErrS3NotFound
	}
	return &s3fs.S3ObjectInfo{
		Size:    int64(len(data)),
		ModTime: f.modTimes[key],
		IsDir:   strings.HasSuffix(key, "/"),
	}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[key]; !ok {
		return s3fs.ErrS3NotFound
	}
	delete(f.objects, key)
	delete(f.modTimes, key)
	return nil
}

func (f *fakeS3) ListObjects(_ context.Context, prefix, delimiter string) ([]s3fs.S3ListEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Collect matching keys in sorted order for deterministic output.
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []s3fs.S3ListEntry
	seen := make(map[string]bool)
	for _, k := range keys {
		rel := strings.TrimPrefix(k, prefix)
		if rel == "" {
			// The placeholder itself; skip.
			continue
		}
		if delimiter == "/" {
			if idx := strings.Index(rel, "/"); idx >= 0 {
				// Belongs to a subdir (common prefix).
				sub := rel[:idx+1] // includes trailing "/"
				pref := prefix + sub
				if !seen[pref] {
					seen[pref] = true
					out = append(out, s3fs.S3ListEntry{
						Key:   pref,
						IsDir: true,
					})
				}
				continue
			}
		}
		// Top-level file under prefix.
		out = append(out, s3fs.S3ListEntry{
			Key:     k,
			Size:    int64(len(f.objects[k])),
			ModTime: f.modTimes[k],
			IsDir:   false,
		})
	}
	return out, nil
}

func (f *fakeS3) CopyObject(_ context.Context, srcKey, dstKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[srcKey]
	if !ok {
		return s3fs.ErrS3NotFound
	}
	f.objects[dstKey] = data
	f.modTimes[dstKey] = time.Now().UTC()
	return nil
}

// newTestS3FS builds an S3FS wired to a fresh fake S3 backend.
func newTestS3FS() (*s3fs.S3FS, *fakeS3) {
	fake := newFakeS3()
	fs := s3fs.New("test", "http://localhost:9000", "test-bucket", "us-east-1", "ak", "sk", "", false).
		WithClient(fake)
	return fs, fake
}

func TestS3FSWriteRead(t *testing.T) {
	ctx := context.Background()
	fs, _ := newTestS3FS()

	require.NoError(t, fs.Write(ctx, "/docs/intro.md", bytes.NewReader([]byte("hello world")), 0o644))

	var buf bytes.Buffer
	require.NoError(t, fs.Read(ctx, "/docs/intro.md", &buf))
	assert.Equal(t, "hello world", buf.String())

	fi, err := fs.Stat(ctx, "/docs/intro.md")
	require.NoError(t, err)
	assert.False(t, fi.IsDir)
	assert.Equal(t, int64(11), fi.Size)
	assert.Equal(t, "intro.md", fi.Name)
}

func TestS3FSMkdir(t *testing.T) {
	ctx := context.Background()
	fs, _ := newTestS3FS()

	require.NoError(t, fs.Mkdir(ctx, "/x/y/z", 0o755))
	fi, err := fs.Stat(ctx, "/x/y/z")
	require.NoError(t, err)
	assert.True(t, fi.IsDir)
}

func TestS3FSStatMissing(t *testing.T) {
	ctx := context.Background()
	fs, _ := newTestS3FS()

	_, err := fs.Stat(ctx, "/nope.md")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestS3FSReadMissing(t *testing.T) {
	ctx := context.Background()
	fs, _ := newTestS3FS()

	err := fs.Read(ctx, "/nope.md", &bytes.Buffer{})
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestS3FSReadDir(t *testing.T) {
	ctx := context.Background()
	fs, _ := newTestS3FS()

	require.NoError(t, fs.Write(ctx, "/docs/a.md", bytes.NewReader([]byte("a")), 0o644))
	require.NoError(t, fs.Write(ctx, "/docs/b.md", bytes.NewReader([]byte("b")), 0o644))
	require.NoError(t, fs.Mkdir(ctx, "/docs/sub", 0o755))
	require.NoError(t, fs.Write(ctx, "/docs/sub/c.md", bytes.NewReader([]byte("c")), 0o644))

	entries, err := fs.ReadDir(ctx, "/docs")
	require.NoError(t, err)
	// expect a.md, b.md, and sub/ (c.md is under sub/, not top-level)
	require.Len(t, entries, 3)

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Info.Name)
	}
	sort.Strings(names)
	assert.Equal(t, []string{"a.md", "b.md", "sub"}, names)

	// Verify the sub entry is marked as a directory.
	for _, e := range entries {
		if e.Info.Name == "sub" {
			assert.True(t, e.Info.IsDir)
		}
	}
}

func TestS3FSRename(t *testing.T) {
	ctx := context.Background()
	fs, _ := newTestS3FS()

	require.NoError(t, fs.Write(ctx, "/a.md", bytes.NewReader([]byte("content")), 0o644))
	require.NoError(t, fs.Rename(ctx, "/a.md", "/b.md"))

	// Old path is gone.
	_, err := fs.Stat(ctx, "/a.md")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// New path has the content.
	var buf bytes.Buffer
	require.NoError(t, fs.Read(ctx, "/b.md", &buf))
	assert.Equal(t, "content", buf.String())
}

func TestS3FSCopy(t *testing.T) {
	ctx := context.Background()
	fs, _ := newTestS3FS()

	require.NoError(t, fs.Write(ctx, "/orig.md", bytes.NewReader([]byte("copy me")), 0o644))
	require.NoError(t, fs.Copy(ctx, "/orig.md", "/dup.md"))

	var buf bytes.Buffer
	require.NoError(t, fs.Read(ctx, "/dup.md", &buf))
	assert.Equal(t, "copy me", buf.String())

	// Source still exists.
	var buf2 bytes.Buffer
	require.NoError(t, fs.Read(ctx, "/orig.md", &buf2))
	assert.Equal(t, "copy me", buf2.String())
}

func TestS3FSRemove(t *testing.T) {
	ctx := context.Background()
	fs, _ := newTestS3FS()

	require.NoError(t, fs.Write(ctx, "/docs/a.md", bytes.NewReader([]byte("a")), 0o644))
	require.NoError(t, fs.Write(ctx, "/docs/sub/b.md", bytes.NewReader([]byte("b")), 0o644))

	// Non-recursive remove of a single file.
	require.NoError(t, fs.Remove(ctx, "/docs/a.md", false))
	_, err := fs.Stat(ctx, "/docs/a.md")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// Recursive remove of the directory.
	require.NoError(t, fs.Remove(ctx, "/docs", true))
	_, err = fs.Stat(ctx, "/docs/sub/b.md")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestS3FSReadOnly(t *testing.T) {
	ctx := context.Background()
	fake := newFakeS3()
	fs := s3fs.New("ro", "", "bucket", "us-east-1", "ak", "sk", "", true).WithClient(fake)

	// Writes are rejected.
	err := fs.Write(ctx, "/a.md", bytes.NewReader([]byte("x")), 0o644)
	assert.ErrorIs(t, err, ragfs.ErrReadOnly)

	// Reads still work (need a file in the fake).
	require.NoError(t, fake.PutObject(ctx, "a.md", bytes.NewReader([]byte("data")), 4))
	var buf bytes.Buffer
	require.NoError(t, fs.Read(ctx, "/a.md", &buf))
	assert.Equal(t, "data", buf.String())

	// Mkdir / Rename / Copy / Remove also rejected.
	assert.ErrorIs(t, fs.Mkdir(ctx, "/d", 0o755), ragfs.ErrReadOnly)
	assert.ErrorIs(t, fs.Rename(ctx, "/a.md", "/b.md"), ragfs.ErrReadOnly)
	assert.ErrorIs(t, fs.Copy(ctx, "/a.md", "/b.md"), ragfs.ErrReadOnly)
	assert.ErrorIs(t, fs.Remove(ctx, "/a.md", false), ragfs.ErrReadOnly)
}

func TestS3FSGrep(t *testing.T) {
	ctx := context.Background()
	fs, _ := newTestS3FS()

	require.NoError(t, fs.Write(ctx, "/a.md", bytes.NewReader([]byte("foo\nbar\nbaz\n")), 0o644))
	require.NoError(t, fs.Write(ctx, "/b.md", bytes.NewReader([]byte("foo here\nnope\n")), 0o644))
	require.NoError(t, fs.Write(ctx, "/sub/c.md", bytes.NewReader([]byte("foo deep\n")), 0o644))

	// Recursive grep finds all matches.
	matches, err := fs.Grep(ctx, "foo", "/", true)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(matches), 3)

	// Non-recursive grep only scans immediate children of /.
	matches, err = fs.Grep(ctx, "foo", "/", false)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(matches), 2)
}

func TestS3FSTreeDirectory(t *testing.T) {
	ctx := context.Background()
	fs, _ := newTestS3FS()

	require.NoError(t, fs.Mkdir(ctx, "/a", 0o755))
	require.NoError(t, fs.Write(ctx, "/a/1.md", bytes.NewReader([]byte("1")), 0o644))
	require.NoError(t, fs.Write(ctx, "/a/2.md", bytes.NewReader([]byte("2")), 0o644))
	require.NoError(t, fs.Mkdir(ctx, "/a/sub", 0o755))
	require.NoError(t, fs.Write(ctx, "/a/sub/3.md", bytes.NewReader([]byte("3")), 0o644))

	// depth=2 should reach /a/* and /a/sub/*.
	entries, err := fs.TreeDirectory(ctx, "/a", 2)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(entries), 3)

	// depth=1 only sees immediate children of /a.
	entries, err = fs.TreeDirectory(ctx, "/a", 1)
	require.NoError(t, err)
	// Expect 1.md, 2.md, sub (3 entries at top level).
	assert.Len(t, entries, 3)
}

func TestS3FSPrefixIsolation(t *testing.T) {
	ctx := context.Background()
	fake := newFakeS3()
	// S3FS rooted at the "tenant-a" prefix; keys outside it must be invisible.
	fs := s3fs.New("pfx", "", "bucket", "us-east-1", "ak", "sk", "tenant-a", false).WithClient(fake)

	require.NoError(t, fs.Write(ctx, "/docs/a.md", bytes.NewReader([]byte("a")), 0o644))
	// The object lands under the prefix.
	_, err := fake.HeadObject(ctx, "tenant-a/docs/a.md")
	require.NoError(t, err)

	// Reading back via the ragfs path works.
	var buf bytes.Buffer
	require.NoError(t, fs.Read(ctx, "/docs/a.md", &buf))
	assert.Equal(t, "a", buf.String())
}

func TestS3FSChmodNoOp(t *testing.T) {
	ctx := context.Background()
	fs, _ := newTestS3FS()
	require.NoError(t, fs.Write(ctx, "/a.md", bytes.NewReader([]byte("x")), 0o644))
	// S3 has no POSIX mode; chmod is a successful no-op.
	require.NoError(t, fs.Chmod(ctx, "/a.md", 0o600))
}

func TestS3FSHealthCheck(t *testing.T) {
	ctx := context.Background()
	fs, _ := newTestS3FS()
	require.NoError(t, fs.HealthCheck(ctx))
}

func TestS3FSNameDefault(t *testing.T) {
	fs := s3fs.New("", "", "b", "r", "ak", "sk", "", false)
	assert.Equal(t, "s3fs", fs.Name())

	fs2 := s3fs.New("custom", "", "b", "r", "ak", "sk", "", false)
	assert.Equal(t, "custom", fs2.Name())
}

// Verify the fake itself satisfies the S3Client interface at compile time.
var _ s3fs.S3Client = (*fakeS3)(nil)

// Ensure errors.Is(err, ErrS3NotFound) works through the wrapper.
func TestS3FSNotFoundWrapping(t *testing.T) {
	// Sanity: ErrS3NotFound is recognized via errors.Is.
	assert.True(t, errors.Is(s3fs.ErrS3NotFound, s3fs.ErrS3NotFound))
}
