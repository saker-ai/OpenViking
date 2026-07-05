package ragfs_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
)

func TestLayerAbstractRoundTrip(t *testing.T) {
	ctx := context.Background()
	fs := memfs.New("test")

	// Initially absent.
	got, err := ragfs.ReadAbstract(ctx, fs, "/docs/intro.md")
	require.NoError(t, err)
	assert.Equal(t, "", got)

	has, err := ragfs.HasAbstract(ctx, fs, "/docs/intro.md")
	require.NoError(t, err)
	assert.False(t, has)

	// Write the abstract and read it back.
	require.NoError(t, ragfs.WriteAbstract(ctx, fs, "/docs/intro.md", "L0 abstract text"))

	got, err = ragfs.ReadAbstract(ctx, fs, "/docs/intro.md")
	require.NoError(t, err)
	assert.Equal(t, "L0 abstract text", got)

	has, err = ragfs.HasAbstract(ctx, fs, "/docs/intro.md")
	require.NoError(t, err)
	assert.True(t, has)
}

func TestLayerOverviewRoundTrip(t *testing.T) {
	ctx := context.Background()
	fs := memfs.New("test")
	require.NoError(t, ragfs.WriteOverview(ctx, fs, "/docs/intro.md", "L1 overview"))
	got, err := ragfs.ReadOverview(ctx, fs, "/docs/intro.md")
	require.NoError(t, err)
	assert.Equal(t, "L1 overview", got)
}

func TestLayerChunks(t *testing.T) {
	ctx := context.Background()
	fs := memfs.New("test")

	// No chunks dir initially.
	chunks, err := ragfs.ListChunks(ctx, fs, "/docs/intro.md")
	require.NoError(t, err)
	assert.Empty(t, chunks)

	// Write three chunks; list returns them in lexicographic order.
	require.NoError(t, ragfs.WriteChunk(ctx, fs, "/docs/intro.md", "chunk_002", "c2"))
	require.NoError(t, ragfs.WriteChunk(ctx, fs, "/docs/intro.md", "chunk_000", "c0"))
	require.NoError(t, ragfs.WriteChunk(ctx, fs, "/docs/intro.md", "chunk_001", "c1"))

	chunks, err = ragfs.ListChunks(ctx, fs, "/docs/intro.md")
	require.NoError(t, err)
	require.Len(t, chunks, 3)
	assert.Equal(t, []string{"chunk_000", "chunk_001", "chunk_002"}, chunks)

	for i, name := range chunks {
		got, err := ragfs.ReadChunk(ctx, fs, "/docs/intro.md", name)
		require.NoError(t, err)
		assert.Equal(t, "c"+string(rune('0'+i)), got)
	}
}

func TestLayerHiddenGeneric(t *testing.T) {
	ctx := context.Background()
	fs := memfs.New("test")
	require.NoError(t, ragfs.WriteHidden(ctx, fs, "/a.md", ".custom", "custom"))
	got, err := ragfs.ReadHidden(ctx, fs, "/a.md", ".custom")
	require.NoError(t, err)
	assert.Equal(t, "custom", got)
}

func TestLayerDirResource(t *testing.T) {
	ctx := context.Background()
	fs := memfs.New("test")
	// directory resource: sidecar lives inside the directory
	require.NoError(t, fs.Mkdir(ctx, "/docs", 0o755))
	require.NoError(t, ragfs.WriteAbstract(ctx, fs, "/docs/", "dir abstract"))
	got, err := ragfs.ReadAbstract(ctx, fs, "/docs/")
	require.NoError(t, err)
	assert.Equal(t, "dir abstract", got)

	// verify the sidecar is at /docs/.abstract (inside the dir)
	var buf bytes.Buffer
	require.NoError(t, fs.Read(ctx, "/docs/.abstract", &buf))
	assert.Equal(t, "dir abstract", buf.String())
}
