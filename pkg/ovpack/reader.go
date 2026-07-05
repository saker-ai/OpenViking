package ovpack

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Reader reads an ovpack ZIP archive.
//
// Open requires an io.ReaderAt and the archive size (zip needs random
// access to the central directory). ReadFrom is a streaming convenience
// that buffers the entire pack in memory; for large packs prefer Open
// with a *os.File or *bytes.Reader.
type Reader struct {
	zr       *zip.Reader
	manifest Manifest
	byPath   map[string]*zip.File
}

// Open returns a Reader over a seekable archive.
func Open(r io.ReaderAt, size int64) (*Reader, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("ovpack: open zip: %w", err)
	}
	return newReader(zr)
}

// ReadFrom buffers the entire pack from r and returns a Reader. Useful
// when only a forward-only io.Reader is available (e.g. a pipe).
func ReadFrom(r io.Reader) (*Reader, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("ovpack: read pack: %w", err)
	}
	return Open(bytes.NewReader(data), int64(len(data)))
}

func newReader(zr *zip.Reader) (*Reader, error) {
	rd := &Reader{
		zr:     zr,
		byPath: make(map[string]*zip.File, len(zr.File)),
	}
	for _, f := range zr.File {
		rd.byPath[f.Name] = f
	}
	mf, ok := rd.byPath["manifest.json"]
	if !ok {
		return nil, errors.New("ovpack: manifest.json not found")
	}
	rc, err := mf.Open()
	if err != nil {
		return nil, fmt.Errorf("ovpack: open manifest: %w", err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, fmt.Errorf("ovpack: read manifest: %w", err)
	}
	if err := json.Unmarshal(data, &rd.manifest); err != nil {
		return nil, fmt.Errorf("ovpack: parse manifest: %w", err)
	}
	if rd.manifest.Format != Format {
		return nil, fmt.Errorf("ovpack: unsupported format %q", rd.manifest.Format)
	}
	return rd, nil
}

// Manifest returns the parsed pack manifest.
func (r *Reader) Manifest() Manifest { return r.manifest }

// Verify recomputes the archive and manifest checksums and returns an
// error wrapping ErrChecksumMismatch if either does not match.
func (r *Reader) Verify() error {
	h := sha256.New()
	for _, f := range r.zr.File {
		if f.Name == "manifest.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("ovpack: open %q: %w", f.Name, err)
		}
		_, err = io.Copy(h, rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("ovpack: read %q: %w", f.Name, err)
		}
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != r.manifest.Checksum.Archive {
		return fmt.Errorf("%w: archive", ErrChecksumMismatch)
	}
	return r.verifyManifest()
}

// verifyManifest recomputes the manifest checksum over a re-marshalled
// manifest with the manifest field zeroed.
func (r *Reader) verifyManifest() error {
	saved := r.manifest.Checksum.Manifest
	m := r.manifest
	m.Checksum.Manifest = ""
	pre, err := marshalManifest(&m)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(pre)
	got := hex.EncodeToString(sum[:])
	if got != saved {
		return fmt.Errorf("%w: manifest", ErrChecksumMismatch)
	}
	return nil
}

// ListFiles returns the paths of all member files (including manifest.json).
func (r *Reader) ListFiles() []string {
	out := make([]string, 0, len(r.zr.File))
	for _, f := range r.zr.File {
		out = append(out, f.Name)
	}
	return out
}

// ListResources returns the pack-relative paths of all resources in the
// pack, excluding redirect entries.
func (r *Reader) ListResources() []string {
	var out []string
	for name := range r.byPath {
		if !strings.HasPrefix(name, "resources/") {
			continue
		}
		if strings.HasSuffix(name, ".redirect.json") {
			continue
		}
		out = append(out, name)
	}
	return out
}

// ReadResource returns the content for uri. If the pack only has a
// redirect entry for uri, ReadResource returns ErrRedirect.
func (r *Reader) ReadResource(uri string) ([]byte, error) {
	p, err := uriToResourcePath(uri)
	if err != nil {
		return nil, err
	}
	if f, ok := r.byPath[p]; ok {
		return readZipFile(f)
	}
	if _, ok := r.byPath[redirectPath(p)]; ok {
		return nil, fmt.Errorf("%w: %s", ErrRedirect, uri)
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, uri)
}

// ReadResourceRedirect returns the redirect entry for uri. Returns
// ErrNotFound if the resource has no redirect entry.
func (r *Reader) ReadResourceRedirect(uri string) (*Redirect, error) {
	p, err := uriToResourcePath(uri)
	if err != nil {
		return nil, err
	}
	f, ok := r.byPath[redirectPath(p)]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, uri)
	}
	data, err := readZipFile(f)
	if err != nil {
		return nil, err
	}
	var rd Redirect
	if err := json.Unmarshal(data, &rd); err != nil {
		return nil, fmt.Errorf("ovpack: parse redirect for %s: %w", uri, err)
	}
	return &rd, nil
}

// IsRedirect reports whether uri has a redirect entry in the pack.
func (r *Reader) IsRedirect(uri string) bool {
	p, err := uriToResourcePath(uri)
	if err != nil {
		return false
	}
	_, ok := r.byPath[redirectPath(p)]
	return ok
}

// ReadLayer returns the derived layer file for uri.
func (r *Reader) ReadLayer(uri, layer string) ([]byte, error) {
	p, err := uriToLayerPath(uri, layer)
	if err != nil {
		return nil, err
	}
	f, ok := r.byPath[p]
	if !ok {
		return nil, fmt.Errorf("%w: %s layer %s", ErrNotFound, uri, layer)
	}
	return readZipFile(f)
}

// ReadVectors returns the vector dump for the given collection and name.
func (r *Reader) ReadVectors(collection, name string) ([]byte, error) {
	p := vectorsPath(collection, name)
	f, ok := r.byPath[p]
	if !ok {
		return nil, fmt.Errorf("%w: vectors/%s/%s", ErrNotFound, collection, name)
	}
	return readZipFile(f)
}

// ReadSession returns the session JSON for id.
func (r *Reader) ReadSession(id string) ([]byte, error) {
	f, ok := r.byPath[sessionPath(id)]
	if !ok {
		return nil, fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	return readZipFile(f)
}

// ReadMemory returns the memory item JSON for id.
func (r *Reader) ReadMemory(id string) ([]byte, error) {
	f, ok := r.byPath[memoryPath(id)]
	if !ok {
		return nil, fmt.Errorf("%w: memory %s", ErrNotFound, id)
	}
	return readZipFile(f)
}

// ReadSkill returns the skill JSON for name.
func (r *Reader) ReadSkill(name string) ([]byte, error) {
	f, ok := r.byPath[skillPath(name)]
	if !ok {
		return nil, fmt.Errorf("%w: skill %s", ErrNotFound, name)
	}
	return readZipFile(f)
}

// ReadRelations returns the relation graph JSON.
func (r *Reader) ReadRelations() ([]byte, error) {
	f, ok := r.byPath["relations/graph.json"]
	if !ok {
		return nil, fmt.Errorf("%w: relations/graph.json", ErrNotFound)
	}
	return readZipFile(f)
}

// ExtractTo writes every member file (except manifest.json) to dir,
// preserving the relative pack path. Existing files are overwritten.
func (r *Reader) ExtractTo(dir string) error {
	return extractZipTo(r.zr, dir)
}

// readZipFile reads the entire content of a zip.File.
func readZipFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// extractZipTo extracts all zip members under dir. The manifest.json is
// also extracted so unpacked directories round-trip through the pack
// command unchanged.
func extractZipTo(zr *zip.Reader, dir string) error {
	fs := newFSWriter(dir)
	for _, f := range zr.File {
		if err := fs.write(f); err != nil {
			return err
		}
	}
	return fs.close()
}
