package ragfs

import (
	"bytes"
	"context"
	"io"
	"path"
	"sort"
	"strings"
)

// Hidden-file naming conventions for L0/L1/L2 layer sidecars.
const (
	// AbstractFile is the L0 ~100-token abstract sidecar.
	AbstractFile = ".abstract"
	// OverviewFile is the L1 ~2k-token overview sidecar.
	OverviewFile = ".overview"
	// ChunksDir is the L2 chunks directory sidecar.
	ChunksDir = ".chunks"
	// RedirectFile is the large-file redirect pointer sidecar.
	RedirectFile = ".redirect.json"
	// SyncLogFile is the multi-write consistency log sidecar.
	SyncLogFile = ".sync_log.json"
	// BackendMetaFile is the storage-shape guard manifest.
	BackendMetaFile = "backend_meta.json"
)

// ReadAbstract reads the L0 abstract sidecar for a resource. Returns ""
// (no error) when the sidecar is missing.
func ReadAbstract(ctx context.Context, fs FileSystem, p string) (string, error) {
	return readHiddenString(ctx, fs, HiddenSidecar(p, AbstractFile))
}

// WriteAbstract writes the L0 abstract sidecar for a resource.
func WriteAbstract(ctx context.Context, fs FileSystem, p, content string) error {
	return writeHiddenString(ctx, fs, HiddenSidecar(p, AbstractFile), content)
}

// ReadOverview reads the L1 overview sidecar for a resource. Returns ""
// (no error) when the sidecar is missing.
func ReadOverview(ctx context.Context, fs FileSystem, p string) (string, error) {
	return readHiddenString(ctx, fs, HiddenSidecar(p, OverviewFile))
}

// WriteOverview writes the L1 overview sidecar for a resource.
func WriteOverview(ctx context.Context, fs FileSystem, p, content string) error {
	return writeHiddenString(ctx, fs, HiddenSidecar(p, OverviewFile), content)
}

// WriteHidden writes an arbitrary hidden sidecar (e.g. ".abstract",
// ".overview") next to a resource path. Equivalent to the Rust
// fs.WriteHidden(uri, name, content) helper referenced in the design doc.
func WriteHidden(ctx context.Context, fs FileSystem, p, name, content string) error {
	return writeHiddenString(ctx, fs, HiddenSidecar(p, name), content)
}

// ReadHidden reads an arbitrary hidden sidecar next to a resource path.
// Returns "" (no error) when the sidecar is missing.
func ReadHidden(ctx context.Context, fs FileSystem, p, name string) (string, error) {
	return readHiddenString(ctx, fs, HiddenSidecar(p, name))
}

// ListChunks enumerates chunk files under the `.chunks/` sidecar directory
// of a resource. Returns an empty slice (no error) when the directory is
// missing. Chunk names are returned in lexicographic order so that
// `chunk_000`, `chunk_001`, ... are deterministically ordered.
func ListChunks(ctx context.Context, fs FileSystem, p string) ([]string, error) {
	dir := HiddenSidecar(p, ChunksDir)
	entries, err := fs.ReadDir(ctx, dir)
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Info == nil || e.Info.IsDir {
			continue
		}
		out = append(out, e.Info.Name)
	}
	sort.Strings(out)
	return out, nil
}

// ReadChunk reads a single chunk by name from the `.chunks/` sidecar.
// Returns "" (no error) when the chunk is missing.
func ReadChunk(ctx context.Context, fs FileSystem, p, chunkName string) (string, error) {
	dir := HiddenSidecar(p, ChunksDir)
	chunkPath := path.Join(dir, chunkName)
	return readHiddenString(ctx, fs, chunkPath)
}

// WriteChunk writes a single chunk by name into the `.chunks/` sidecar.
func WriteChunk(ctx context.Context, fs FileSystem, p, chunkName, content string) error {
	dir := HiddenSidecar(p, ChunksDir)
	if err := fs.Mkdir(ctx, dir, 0o755); err != nil && !IsConflict(err) {
		return err
	}
	chunkPath := path.Join(dir, chunkName)
	return writeHiddenString(ctx, fs, chunkPath, content)
}

// HasAbstract reports whether the L0 abstract sidecar exists.
func HasAbstract(ctx context.Context, fs FileSystem, p string) (bool, error) {
	return hasHidden(ctx, fs, HiddenSidecar(p, AbstractFile))
}

// HasOverview reports whether the L1 overview sidecar exists.
func HasOverview(ctx context.Context, fs FileSystem, p string) (bool, error) {
	return hasHidden(ctx, fs, HiddenSidecar(p, OverviewFile))
}

// readHiddenString reads a sidecar file as a string. Missing -> "".
func readHiddenString(ctx context.Context, fs FileSystem, p string) (string, error) {
	var buf bytes.Buffer
	if err := fs.Read(ctx, p, &buf); err != nil {
		if IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return buf.String(), nil
}

// writeHiddenString writes a sidecar file from a string.
func writeHiddenString(ctx context.Context, fs FileSystem, p, content string) error {
	return fs.Write(ctx, p, strings.NewReader(content), 0o644)
}

// hasHidden reports whether a sidecar file exists.
func hasHidden(ctx context.Context, fs FileSystem, p string) (bool, error) {
	if _, err := fs.Stat(ctx, p); err != nil {
		if IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Compile-time guard: io.Writer must be satisfied by *bytes.Buffer.
var _ io.Writer = (*bytes.Buffer)(nil)
