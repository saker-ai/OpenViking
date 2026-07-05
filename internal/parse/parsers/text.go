package parsers

import (
	"context"
	"strings"

	"github.com/saker-ai/ctxhub/internal/parse"
)

// TextParser parses .txt / .log / .text files using the standard library
// only. It splits the content into paragraphs on blank-line boundaries so
// downstream consumers can chunk on natural breaks.
type TextParser struct{}

func init() {
	parse.RegisterParser("text", func() (parse.Parser, error) { return &TextParser{}, nil })
	parse.RegisterExtension(".txt", "text")
	parse.RegisterExtension(".text", "text")
	parse.RegisterExtension(".log", "text")
	parse.RegisterExtension(".mdown", "text")
}

// SupportedExtensions returns .txt, .text, .log, .mdown.
func (p *TextParser) SupportedExtensions() []string {
	return []string{".txt", ".text", ".log", ".mdown"}
}

// Parse reads the local resource's content as UTF-8 text.
func (p *TextParser) Parse(ctx context.Context, res *parse.LocalResource, opts parse.ParserOptions) (*parse.ParseResult, error) {
	b, err := readAll(res)
	if err != nil {
		return nil, err
	}
	return p.parseBytes(b, res.Path)
}

// ParseContent parses an in-memory text blob.
func (p *TextParser) ParseContent(ctx context.Context, content []byte, sourcePath string, opts parse.ParserOptions) (*parse.ParseResult, error) {
	return p.parseBytes(content, sourcePath)
}

// parseBytes splits content into paragraphs on blank-line boundaries and
// builds a flat tree under a root node.
func (p *TextParser) parseBytes(b []byte, sourcePath string) (*parse.ParseResult, error) {
	content := string(b)
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(sourcePath)}
	for _, para := range splitParagraphs(content) {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		cp, _ := writeTempText(para)
		root.AddChild(&parse.ResourceNode{
			Type:        parse.NodeParagraph,
			ContentPath: cp,
			Meta:        map[string]any{"text": para},
		})
	}
	pr := parse.NewParseResult(root, sourcePath, "text", "TextParser")
	return pr, nil
}

// splitParagraphs splits s on one-or-more blank lines (\n\s*\n).
func splitParagraphs(s string) []string {
	normalized := strings.ReplaceAll(s, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	parts := strings.Split(normalized, "\n\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
