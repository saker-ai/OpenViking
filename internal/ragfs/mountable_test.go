package ragfs_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
)

func TestMountableFSRouting(t *testing.T) {
	ctx := context.Background()
	mnt := ragfs.NewMountableFS()
	docs := memfs.New("docs")
	sessions := memfs.New("sessions")
	root := memfs.New("root")

	require.NoError(t, mnt.Mount("/", root))
	require.NoError(t, mnt.Mount("/docs", docs))
	require.NoError(t, mnt.Mount("/sessions", sessions))

	// duplicate mount conflicts
	err := mnt.Mount("/docs", memfs.New("dup"))
	assert.ErrorIs(t, err, domain.ErrConflict)

	// writes route to the longest-prefix backend
	require.NoError(t, mnt.Write(ctx, "/docs/intro.md", bytes.NewReader([]byte("hello docs")), 0o644))
	require.NoError(t, mnt.Write(ctx, "/sessions/s1.md", bytes.NewReader([]byte("hello sessions")), 0o644))
	require.NoError(t, mnt.Write(ctx, "/readme.md", bytes.NewReader([]byte("hello root")), 0o644))

	// reads come from the right backend
	var buf bytes.Buffer
	require.NoError(t, mnt.Read(ctx, "/docs/intro.md", &buf))
	assert.Equal(t, "hello docs", buf.String())

	buf.Reset()
	require.NoError(t, mnt.Read(ctx, "/sessions/s1.md", &buf))
	assert.Equal(t, "hello sessions", buf.String())

	buf.Reset()
	require.NoError(t, mnt.Read(ctx, "/readme.md", &buf))
	assert.Equal(t, "hello root", buf.String())
}

func TestMountableFSPrefixBoundary(t *testing.T) {
	// A mount at /docs must NOT match /docsessions/x (path-component rule).
	ctx := context.Background()
	mnt := ragfs.NewMountableFS()
	docs := memfs.New("docs")
	root := memfs.New("root")
	require.NoError(t, mnt.Mount("/", root))
	require.NoError(t, mnt.Mount("/docs", docs))

	// /docsessions/x should route to root, not docs
	require.NoError(t, mnt.Write(ctx, "/docsessions/x.md", bytes.NewReader([]byte("x")), 0o644))
	require.NoError(t, mnt.Write(ctx, "/docs/intro.md", bytes.NewReader([]byte("d")), 0o644))

	// docs backend should only have intro.md
	entries, err := docs.ReadDir(ctx, "/")
	require.NoError(t, err)
	names := namesOf(entries)
	assert.Contains(t, names, "intro.md")
	assert.NotContains(t, names, "docsessions")

	// root backend should have docsessions
	entries, err = root.ReadDir(ctx, "/")
	require.NoError(t, err)
	names = namesOf(entries)
	assert.Contains(t, names, "docsessions")
}

func TestMountableFSResolveMissing(t *testing.T) {
	mnt := ragfs.NewMountableFS()
	_, _, err := mnt.Resolve("/nope")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestMountableFSUnmount(t *testing.T) {
	mnt := ragfs.NewMountableFS()
	require.NoError(t, mnt.Mount("/x", memfs.New("x")))
	require.NoError(t, mnt.Unmount("/x"))
	// unmounting missing prefix
	err := mnt.Unmount("/x")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestMountableFSRenameCrossMount(t *testing.T) {
	ctx := context.Background()
	mnt := ragfs.NewMountableFS()
	require.NoError(t, mnt.Mount("/a", memfs.New("a")))
	require.NoError(t, mnt.Mount("/b", memfs.New("b")))
	require.NoError(t, mnt.Write(ctx, "/a/x.md", bytes.NewReader([]byte("x")), 0o644))
	err := mnt.Rename(ctx, "/a/x.md", "/b/y.md")
	assert.Error(t, err) // cross-mount rename forbidden
}

func namesOf(entries []*ragfs.TreeEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Info != nil {
			out = append(out, e.Info.Name)
		}
	}
	return out
}
