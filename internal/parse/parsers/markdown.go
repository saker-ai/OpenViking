package parsers

import (
	"context"
	"fmt"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"

	"github.com/saker-ai/ctxhub/internal/parse"
)

// MarkdownParser parses .md / .markdown files using yuin/goldmark. The
// AST is walked to produce a ResourceNode tree preserving heading levels.
type MarkdownParser struct{}

func init() {
	parse.RegisterParser("markdown", func() (parse.Parser, error) { return &MarkdownParser{}, nil })
	parse.RegisterExtension(".md", "markdown")
	parse.RegisterExtension(".markdown", "markdown")
}

// SupportedExtensions returns .md and .markdown.
func (p *MarkdownParser) SupportedExtensions() []string { return []string{".md", ".markdown"} }

// Parse parses the local resource's content as Markdown.
func (p *MarkdownParser) Parse(ctx context.Context, res *parse.LocalResource, opts parse.ParserOptions) (*parse.ParseResult, error) {
	b, err := readAll(res)
	if err != nil {
		return nil, err
	}
	return p.parseBytes(b, res.Path)
}

// ParseContent parses an in-memory Markdown blob.
func (p *MarkdownParser) ParseContent(ctx context.Context, content []byte, sourcePath string, opts parse.ParserOptions) (*parse.ParseResult, error) {
	return p.parseBytes(content, sourcePath)
}

// parseBytes walks the goldmark AST and produces a ResourceNode tree.
func (p *MarkdownParser) parseBytes(b []byte, sourcePath string) (*parse.ParseResult, error) {
	gm := goldmark.New()
	reader := text.NewReader(b)
	doc := gm.Parser().Parse(reader)
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(sourcePath)}
	stack := []*parse.ResourceNode{root}
	walkAST(doc, b, stack)
	pr := parse.NewParseResult(root, sourcePath, "markdown", "MarkdownParser")
	return pr, nil
}

// walkAST traverses the goldmark AST, appending nodes to the stack. Headings
// push a new section node and pop on level decrease; paragraphs become leaf
// NodeParagraph entries with the literal text in Meta.text.
func walkAST(n ast.Node, source []byte, stack []*parse.ResourceNode) {
	level := len(stack)
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch c.Kind() {
		case ast.KindHeading:
			h := c.(*ast.Heading)
			headingLevel := h.Level
			// Pop until we're at a shallower level.
			for len(stack) > 1 && len(stack)-1 >= headingLevel {
				stack = stack[:len(stack)-1]
			}
			for len(stack) < headingLevel {
				placeholder := &parse.ResourceNode{Type: parse.NodeSection, Level: len(stack)}
				stack[len(stack)-1].AddChild(placeholder)
				stack = append(stack, placeholder)
			}
			section := &parse.ResourceNode{
				Type:  parse.NodeSection,
				Level: headingLevel,
				Title: string(c.Text(source)),
			}
			stack[len(stack)-1].AddChild(section)
			stack = append(stack, section)
			walkAST(c, source, stack)
			stack = stack[:len(stack)-1]
		case ast.KindParagraph:
			text := strings.TrimSpace(string(c.Text(source)))
			if text == "" {
				continue
			}
			cp, _ := writeTempText(text)
			stack[len(stack)-1].AddChild(&parse.ResourceNode{
				Type:        parse.NodeParagraph,
				ContentPath: cp,
				Meta:        map[string]any{"text": text},
			})
		case ast.KindFencedCodeBlock:
			text := strings.TrimSpace(extractCodeBlock(c, source))
			if text == "" {
				continue
			}
			cp, _ := writeTempText(text)
			stack[len(stack)-1].AddChild(&parse.ResourceNode{
				Type:        parse.NodeCode,
				ContentPath: cp,
				Meta:        map[string]any{"text": text, "lang": codeBlockLang(c, source)},
			})
		case ast.KindCodeBlock:
			text := strings.TrimSpace(extractCodeBlock(c, source))
			if text == "" {
				continue
			}
			cp, _ := writeTempText(text)
			stack[len(stack)-1].AddChild(&parse.ResourceNode{
				Type:        parse.NodeCode,
				ContentPath: cp,
				Meta:        map[string]any{"text": text},
			})
		default:
			// Recurse into other block types (lists, blockquotes, ...) and
			// let the cases above handle their children.
			walkAST(c, source, stack)
		}
	}
	_ = level
}

// extractCodeBlock pulls the literal text of a code-block node from the
// source slice. FencedCodeBlock and CodeBlock store content in their
// Lines() segments, not in child nodes.
func extractCodeBlock(n ast.Node, source []byte) string {
	if lines := n.Lines(); lines != nil && lines.Len() > 0 {
		var b strings.Builder
		for i := 0; i < lines.Len(); i++ {
			seg := lines.At(i)
			b.Write(seg.Value(source))
		}
		return b.String()
	}
	return string(n.Text(source))
}

// codeBlockLang extracts the language tag from a fenced code block's
// info string.
func codeBlockLang(n ast.Node, source []byte) string {
	if n.Kind() != ast.KindFencedCodeBlock {
		return ""
	}
	fcb := n.(*ast.FencedCodeBlock)
	if fcb.Info == nil {
		return ""
	}
	lang := fcb.Info.Text(source)
	return string(lang)
}

// _ = fmt.Sprintf is an import-keeper for future parser extensions.
var _ = fmt.Sprintf

// _ = util.UTF8Len is an import-keeper to keep the goldmark/util import.
var _ = util.IsSpace
