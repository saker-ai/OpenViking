package parsers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ledongthuc/pdf"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// PDFParser parses .pdf files using ledongthuc/pdf for text and
// pdfcpu/pdfcpu for metadata.
type PDFParser struct{}

func init() {
	parse.RegisterParser("pdf", func() (parse.Parser, error) { return &PDFParser{}, nil })
	parse.RegisterExtension(".pdf", "pdf")
}

// SupportedExtensions returns .pdf.
func (p *PDFParser) SupportedExtensions() []string { return []string{".pdf"} }

// Parse extracts text from the local PDF file and emits one NodeSection
// per page plus one NodeParagraph with the page's text.
func (p *PDFParser) Parse(ctx context.Context, res *parse.LocalResource, opts parse.ParserOptions) (*parse.ParseResult, error) {
	if res == nil || res.Path == "" {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422,
			"pdf parser: nil or empty local resource")
	}
	meta, metaErr := pdfInfo(res.Path)
	text, numPages, err := pdfExtractText(res.Path)
	if err != nil {
		return nil, domain.Wrap(domain.CodeParseFailed, 422, err)
	}
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(res.Path)}
	if meta != nil {
		root.Meta = meta
	}
	if numPages > 0 {
		root.Meta["page_count"] = numPages
	}
	// Split by page using form feed (\x0c) which GetPlainText emits.
	pages := strings.Split(text, "\x0c")
	for i, page := range pages {
		page = strings.TrimSpace(page)
		if page == "" {
			continue
		}
		section := &parse.ResourceNode{
			Type:  parse.NodeSection,
			Level: 1,
			Title: fmt.Sprintf("Page %d", i+1),
		}
		cp, _ := writeTempText(page)
		section.AddChild(&parse.ResourceNode{
			Type:        parse.NodeParagraph,
			ContentPath: cp,
			Meta:        map[string]any{"text": page, "page": i + 1},
		})
		root.AddChild(section)
	}
	pr := parse.NewParseResult(root, res.Path, "pdf", "PDFParser")
	if metaErr != nil {
		pr.AddWarning("metadata: " + metaErr.Error())
	}
	return pr, nil
}

// ParseContent parses an in-memory PDF blob by writing it to a temp file
// and calling Parse.
func (p *PDFParser) ParseContent(ctx context.Context, content []byte, sourcePath string, opts parse.ParserOptions) (*parse.ParseResult, error) {
	tmp, err := os.CreateTemp("", "openviking-pdf-input-*.pdf")
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

// pdfExtractText uses ledongthuc/pdf to read all pages as plain text.
// Returns the concatenated text (pages separated by \x0c) and page count.
func pdfExtractText(path string) (string, int, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	numPages := r.NumPage()
	reader, err := r.GetPlainText()
	if err != nil {
		return "", numPages, err
	}
	var b bytes.Buffer
	if _, err := io.Copy(&b, reader); err != nil {
		return "", numPages, err
	}
	return b.String(), numPages, nil
}

// pdfInfo uses pdfcpu to read PDF metadata (page count, title, author).
func pdfInfo(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := api.PDFInfo(f, path, nil, false, model.NewDefaultConfiguration())
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if info != nil {
		if info.PageCount > 0 {
			out["page_count"] = info.PageCount
		}
		if info.Title != "" {
			out["title"] = info.Title
		}
		if info.Author != "" {
			out["author"] = info.Author
		}
		if info.Creator != "" {
			out["creator"] = info.Creator
		}
	}
	return out, nil
}
