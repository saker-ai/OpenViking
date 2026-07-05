package ragfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"time"
)

// RedirectType labels why a resource was redirected to a pointer file.
type RedirectType string

const (
	// RedirectFileOverSize: write exceeded RedirectPolicy.FileOverSize.
	RedirectFileOverSize RedirectType = "file_over_size"
	// RedirectFileExtension: write matched RedirectPolicy.FileExtensions.
	RedirectFileExtension RedirectType = "file_extension"
)

// Redirect is the in-memory representation of a `.redirect.json` sidecar.
//
// When a write exceeds the configured size threshold, MultiWriteFS stores
// the actual bytes at a separate target URI (typically a large-object
// store) and writes this pointer next to the resource path instead. Reads
// follow the pointer transparently.
type Redirect struct {
	Type      RedirectType `json:"type"`
	Target    string       `json:"target"`
	Size      int64        `json:"size,omitempty"`
	Hash      string       `json:"hash,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
}

// RedirectPolicy decides which writes should be redirected to a pointer.
//
// FileOverSize is the primary trigger (mirrors
// ragfs.redirect.file_over_size from ov.conf). FileExtensions is an
// optional allow-list for extension-based redirect (e.g. [".mp4"]).
type RedirectPolicy struct {
	FileOverSize   int64
	FileExtensions []string
}

// ShouldRedirect reports whether a write of size `size` to path `p` should
// produce a redirect pointer instead of inlining the bytes.
//
// A size of -1 (unknown) is treated as "do not redirect by size"; callers
// streaming unbounded readers should pre-compute the size if they want
// size-based redirect to apply.
func (p RedirectPolicy) ShouldRedirect(size int64, pth string) bool {
	if p.FileOverSize > 0 && size > 0 && size > p.FileOverSize {
		return true
	}
	if len(p.FileExtensions) > 0 {
		ext := strings.ToLower(path.Ext(pth))
		for _, e := range p.FileExtensions {
			if strings.ToLower(e) == ext {
				return true
			}
		}
	}
	return false
}

// redirectSidecar returns the path of the `.redirect.json` sidecar for a
// resource path.
func redirectSidecar(resourcePath string) string {
	return HiddenSidecar(resourcePath, ".redirect.json")
}

// LoadRedirect reads and decodes the `.redirect.json` sidecar for a path.
// Returns a nil pointer and no error when the sidecar does not exist.
func LoadRedirect(ctx context.Context, fs FileSystem, p string) (*Redirect, error) {
	sidecar := redirectSidecar(p)
	var buf bytes.Buffer
	if err := fs.Read(ctx, sidecar, &buf); err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var r Redirect
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		return nil, wrapRAGFS(err)
	}
	return &r, nil
}

// WriteRedirect writes a `.redirect.json` sidecar for a path.
func WriteRedirect(ctx context.Context, fs FileSystem, p string, r *Redirect) error {
	if r == nil {
		return wrapRAGFS(errors.New("redirect pointer is nil"))
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return wrapRAGFS(err)
	}
	sidecar := redirectSidecar(p)
	return fs.Write(ctx, sidecar, bytes.NewReader(data), 0o644)
}

// RemoveRedirect removes the `.redirect.json` sidecar for a path.
// Missing sidecar is not an error.
func RemoveRedirect(ctx context.Context, fs FileSystem, p string) error {
	sidecar := redirectSidecar(p)
	if err := fs.Remove(ctx, sidecar, false); err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

// FollowRedirect resolves the target URI of a redirect pointer. If no
// sidecar exists, it returns the original path unchanged.
func FollowRedirect(ctx context.Context, fs FileSystem, p string) (string, error) {
	r, err := LoadRedirect(ctx, fs, p)
	if err != nil {
		return p, err
	}
	if r == nil {
		return p, nil
	}
	return r.Target, nil
}

// FollowRedirectTarget returns the redirect target if present, else "".
// Useful for readers that want to short-circuit to the large-object store.
func FollowRedirectTarget(ctx context.Context, fs FileSystem, p string) (string, bool, error) {
	r, err := LoadRedirect(ctx, fs, p)
	if err != nil {
		return "", false, err
	}
	if r == nil {
		return "", false, nil
	}
	return r.Target, true, nil
}
