package parsers

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/unidoc/unioffice/document"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// WordParser parses .docx files using unidoc/unioffice. Each paragraph
// becomes a NodeParagraph; consecutive headings are emitted as sections
// when the paragraph's outline level is set.
type WordParser struct{}

func init() {
	parse.RegisterParser("word", func() (parse.Parser, error) { return &WordParser{}, nil })
	parse.RegisterExtension(".docx", "word")
}

// SupportedExtensions returns .docx.
func (p *WordParser) SupportedExtensions() []string { return []string{".docx"} }

// Parse opens the local .docx file and emits one paragraph per non-empty
// run-block.
func (p *WordParser) Parse(ctx context.Context, res *parse.LocalResource, opts parse.ParserOptions) (*parse.ParseResult, error) {
	if res == nil || res.Path == "" {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422,
			"word parser: nil or empty local resource")
	}
	doc, err := document.Open(res.Path)
	if err != nil {
		return nil, domain.Wrap(domain.CodeParseFailed, 422, err)
	}
	defer doc.Close()
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(res.Path)}
	dt := doc.ExtractText()
	if dt == nil {
		return nil, domain.NewAppError(domain.CodeParseFailed, 422,
			"word parser: extract text returned nil")
	}
	var para strings.Builder
	flush := func() {
		text := strings.TrimSpace(para.String())
		if text == "" {
			para.Reset()
			return
		}
		cp, _ := writeTempText(text)
		root.AddChild(&parse.ResourceNode{
			Type:        parse.NodeParagraph,
			ContentPath: cp,
			Meta:        map[string]any{"text": text},
		})
		para.Reset()
	}
	for _, item := range dt.Items {
		text := strings.TrimSpace(item.Text)
		if text == "" {
			flush()
			continue
		}
		if para.Len() > 0 {
			para.WriteString("\n")
		}
		para.WriteString(text)
	}
	flush()
	pr := parse.NewParseResult(root, res.Path, "docx", "WordParser")
	return pr, nil
}

// ParseContent parses an in-memory .docx blob.
func (p *WordParser) ParseContent(ctx context.Context, content []byte, sourcePath string, opts parse.ParserOptions) (*parse.ParseResult, error) {
	tmp, err := os.CreateTemp("", "openviking-word-input-*.docx")
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

// _ = fmt.Sprintf is an import-keeper.
var _ = fmt.Sprintf
