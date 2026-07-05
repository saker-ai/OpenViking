package ovpack

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rebuildZip writes a new zip with the same members as pack, applying fn
// to each member's (name, content). Returning newName == "" drops the
// member. Used by corruption tests to mutate member content or the
// manifest while preserving zip structure.
func rebuildZip(t *testing.T, pack []byte, fn func(name string, content []byte) (string, []byte)) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(pack), int64(len(pack)))
	require.NoError(t, err)
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(rc)
		rc.Close()
		require.NoError(t, err)
		newName, newData := fn(f.Name, data)
		if newName == "" {
			continue
		}
		w, err := zw.Create(newName)
		require.NoError(t, err)
		_, err = w.Write(newData)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return out.Bytes()
}

// writePack writes a deterministic pack to a buffer and returns it along
// with the expected manifest counts.
func writePack(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	w := NewWriter(buf)
	w.SetSource("acct_001", "viking://agent/")
	w.SetEmbedder("openai", "text-embedding-3-large", 1536)
	w.SetVLM("volcengine", "doubao-seed-2-0-lite-260428")
	w.SetRelationEdges(230)

	require.NoError(t, w.WriteResource("viking://agent/resources/docs/readme.md", []byte("hello readme")))
	require.NoError(t, w.WriteResource("viking://agent/resources/images/logo.png", []byte("\x89PNG fake bytes")))
	require.NoError(t, w.WriteLayer("viking://agent/resources/docs/readme.md", LayerAbstract, []byte("abstract of readme")))
	require.NoError(t, w.WriteLayer("viking://agent/resources/docs/readme.md", LayerOverview, []byte("overview of readme")))
	require.NoError(t, w.WriteVectors("chunks", "docs/readme.md.vec.jsonl", []byte("{\"v\":[1.0]}\n")))
	require.NoError(t, w.WriteVectors("abstract", "docs/readme.md.vec.jsonl", []byte("{\"v\":[2.0]}\n")))
	require.NoError(t, w.WriteSession("01HXYZ", []byte("{\"id\":\"01HXYZ\"}")))
	require.NoError(t, w.WriteMemory("01HABC", []byte("{\"id\":\"01HABC\"}")))
	require.NoError(t, w.WriteSkill("code-review", []byte("{\"name\":\"code-review\"}")))
	require.NoError(t, w.WriteRelations([]byte("{\"edges\":230}")))
	require.NoError(t, w.Close())
}

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	writePack(t, &buf)

	r, err := Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.NoError(t, err)

	m := r.Manifest()
	assert.Equal(t, Format, m.Format)
	assert.Equal(t, Version, m.Version)
	assert.Equal(t, "acct_001", m.Source.Account)
	assert.Equal(t, "viking://agent/", m.Source.BaseURI)
	assert.Equal(t, "openai", m.Embedder.Provider)
	assert.Equal(t, 1536, m.Embedder.Dim)
	assert.Equal(t, "volcengine", m.VLM.Provider)

	// Resources: 2 files written directly.
	assert.Equal(t, 2, m.Contents.Resources.Count)
	assert.Equal(t, int64(len("hello readme")+len("\x89PNG fake bytes")), m.Contents.Resources.Bytes)
	// Layers: one abstract + one overview.
	assert.Equal(t, 1, m.Contents.Layers.Abstract)
	assert.Equal(t, 1, m.Contents.Layers.Overview)
	// Vectors: one chunks dump + one abstract dump.
	assert.Equal(t, 1, m.Contents.Vectors.Chunks)
	assert.Equal(t, 1, m.Contents.Vectors.Abstract)
	assert.Equal(t, 1536, m.Contents.Vectors.Dim)
	// Sessions / memory / skills.
	assert.Equal(t, 1, m.Contents.Sessions.Count)
	assert.Equal(t, 1, m.Contents.Memory.Count)
	assert.Equal(t, 1, m.Contents.Skills.Count)
	// Relations.
	assert.Equal(t, 230, m.Contents.Relations.Edges)

	// Checksums verify cleanly.
	require.NoError(t, r.Verify())

	// Spot-check content round-trips.
	got, err := r.ReadResource("viking://agent/resources/docs/readme.md")
	require.NoError(t, err)
	assert.Equal(t, "hello readme", string(got))

	got, err = r.ReadLayer("viking://agent/resources/docs/readme.md", LayerAbstract)
	require.NoError(t, err)
	assert.Equal(t, "abstract of readme", string(got))

	got, err = r.ReadVectors("chunks", "docs/readme.md.vec.jsonl")
	require.NoError(t, err)
	assert.Equal(t, "{\"v\":[1.0]}\n", string(got))

	got, err = r.ReadSession("01HXYZ")
	require.NoError(t, err)
	assert.Equal(t, "{\"id\":\"01HXYZ\"}", string(got))

	got, err = r.ReadMemory("01HABC")
	require.NoError(t, err)
	assert.Equal(t, "{\"id\":\"01HABC\"}", string(got))

	got, err = r.ReadSkill("code-review")
	require.NoError(t, err)
	assert.Equal(t, "{\"name\":\"code-review\"}", string(got))

	got, err = r.ReadRelations()
	require.NoError(t, err)
	assert.Equal(t, "{\"edges\":230}", string(got))
}

func TestVerifyChecksumOK(t *testing.T) {
	var buf bytes.Buffer
	writePack(t, &buf)
	r, err := Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.NoError(t, err)
	assert.NoError(t, r.Verify())
}

func TestChecksumFailureCorruptByte(t *testing.T) {
	var buf bytes.Buffer
	writePack(t, &buf)

	// Rebuild the zip with one member's content changed. The archive
	// hash recomputed by Verify will not match the manifest's stored
	// archive hash.
	corrupted := rebuildZip(t, buf.Bytes(), func(name string, content []byte) (string, []byte) {
		if name == "resources/docs/readme.md" {
			return name, []byte("JELLO readme")
		}
		return name, content
	})

	r, err := Open(bytes.NewReader(corrupted), int64(len(corrupted)))
	require.NoError(t, err)
	err = r.Verify()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrChecksumMismatch), "want ErrChecksumMismatch, got %v", err)
}

func TestChecksumFailureManifestCorrupt(t *testing.T) {
	var buf bytes.Buffer
	writePack(t, &buf)

	// Mutate the manifest's archive checksum field. The manifest hash
	// recomputed by verifyManifest (with the modified archive field and
	// the manifest field zeroed) will not match the stored manifest hash.
	corrupted := rebuildZip(t, buf.Bytes(), func(name string, content []byte) (string, []byte) {
		if name == "manifest.json" {
			var m Manifest
			require.NoError(t, json.Unmarshal(content, &m))
			// Replace the archive hex with a known-wrong value.
			m.Checksum.Archive = "0000000000000000000000000000000000000000000000000000000000000000"
			newData, err := marshalManifest(&m)
			require.NoError(t, err)
			return name, newData
		}
		return name, content
	})

	r, err := Open(bytes.NewReader(corrupted), int64(len(corrupted)))
	require.NoError(t, err)
	err = r.Verify()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrChecksumMismatch), "want ErrChecksumMismatch, got %v", err)
}

func TestEmptyPackManifestOnly(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetSource("acct_001", "viking://agent/")
	require.NoError(t, w.Close())

	r, err := Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.NoError(t, err)
	m := r.Manifest()
	assert.Equal(t, Format, m.Format)
	assert.Equal(t, Version, m.Version)
	assert.Equal(t, 0, m.Contents.Resources.Count)
	assert.Equal(t, int64(0), m.Contents.Resources.Bytes)
	assert.Equal(t, 0, m.Contents.Sessions.Count)
	assert.Equal(t, "sha256", m.Checksum.Algorithm)
	assert.NotEmpty(t, m.Checksum.Manifest)
	// Archive hash of empty content is the SHA-256 of the empty string.
	assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", m.Checksum.Archive)
	require.NoError(t, r.Verify())

	// No member files except manifest.
	files := r.ListFiles()
	assert.ElementsMatch(t, []string{"manifest.json"}, files)
}

func TestLargeFileRedirect(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetSource("acct_001", "viking://agent/")
	rd := Redirect{
		URL:  "s3://bucket/large.bin",
		Size: 1 << 30,
		Hash: "abc123",
	}
	require.NoError(t, w.WriteResourceRedirect("viking://agent/resources/large.bin", rd))
	// A real file alongside the redirect for a different URI.
	require.NoError(t, w.WriteResource("viking://agent/resources/small.txt", []byte("small")))
	require.NoError(t, w.Close())

	r, err := Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.NoError(t, err)

	// small.txt reads normally.
	got, err := r.ReadResource("viking://agent/resources/small.txt")
	require.NoError(t, err)
	assert.Equal(t, "small", string(got))

	// large.bin is a redirect: ReadResource errors with ErrRedirect.
	_, err = r.ReadResource("viking://agent/resources/large.bin")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRedirect), "want ErrRedirect, got %v", err)

	// ReadResourceRedirect returns the stored redirect.
	gotRd, err := r.ReadResourceRedirect("viking://agent/resources/large.bin")
	require.NoError(t, err)
	assert.Equal(t, "s3://bucket/large.bin", gotRd.URL)
	assert.Equal(t, int64(1<<30), gotRd.Size)
	assert.Equal(t, "abc123", gotRd.Hash)

	// IsRedirect reports true only for the redirect URI.
	assert.True(t, r.IsRedirect("viking://agent/resources/large.bin"))
	assert.False(t, r.IsRedirect("viking://agent/resources/small.txt"))

	// Counters: redirect counts as a resource but contributes no bytes.
	m := r.Manifest()
	assert.Equal(t, 2, m.Contents.Resources.Count)
	assert.Equal(t, int64(len("small")), m.Contents.Resources.Bytes)

	// Checksums still verify.
	require.NoError(t, r.Verify())
}

func TestReadFromStreaming(t *testing.T) {
	var buf bytes.Buffer
	writePack(t, &buf)

	// ReadFrom accepts a forward-only io.Reader.
	r, err := ReadFrom(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.NoError(t, r.Verify())
	got, err := r.ReadResource("viking://agent/resources/docs/readme.md")
	require.NoError(t, err)
	assert.Equal(t, "hello readme", string(got))
}

func TestWriteAfterClose(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	require.NoError(t, w.Close())
	err := w.WriteResource("viking://agent/resources/x.md", []byte("x"))
	require.Error(t, err)
}

func TestReadNotFound(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	require.NoError(t, w.WriteResource("viking://agent/resources/exists.md", []byte("y")))
	require.NoError(t, w.Close())

	r, err := Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.NoError(t, err)
	_, err = r.ReadResource("viking://agent/resources/missing.md")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound), "want ErrNotFound, got %v", err)
}

func TestExportedAtIsUTC(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	require.NoError(t, w.Close())
	r, err := Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.NoError(t, err)
	// Within a few seconds of now-UTC.
	now := time.Now().UTC()
	delta := now.Sub(r.Manifest().ExportedAt)
	assert.LessOrEqual(t, delta, 5*time.Second)
	assert.GreaterOrEqual(t, delta, -5*time.Second)
	assert.Equal(t, "UTC", r.Manifest().ExportedAt.Location().String())
}

// TestWriterSetEmbedderUpdatesDim verifies SetEmbedder also seeds the
// vectors dim counter so an empty pack still reports the embedder dim.
func TestWriterSetEmbedderUpdatesDim(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetEmbedder("openai", "text-embedding-3-large", 3072)
	require.NoError(t, w.Close())
	r, err := Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.NoError(t, err)
	assert.Equal(t, 3072, r.Manifest().Embedder.Dim)
	assert.Equal(t, 3072, r.Manifest().Contents.Vectors.Dim)
}

// TestOpenBadZip verifies Open surfaces a zip-parse error.
func TestOpenBadZip(t *testing.T) {
	_, err := Open(bytes.NewReader([]byte("not a zip")), 9)
	require.Error(t, err)
}

// TestOpenMissingManifest verifies Open rejects a zip without manifest.
func TestOpenMissingManifest(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("resources/nope.md")
	require.NoError(t, err)
	_, err = f.Write([]byte("nope"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	_, err = Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "manifest.json not found")
}

// TestOpenBadFormat verifies Open rejects a manifest with the wrong
// format identifier.
func TestOpenBadFormat(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("manifest.json")
	require.NoError(t, err)
	_, err = f.Write([]byte(`{"format":"not-ovpack","version":"1.0","checksum":{"algorithm":"sha256"}}`))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	_, err = Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported format")
}
