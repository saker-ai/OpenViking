// Package accessors implements the L1 DataAccessor layer of the parse
// pipeline. Each accessor fetches a remote or special-path source into a
// local file or directory, returning a parse.LocalResource for the L2
// parser layer to consume.
package accessors

import (
	"context"
	"os"
	"path/filepath"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// LocalAccessor handles plain local filesystem paths. It is the default
// accessor when source has no URI scheme.
type LocalAccessor struct{}

func init() {
	parse.RegisterAccessor("local", func() (parse.DataAccessor, error) {
		return &LocalAccessor{}, nil
	})
}

// CanHandle reports whether source is a plain local path (no scheme).
func (a *LocalAccessor) CanHandle(source string, opts parse.AccessorOptions) bool {
	return parse.SchemeOf(source) == "local"
}

// Fetch stat's the path and returns a non-temporary LocalResource. The
// caller must NOT call Cleanup on the returned resource.
func (a *LocalAccessor) Fetch(ctx context.Context, source string, opts parse.AccessorOptions) (*parse.LocalResource, error) {
	path := source
	if parse.SchemeOf(source) == "local" && len(source) > 0 && source[0] != '/' {
		// Treat as relative to cwd.
		abs, err := filepath.Abs(source)
		if err == nil {
			path = abs
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, domain.Wrap(domain.CodeResourceNotFound, 404, err)
	}
	return &parse.LocalResource{
		Path:           path,
		SourceType:     parse.SourceLocal,
		OriginalSource: source,
		IsTemporary:    false,
		Meta: map[string]any{
			"size":  info.Size(),
			"isDir": info.IsDir(),
		},
	}, nil
}

// Schemes returns the URI schemes handled (none — LocalAccessor is
// routed by the registry's "local" / "" entry).
func (a *LocalAccessor) Schemes() []string { return []string{"local", ""} }
