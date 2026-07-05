package parsers

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/go-shiori/go-readability"

	"github.com/saker-ai/ctxhub/internal/parse"
)

// HTMLParser parses .html / .htm files. It first tries the readability
// extractor for the main article body; on failure it falls back to a
// goquery walk that strips script/style and concatenates text blocks.
type HTMLParser struct{}

func init() {
	parse.RegisterParser("html", func() (parse.Parser, error) { return &HTMLParser{}, nil })
	parse.RegisterExtension(".html", "html")
	parse.RegisterExtension(".htm", "html")
	parse.RegisterExtension(".xhtml", "html")
}

// SupportedExtensions returns .html, .htm, .xhtml.
func (p *HTMLParser) SupportedExtensions() []string { return []string{".html", ".htm", ".xhtml"} }

// Parse parses the local HTML file. It tries readability first, then
// falls back to a goquery walk.
func (p *HTMLParser) Parse(ctx context.Context, res *parse.LocalResource, opts parse.ParserOptions) (*parse.ParseResult, error) {
	b, err := readAll(res)
	if err != nil {
		return nil, err
	}
	return p.parseBytes(b, res.Path)
}

// ParseContent parses an in-memory HTML blob.
func (p *HTMLParser) ParseContent(ctx context.Context, content []byte, sourcePath string, opts parse.ParserOptions) (*parse.ParseResult, error) {
	return p.parseBytes(content, sourcePath)
}

// parseBytes first tries readability; on failure it walks the HTML with
// goquery.
func (p *HTMLParser) parseBytes(b []byte, sourcePath string) (*parse.ParseResult, error) {
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(sourcePath)}
	pr := parse.NewParseResult(root, sourcePath, "html", "HTMLParser")

	// Try readability for the article body. The page URL is unknown for
	// local files, so we pass nil and let readability work in URL-free
	// mode (it skips RSS/feed metadata extraction).
	article, err := readability.FromReader(bytes.NewReader(b), nil)
	if err == nil && strings.TrimSpace(article.TextContent) != "" {
		title := article.Title
		if title == "" {
			title = titleFromPath(sourcePath)
		}
		root.Title = title
		cp, _ := writeTempText(article.TextContent)
		articleMeta := map[string]any{
			"text":      article.TextContent,
			"byline":    article.Byline,
			"excerpt":   article.Excerpt,
			"language":  article.Language,
			"site_name": article.SiteName,
			"length":    article.Length,
		}
		if article.Image != "" {
			articleMeta["image"] = article.Image
		}
		if article.PublishedTime != nil {
			articleMeta["published_at"] = article.PublishedTime.UTC().Format("2006-01-02T15:04:05Z")
		}
		root.AddChild(&parse.ResourceNode{
			Type:        parse.NodeParagraph,
			ContentPath: cp,
			Meta:        articleMeta,
		})
		return pr, nil
	}

	// Fallback: goquery walk.
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(b)))
	if err == nil {
		walkHTML(doc, root)
	}
	if root.Children == nil {
		// Last resort: emit the raw bytes as a single paragraph.
		cp, _ := writeTempText(string(b))
		root.AddChild(&parse.ResourceNode{
			Type:        parse.NodeParagraph,
			ContentPath: cp,
			Meta:        map[string]any{"text": string(b)},
		})
		pr.AddWarning("html: readability and goquery both failed; emitted raw bytes")
	}
	return pr, nil
}

// walkHTML traverses a goquery Document and emits NodeSection per heading
// and NodeParagraph per text block.
func walkHTML(doc *goquery.Document, root *parse.ResourceNode) {
	stack := []*parse.ResourceNode{root}
	doc.Find("h1, h2, h3, h4, h5, h6, p, pre, code, table, ul, ol").Each(func(_ int, s *goquery.Selection) {
		tag := goquery.NodeName(s)
		text := strings.TrimSpace(s.Text())
		if text == "" {
			return
		}
		switch tag {
		case "h1", "h2", "h3", "h4", "h5", "h6":
			level := int(tag[1] - '0')
			for len(stack) > 1 && len(stack)-1 >= level {
				stack = stack[:len(stack)-1]
			}
			for len(stack) < level {
				placeholder := &parse.ResourceNode{Type: parse.NodeSection, Level: len(stack)}
				stack[len(stack)-1].AddChild(placeholder)
				stack = append(stack, placeholder)
			}
			section := &parse.ResourceNode{
				Type:  parse.NodeSection,
				Level: level,
				Title: text,
			}
			stack[len(stack)-1].AddChild(section)
			stack = append(stack, section)
		case "pre", "code":
			cp, _ := writeTempText(text)
			stack[len(stack)-1].AddChild(&parse.ResourceNode{
				Type:        parse.NodeCode,
				ContentPath: cp,
				Meta:        map[string]any{"text": text},
			})
		default:
			cp, _ := writeTempText(text)
			stack[len(stack)-1].AddChild(&parse.ResourceNode{
				Type:        parse.NodeParagraph,
				ContentPath: cp,
				Meta:        map[string]any{"text": text},
			})
		}
	})
}

// stripHTMLTags is a quick-and-dirty fallback that removes all <...>
// tags from a string. Used when goquery parsing fails.
func stripHTMLTags(s string) string {
	re := regexp.MustCompile(`<[^>]+>`)
	return re.ReplaceAllString(s, "")
}

// _ = fmt.Sprintf / os.Stat are import-keepers for future extensions.
var (
	_ = fmt.Sprintf
	_ = os.Stat
)
