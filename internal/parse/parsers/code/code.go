// Package code implements the L2 parser for source code files using
// tree-sitter. One CodeParser instance per supported language; each
// language binding is in its own file (python.go, javascript.go, ...).
package code

import (
	"context"
	"fmt"
	"os"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// CodeParser parses source files in a single tree-sitter language. It
// emits one NodeCode per top-level definition (function / class / method
// / struct), with the source text in Meta.text.
type CodeParser struct {
	Name string
	Ext  string
	Lang *sitter.Language
}

// register is a convenience used by per-language init() functions.
func (p *CodeParser) register() {
	if p.Name == "" || p.Ext == "" || p.Lang == nil {
		panic("code: incomplete CodeParser registration")
	}
	parse.RegisterParser(p.Name, func() (parse.Parser, error) { return p, nil })
	parse.RegisterExtension(p.Ext, p.Name)
}

// SupportedExtensions returns the single extension bound to this parser.
func (p *CodeParser) SupportedExtensions() []string { return []string{p.Ext} }

// Parse parses the local source file with tree-sitter.
func (p *CodeParser) Parse(ctx context.Context, res *parse.LocalResource, opts parse.ParserOptions) (*parse.ParseResult, error) {
	if res == nil || res.Path == "" {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422,
			"code parser: nil or empty local resource")
	}
	b, err := os.ReadFile(res.Path)
	if err != nil {
		return nil, domain.Wrap(domain.CodeParseFailed, 422, err)
	}
	return p.parseBytes(b, res.Path)
}

// ParseContent parses an in-memory source blob.
func (p *CodeParser) ParseContent(ctx context.Context, content []byte, sourcePath string, opts parse.ParserOptions) (*parse.ParseResult, error) {
	return p.parseBytes(content, sourcePath)
}

// parseBytes parses src with tree-sitter and emits one NodeCode per
// top-level definition.
func (p *CodeParser) parseBytes(src []byte, sourcePath string) (*parse.ParseResult, error) {
	root, err := sitter.ParseCtx(context.Background(), src, p.Lang)
	if err != nil {
		return nil, domain.Wrap(domain.CodeParseFailed, 422, err)
	}
	if root == nil {
		return nil, domain.Wrap(domain.CodeParseFailed, 422,
			fmt.Errorf("code: parse returned nil root for %s", sourcePath))
	}
	out := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(sourcePath)}
	for i := 0; i < int(root.NamedChildCount()); i++ {
		child := root.NamedChild(i)
		if child == nil {
			continue
		}
		text := child.Content(src)
		if strings.TrimSpace(text) == "" {
			continue
		}
		// Write the source slice to a temp file so downstream consumers
		// can stream it.
		cp, _ := writeTempText(text)
		out.AddChild(&parse.ResourceNode{
			Type:        parse.NodeCode,
			Title:       child.Type(),
			ContentPath: cp,
			Meta: map[string]any{
				"text":      text,
				"node_type": child.Type(),
				"lang":      p.Name,
			},
		})
	}
	pr := parse.NewParseResult(out, sourcePath, p.Name, "CodeParser")
	pr.Meta = map[string]any{"language": p.Name}
	return pr, nil
}

// writeTempText is duplicated from parsers/base.go to keep the code
// package self-contained (it cannot import the internal parsers package
// without a cycle).
func writeTempText(content string) (string, error) {
	f, err := os.CreateTemp("", "openviking-code-*.txt")
	if err != nil {
		return "", domain.Wrap(domain.CodeInternalError, 500, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return "", domain.Wrap(domain.CodeInternalError, 500, err)
	}
	return f.Name(), nil
}

// titleFromPath returns the file base name (without extension).
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
