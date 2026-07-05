package ovpack

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"strings"
	"time"
)

// Writer builds an ovpack ZIP archive. File contents are streamed to the
// underlying io.Writer; the manifest is finalized and written on Close.
//
// Writer counts each WriteXxx call into the manifest Contents and feeds
// every byte through the archive SHA-256 digest so Close can emit a
// verified checksum without re-reading the archive.
type Writer struct {
	zw          *zip.Writer
	manifest    Manifest
	archiveHash hash.Hash // sha256 digest over member file contents
	closed      bool
}

// NewWriter returns a Writer that streams the pack to w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{
		zw:          zip.NewWriter(w),
		archiveHash: sha256.New(),
		manifest: Manifest{
			Format:     Format,
			Version:    Version,
			ExportedAt: time.Now().UTC(),
			Exporter:   "openviking-go",
			Contents: Contents{
				Resources: ResourceStats{},
			},
			Checksum: Checksum{Algorithm: Algorithm},
		},
	}
}

// SetSource records the exporting account and base viking:// URI.
func (w *Writer) SetSource(account, baseURI string) {
	w.manifest.Source = Source{Account: account, BaseURI: baseURI}
}

// SetEmbedder records the embedding model used for vectors in this pack.
func (w *Writer) SetEmbedder(provider, model string, dim int) {
	w.manifest.Embedder = EmbedderInfo{Provider: provider, Model: model, Dim: dim}
	if w.manifest.Contents.Vectors.Dim == 0 {
		w.manifest.Contents.Vectors.Dim = dim
	}
}

// SetVLM records the vision-language model used for L0/L1 layers.
func (w *Writer) SetVLM(provider, model string) {
	w.manifest.VLM = VLMInfo{Provider: provider, Model: model}
}

// WriteResource adds an original resource to the pack.
func (w *Writer) WriteResource(uri string, content []byte) error {
	p, err := uriToResourcePath(uri)
	if err != nil {
		return err
	}
	if err := w.writeFile(p, content); err != nil {
		return err
	}
	w.manifest.Contents.Resources.Count++
	w.manifest.Contents.Resources.Bytes += int64(len(content))
	return nil
}

// WriteResourceRedirect adds a redirect entry for a large resource whose
// content lives outside the pack. The redirect counts toward
// Resources.Count but not Resources.Bytes.
func (w *Writer) WriteResourceRedirect(uri string, redirect Redirect) error {
	p, err := uriToResourcePath(uri)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(redirect, "", "  ")
	if err != nil {
		return err
	}
	if err := w.writeFile(redirectPath(p), data); err != nil {
		return err
	}
	w.manifest.Contents.Resources.Count++
	return nil
}

// WriteLayer adds a derived hidden file (abstract / overview / fallback_l1).
func (w *Writer) WriteLayer(uri, layer string, content []byte) error {
	p, err := uriToLayerPath(uri, layer)
	if err != nil {
		return err
	}
	if err := w.writeFile(p, content); err != nil {
		return err
	}
	switch layer {
	case LayerAbstract:
		w.manifest.Contents.Layers.Abstract++
	case LayerOverview:
		w.manifest.Contents.Layers.Overview++
	case LayerFallbackL1:
		w.manifest.Contents.Layers.FallbackL1++
	}
	return nil
}

// WriteVectors adds a vector dump for a collection (chunks / abstract /
// overview). The name carries the per-resource filename suffix.
func (w *Writer) WriteVectors(collection, name string, content []byte) error {
	p := vectorsPath(collection, name)
	if err := w.writeFile(p, content); err != nil {
		return err
	}
	switch collection {
	case "chunks":
		w.manifest.Contents.Vectors.Chunks++
	case "abstract":
		w.manifest.Contents.Vectors.Abstract++
	case "overview":
		w.manifest.Contents.Vectors.Overview++
	}
	return nil
}

// WriteSession adds a session JSON snapshot.
func (w *Writer) WriteSession(id string, content []byte) error {
	if err := w.writeFile(sessionPath(id), content); err != nil {
		return err
	}
	w.manifest.Contents.Sessions.Count++
	return nil
}

// WriteMemory adds an extracted memory item JSON.
func (w *Writer) WriteMemory(id string, content []byte) error {
	if err := w.writeFile(memoryPath(id), content); err != nil {
		return err
	}
	w.manifest.Contents.Memory.Count++
	return nil
}

// WriteSkill adds a skill JSON definition.
func (w *Writer) WriteSkill(name string, content []byte) error {
	if err := w.writeFile(skillPath(name), content); err != nil {
		return err
	}
	w.manifest.Contents.Skills.Count++
	return nil
}

// WriteRelations adds the relation graph JSON.
func (w *Writer) WriteRelations(content []byte) error {
	if err := w.writeFile("relations/graph.json", content); err != nil {
		return err
	}
	// Edges is left for the caller to set via SetRelationEdges; the
	// pack does not parse the graph to count edges.
	return nil
}

// SetRelationEdges records the edge count for the relations graph.
func (w *Writer) SetRelationEdges(edges int) {
	w.manifest.Contents.Relations.Edges = edges
}

// WriteFile writes a member file at an arbitrary pack-relative path and
// infers the manifest counters from the path prefix. It is intended for
// the ovpack CLI's `pack` subcommand, which walks a directory and copies
// files into the pack preserving their relative paths. Programmatic
// callers should prefer the URI-based methods (WriteResource etc.).
func (w *Writer) WriteFile(p string, content []byte) error {
	if err := w.writeFile(p, content); err != nil {
		return err
	}
	w.inferCounts(p, content)
	return nil
}

// inferCounts updates manifest counters based on the pack path prefix.
func (w *Writer) inferCounts(p string, content []byte) {
	switch {
	case p == "manifest.json":
		// never counted; manifest is generated by Close
	case p == "relations/graph.json":
		// edge count is left to SetRelationEdges; presence alone is
		// not enough to derive edges.
	case strings.HasPrefix(p, "resources/"):
		if strings.HasSuffix(p, ".redirect.json") {
			w.manifest.Contents.Resources.Count++
			return
		}
		w.manifest.Contents.Resources.Count++
		w.manifest.Contents.Resources.Bytes += int64(len(content))
	case strings.HasPrefix(p, "layers/"):
		switch {
		case strings.HasSuffix(p, "."+LayerAbstract):
			w.manifest.Contents.Layers.Abstract++
		case strings.HasSuffix(p, "."+LayerOverview):
			w.manifest.Contents.Layers.Overview++
		case strings.HasSuffix(p, "."+LayerFallbackL1):
			w.manifest.Contents.Layers.FallbackL1++
		}
	case strings.HasPrefix(p, "vectors/"):
		// vectors/<collection>/<name>
		rest := strings.TrimPrefix(p, "vectors/")
		idx := strings.IndexByte(rest, '/')
		if idx < 0 {
			return
		}
		collection := rest[:idx]
		switch collection {
		case "chunks":
			w.manifest.Contents.Vectors.Chunks++
		case "abstract":
			w.manifest.Contents.Vectors.Abstract++
		case "overview":
			w.manifest.Contents.Vectors.Overview++
		}
	case strings.HasPrefix(p, "sessions/"):
		w.manifest.Contents.Sessions.Count++
	case strings.HasPrefix(p, "memory/"):
		w.manifest.Contents.Memory.Count++
	case strings.HasPrefix(p, "skills/"):
		w.manifest.Contents.Skills.Count++
	}
}

// Close finalizes the manifest (with checksums) and the zip archive.
// After Close the Writer is unusable.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true

	// Finalize archive checksum.
	w.manifest.Checksum.Archive = hex.EncodeToString(w.archiveHash.Sum(nil))

	// Manifest checksum is computed over the manifest JSON with the
	// manifest checksum field zeroed, then filled in.
	w.manifest.Checksum.Manifest = ""
	pre, err := marshalManifest(&w.manifest)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(pre)
	w.manifest.Checksum.Manifest = hex.EncodeToString(sum[:])

	final, err := marshalManifest(&w.manifest)
	if err != nil {
		return err
	}
	if err := w.writeRaw("manifest.json", final); err != nil {
		return err
	}
	return w.zw.Close()
}

// writeFile writes a single member file and feeds its content through the
// archive hash digest.
func (w *Writer) writeFile(p string, content []byte) error {
	if w.closed {
		return errClosed
	}
	if _, err := w.archiveHash.Write(content); err != nil {
		return err
	}
	return w.writeRaw(p, content)
}

// writeRaw writes a member file without touching the archive hash (used
// for manifest.json itself).
func (w *Writer) writeRaw(p string, content []byte) error {
	f, err := w.zw.Create(p)
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	return err
}

// marshalManifest serializes m with stable formatting shared by Close
// (write) and Open (read) so the manifest checksum round-trips.
func marshalManifest(m *Manifest) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	// json.Encoder appends a trailing newline; drop it for hash
	// stability and for parity with the manifest bytes readers see.
	out := strings.TrimRight(b.String(), "\n")
	return []byte(out), nil
}

// errClosed is returned when a write method is called after Close.
var errClosed = errors.New("ovpack: writer closed")
