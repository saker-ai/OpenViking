package parsers

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// LegacyDocParser parses .doc (legacy Word binary) files. The pure-Go
// ecosystem has no mature .doc reader (the binary format is undocumented
// in places and drifts between Word versions). This parser degrades to
// printable-text extraction: it scans the file for ASCII / UTF-8 runs of
// length >= 4 and concatenates them. This is enough to surface the
// visible text of a typical .doc; formatting / tables / images are lost.
//
// Install: a full .doc parser would require porting olefile + the Word
// binary format reader to pure Go; not currently planned.
type LegacyDocParser struct{}

func init() {
	parse.RegisterParser("legacy_doc", func() (parse.Parser, error) { return &LegacyDocParser{}, nil })
	parse.RegisterExtension(".doc", "legacy_doc")
}

// SupportedExtensions returns .doc.
func (p *LegacyDocParser) SupportedExtensions() []string { return []string{".doc"} }

// Parse extracts printable text from the local .doc file.
func (p *LegacyDocParser) Parse(ctx context.Context, res *parse.LocalResource, opts parse.ParserOptions) (*parse.ParseResult, error) {
	b, err := readAll(res)
	if err != nil {
		return nil, err
	}
	text := extractPrintable(b)
	if strings.TrimSpace(text) == "" {
		return nil, domain.Wrap(domain.CodeParseFailed, 422,
			fmt.Errorf("legacy_doc: no printable text in %s", res.Path))
	}
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(res.Path)}
	for _, para := range splitParagraphs(text) {
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
	pr := parse.NewParseResult(root, res.Path, "doc", "LegacyDocParser")
	pr.AddWarning("legacy_doc: degraded text extraction; formatting/tables/images lost")
	return pr, nil
}

// ParseContent parses an in-memory .doc blob.
func (p *LegacyDocParser) ParseContent(ctx context.Context, content []byte, sourcePath string, opts parse.ParserOptions) (*parse.ParseResult, error) {
	return p.parseBytes(content, sourcePath)
}

// parseBytes is the inner entry point shared by Parse and ParseContent.
func (p *LegacyDocParser) parseBytes(b []byte, sourcePath string) (*parse.ParseResult, error) {
	text := extractPrintable(b)
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(sourcePath)}
	for _, para := range splitParagraphs(text) {
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
	pr := parse.NewParseResult(root, sourcePath, "doc", "LegacyDocParser")
	pr.AddWarning("legacy_doc: degraded text extraction; formatting/tables/images lost")
	return pr, nil
}

// extractPrintable scans b for runs of printable ASCII / UTF-8
// characters and returns them joined by newlines. Runs of length < 4 are
// dropped to suppress binary noise.
func extractPrintable(b []byte) string {
	var out bytes.Buffer
	var run bytes.Buffer
	flushRun := func() {
		if run.Len() >= 4 {
			if out.Len() > 0 {
				out.WriteByte('\n')
			}
			out.Write(run.Bytes())
		}
		run.Reset()
	}
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			// Invalid byte; break the run.
			flushRun()
			i++
			continue
		}
		if isPrintableRune(r) {
			run.WriteRune(r)
		} else if r == '\n' || r == '\r' {
			flushRun()
		} else {
			flushRun()
		}
		i += size
	}
	flushRun()
	return out.String()
}

// isPrintableRune reports whether r is a printable character (letter,
// digit, punctuation, or space).
func isPrintableRune(r rune) bool {
	if r == ' ' || r == '\t' {
		return true
	}
	if r < 0x20 {
		return false
	}
	if r > 0x10FFFF {
		return false
	}
	// Reject control pictures and BOM.
	if r == 0xFEFF {
		return false
	}
	return true
}
