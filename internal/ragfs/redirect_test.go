package ragfs_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
)

func TestRedirectRoundTrip(t *testing.T) {
	ctx := context.Background()
	fs := memfs.New("test")

	// No redirect pointer initially.
	r, err := ragfs.LoadRedirect(ctx, fs, "/big.bin")
	require.NoError(t, err)
	assert.Nil(t, r)

	// Write a redirect pointer.
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, ragfs.WriteRedirect(ctx, fs, "/big.bin", &ragfs.Redirect{
		Type:      ragfs.RedirectFileOverSize,
		Target:    "/big.bin.blob",
		Size:      1024,
		CreatedAt: now,
	}))

	r, err = ragfs.LoadRedirect(ctx, fs, "/big.bin")
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Equal(t, ragfs.RedirectFileOverSize, r.Type)
	assert.Equal(t, "/big.bin.blob", r.Target)
	assert.Equal(t, int64(1024), r.Size)

	// FollowRedirect resolves the target.
	target, err := ragfs.FollowRedirect(ctx, fs, "/big.bin")
	require.NoError(t, err)
	assert.Equal(t, "/big.bin.blob", target)

	// No redirect -> FollowRedirect returns original path.
	target, err = ragfs.FollowRedirect(ctx, fs, "/nope.bin")
	require.NoError(t, err)
	assert.Equal(t, "/nope.bin", target)
}

func TestRedirectRemove(t *testing.T) {
	ctx := context.Background()
	fs := memfs.New("test")
	require.NoError(t, ragfs.WriteRedirect(ctx, fs, "/x", &ragfs.Redirect{
		Type:   ragfs.RedirectFileOverSize,
		Target: "/x.blob",
	}))
	require.NoError(t, ragfs.RemoveRedirect(ctx, fs, "/x"))
	r, err := ragfs.LoadRedirect(ctx, fs, "/x")
	require.NoError(t, err)
	assert.Nil(t, r)
}

func TestRedirectAutoTimestamp(t *testing.T) {
	ctx := context.Background()
	fs := memfs.New("test")
	before := time.Now().UTC()
	require.NoError(t, ragfs.WriteRedirect(ctx, fs, "/x", &ragfs.Redirect{
		Type:   ragfs.RedirectFileOverSize,
		Target: "/x.blob",
	}))
	r, err := ragfs.LoadRedirect(ctx, fs, "/x")
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.True(t, r.CreatedAt.After(before) || r.CreatedAt.Equal(before))
}

func TestRedirectNilWritesError(t *testing.T) {
	ctx := context.Background()
	fs := memfs.New("test")
	err := ragfs.WriteRedirect(ctx, fs, "/x", nil)
	assert.Error(t, err)
}

// Test that the sidecar filename for the redirect is `path + .redirect.json`.
func TestRedirectSidecarPath(t *testing.T) {
	ctx := context.Background()
	fs := memfs.New("test")
	require.NoError(t, ragfs.WriteRedirect(ctx, fs, "/docs/big.bin", &ragfs.Redirect{
		Type:   ragfs.RedirectFileOverSize,
		Target: "/docs/big.bin.blob",
	}))
	// the sidecar must be at /docs/big.bin.redirect.json
	var buf bytes.Buffer
	require.NoError(t, fs.Read(ctx, "/docs/big.bin.redirect.json", &buf))
	assert.Contains(t, buf.String(), "file_over_size")
}
