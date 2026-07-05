package parsers

import (
	"context"
	"os"
	"path/filepath"
	"sort"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// DirectoryParser parses a directory by delegating each child file to the
// parser bound to its extension. Children are flattened into a list of
// NodeFile entries; the caller can re-tree them as needed.
type DirectoryParser struct{}

func init() {
	parse.RegisterParser("directory", func() (parse.Parser, error) {
		return &DirectoryParser{}, nil
	})
}

// SupportedExtensions returns nil; the directory parser is routed by
// "directory" name only (the registry routes dirs explicitly).
func (p *DirectoryParser) SupportedExtensions() []string { return nil }

// Parse walks res.Path recursively and emits a NodeFile child per file.
func (p *DirectoryParser) Parse(ctx context.Context, res *parse.LocalResource, opts parse.ParserOptions) (*parse.ParseResult, error) {
	if res == nil || res.Path == "" {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422,
			"directory parser: nil or empty local resource")
	}
	info, err := os.Stat(res.Path)
	if err != nil {
		return nil, domain.Wrap(domain.CodeResourceNotFound, 404, err)
	}
	if !info.IsDir() {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422,
			"directory parser: not a directory: "+res.Path)
	}
	files := collectFiles(res.Path)
	return newDirResult(res.Path, "DirectoryParser", files), nil
}

// ParseContent is unsupported for the directory parser — directories
// cannot be represented by an in-memory byte slice.
func (p *DirectoryParser) ParseContent(ctx context.Context, content []byte, sourcePath string, opts parse.ParserOptions) (*parse.ParseResult, error) {
	return nil, domain.ErrUnsupported
}

// collectFiles walks root recursively and returns a NodeFile entry per
// file, sorted by path.
func collectFiles(root string) []*parse.ResourceNode {
	var out []*parse.ResourceNode
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, &parse.ResourceNode{
			Type:        parse.NodeFile,
			Title:       filepath.Base(path),
			ContentPath: path,
			Meta: map[string]any{
				"rel_path": rel,
				"size":     info.Size(),
				"mod_time": info.ModTime().UnixNano(),
			},
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i].Meta["rel_path"].(string) < out[j].Meta["rel_path"].(string)
	})
	return out
}
