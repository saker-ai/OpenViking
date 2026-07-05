package parsers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// EPubParser parses .epub files. EPUB is a zip of XHTML files plus a
// container.xml manifest. We extract the spine-order XHTML files and
// concatenate their text.
type EPubParser struct{}

func init() {
	parse.RegisterParser("epub", func() (parse.Parser, error) { return &EPubParser{}, nil })
	parse.RegisterExtension(".epub", "epub")
}

// SupportedExtensions returns .epub.
func (p *EPubParser) SupportedExtensions() []string { return []string{".epub"} }

// Parse extracts text from the local epub file.
func (p *EPubParser) Parse(ctx context.Context, res *parse.LocalResource, opts parse.ParserOptions) (*parse.ParseResult, error) {
	if res == nil || res.Path == "" {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422,
			"epub parser: nil or empty local resource")
	}
	r, err := zip.OpenReader(res.Path)
	if err != nil {
		return nil, domain.Wrap(domain.CodeParseFailed, 422, err)
	}
	defer r.Close()
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(res.Path)}

	// 1. Parse META-INF/container.xml to find the OPF file.
	opfPath, err := findOPFPath(&r.Reader)
	if err != nil {
		return nil, domain.Wrap(domain.CodeParseFailed, 422, err)
	}
	// 2. Parse the OPF to get spine order + manifest href map.
	spine, manifest, err := parseOPF(&r.Reader, opfPath)
	if err != nil {
		return nil, domain.Wrap(domain.CodeParseFailed, 422, err)
	}
	// 3. For each spine item, extract text from its XHTML.
	for i, idref := range spine {
		href, ok := manifest[idref]
		if !ok {
			continue
		}
		text, title, err := extractEPubChapter(&r.Reader, href)
		if err != nil {
			continue
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		section := &parse.ResourceNode{
			Type:  parse.NodeSection,
			Level: 1,
			Title: firstNonEmpty(title, fmt.Sprintf("Chapter %d", i+1)),
		}
		cp, _ := writeTempText(text)
		section.AddChild(&parse.ResourceNode{
			Type:        parse.NodeParagraph,
			ContentPath: cp,
			Meta:        map[string]any{"text": text, "href": href},
		})
		root.AddChild(section)
	}
	pr := parse.NewParseResult(root, res.Path, "epub", "EPubParser")
	return pr, nil
}

// ParseContent parses an in-memory epub blob.
func (p *EPubParser) ParseContent(ctx context.Context, content []byte, sourcePath string, opts parse.ParserOptions) (*parse.ParseResult, error) {
	tmp, err := os.CreateTemp("", "openviking-epub-input-*.epub")
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

// findOPFPath parses META-INF/container.xml and returns the path to the
// OPF file (rootfile@full-path).
func findOPFPath(r *zip.Reader) (string, error) {
	f, err := r.Open("META-INF/container.xml")
	if err != nil {
		return "", err
	}
	defer f.Close()
	type rootFile struct {
		FullPath string `xml:"full-path,attr"`
	}
	type container struct {
		XMLName   xml.Name `xml:"container"`
		RootFiles []struct {
			RootFiles []rootFile `xml:"rootfile"`
		} `xml:"rootfiles"`
	}
	var c container
	if err := xml.NewDecoder(f).Decode(&c); err != nil {
		return "", err
	}
	for _, rf := range c.RootFiles {
		for _, r := range rf.RootFiles {
			if r.FullPath != "" {
				return r.FullPath, nil
			}
		}
	}
	return "", fmt.Errorf("epub: no rootfile in container.xml")
}

// parseOPF parses the OPF manifest and spine. Returns (spine-order idrefs,
// manifest id->href map).
func parseOPF(r *zip.Reader, opfPath string) ([]string, map[string]string, error) {
	f, err := r.Open(opfPath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	type item struct {
		ID        string `xml:"id,attr"`
		Href      string `xml:"href,attr"`
		MediaType string `xml:"media-type,attr"`
	}
	type itemRef struct {
		IDRef string `xml:"idref,attr"`
	}
	type opf struct {
		XMLName  xml.Name `xml:"package"`
		Manifest struct {
			Items []item `xml:"item"`
		} `xml:"manifest"`
		Spine struct {
			ItemRefs []itemRef `xml:"itemref"`
		} `xml:"spine"`
	}
	var o opf
	if err := xml.NewDecoder(f).Decode(&o); err != nil {
		return nil, nil, err
	}
	manifest := make(map[string]string, len(o.Manifest.Items))
	for _, it := range o.Manifest.Items {
		manifest[it.ID] = it.Href
	}
	spine := make([]string, 0, len(o.Spine.ItemRefs))
	for _, ref := range o.Spine.ItemRefs {
		spine = append(spine, ref.IDRef)
	}
	return spine, manifest, nil
}

// extractEPubChapter opens one XHTML file from the epub zip and returns
// (text, title).
func extractEPubChapter(r *zip.Reader, href string) (string, string, error) {
	// href may be relative; try as-is and then with "OEBPS/" prefix.
	candidates := []string{href, "OEBPS/" + href, "content/" + href}
	var f io.ReadCloser
	var err error
	for _, c := range candidates {
		f, err = r.Open(c)
		if err == nil {
			break
		}
	}
	if f == nil {
		return "", "", fmt.Errorf("epub: chapter not found: %s", href)
	}
	defer f.Close()
	var b bytes.Buffer
	if _, err := io.Copy(&b, f); err != nil {
		return "", "", err
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(b.Bytes()))
	if err != nil {
		return "", "", err
	}
	title := doc.Find("title").First().Text()
	if strings.TrimSpace(title) == "" {
		title = doc.Find("h1, h2").First().Text()
	}
	// Strip script/style; concatenate text.
	doc.Find("script, style, head").Remove()
	text := doc.Find("body").Text()
	if strings.TrimSpace(text) == "" {
		text = doc.Text()
	}
	return strings.TrimSpace(text), strings.TrimSpace(title), nil
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// _ = sort.Strings is an import-keeper.
var _ = sort.Strings
