// Package parsers implements the L2 Parser layer of the parse pipeline.
// Each parser converts a parse.LocalResource into a parse.ParseResult.
package parsers

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// readAll reads the entire content of res.Path into memory. Returns a
// wrapped domain error on failure.
func readAll(res *parse.LocalResource) ([]byte, error) {
	if res == nil || res.Path == "" {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422,
			"parser: nil or empty local resource")
	}
	b, err := os.ReadFile(res.Path)
	if err != nil {
		return nil, domain.Wrap(domain.CodeParseFailed, 422, err)
	}
	return b, nil
}

// newTextResult is a convenience constructor for a flat text ParseResult
// with a single root paragraph node. The content is written to a temp
// file and the node's ContentPath points to it so downstream consumers
// can stream large content without re-parsing.
func newTextResult(content, sourcePath, parserName, sourceFormat string) *parse.ParseResult {
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(sourcePath)}
	if cp, err := writeTempText(content); err == nil {
		root.AddChild(&parse.ResourceNode{
			Type:        parse.NodeParagraph,
			ContentPath: cp,
			Meta:        map[string]any{"text": content},
		})
	} else {
		root.AddChild(&parse.ResourceNode{
			Type: parse.NodeParagraph,
			Meta: map[string]any{"text": content},
		})
	}
	pr := parse.NewParseResult(root, sourcePath, sourceFormat, parserName)
	return pr
}

// titleFromPath returns the file base name (without extension) of path.
func titleFromPath(path string) string {
	if path == "" {
		return ""
	}
	base := path
	if idx := strings.LastIndexByte(base, '/'); idx >= 0 {
		base = base[idx+1:]
	}
	if idx := strings.LastIndexByte(base, '.'); idx > 0 {
		base = base[:idx]
	}
	return base
}

// newDirResult is a convenience constructor for a directory ParseResult
// with one NodeFile child per file in the tree.
func newDirResult(sourcePath, parserName string, files []*parse.ResourceNode) *parse.ParseResult {
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(sourcePath)}
	for _, f := range files {
		root.AddChild(f)
	}
	return parse.NewParseResult(root, sourcePath, "directory", parserName)
}

// writeTempText writes content to a fresh temp file and returns its
// path. Used by parsers that need a content_path for ResourceNode.
func writeTempText(content string) (string, error) {
	f, err := os.CreateTemp("", "openviking-parser-*.txt")
	if err != nil {
		return "", domain.Wrap(domain.CodeInternalError, 500, err)
	}
	defer f.Close()
	if _, err := io.Copy(f, bytes.NewReader([]byte(content))); err != nil {
		return "", domain.Wrap(domain.CodeInternalError, 500, err)
	}
	return f.Name(), nil
}

// _ = fmt.Sprintf is an import-keeper for parsers that use it in error
// messages.
var _ = fmt.Sprintf
