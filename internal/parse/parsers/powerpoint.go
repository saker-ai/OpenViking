package parsers

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/unidoc/unioffice/presentation"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// PowerPointParser parses .pptx files using unidoc/unioffice. Each slide
// becomes a NodeSection; non-empty text items within a slide are joined
// into a single NodeParagraph.
type PowerPointParser struct{}

func init() {
	parse.RegisterParser("powerpoint", func() (parse.Parser, error) { return &PowerPointParser{}, nil })
	parse.RegisterExtension(".pptx", "powerpoint")
}

// SupportedExtensions returns .pptx.
func (p *PowerPointParser) SupportedExtensions() []string { return []string{".pptx"} }

// Parse opens the local .pptx file and emits one section per slide.
func (p *PowerPointParser) Parse(ctx context.Context, res *parse.LocalResource, opts parse.ParserOptions) (*parse.ParseResult, error) {
	if res == nil || res.Path == "" {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422,
			"powerpoint parser: nil or empty local resource")
	}
	pres, err := presentation.Open(res.Path)
	if err != nil {
		return nil, domain.Wrap(domain.CodeParseFailed, 422, err)
	}
	defer pres.Close()
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(res.Path)}
	pt := pres.ExtractText()
	if pt == nil {
		return nil, domain.NewAppError(domain.CodeParseFailed, 422,
			"powerpoint parser: extract text returned nil")
	}
	for i, slide := range pt.Slides {
		var b strings.Builder
		for _, item := range slide.Items {
			text := strings.TrimSpace(item.Text)
			if text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(text)
		}
		text := strings.TrimSpace(b.String())
		if text == "" {
			continue
		}
		section := &parse.ResourceNode{
			Type:  parse.NodeSection,
			Level: 1,
			Title: fmt.Sprintf("Slide %d", i+1),
		}
		cp, _ := writeTempText(text)
		section.AddChild(&parse.ResourceNode{
			Type:        parse.NodeParagraph,
			ContentPath: cp,
			Meta:        map[string]any{"text": text, "slide": i + 1},
		})
		root.AddChild(section)
	}
	pr := parse.NewParseResult(root, res.Path, "pptx", "PowerPointParser")
	return pr, nil
}

// ParseContent parses an in-memory .pptx blob.
func (p *PowerPointParser) ParseContent(ctx context.Context, content []byte, sourcePath string, opts parse.ParserOptions) (*parse.ParseResult, error) {
	tmp, err := os.CreateTemp("", "openviking-pptx-input-*.pptx")
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	tmp.Close()
	return p.Parse(ctx, &parse.LocalResource{
		Path:           tmp.Name(),
		SourceType:     parse.SourceLocal,
		OriginalSource: sourcePath,
		IsTemporary:    false,
	}, opts)
}
