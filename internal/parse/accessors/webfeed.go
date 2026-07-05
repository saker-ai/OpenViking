package accessors

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// WebFeedAccessor fetches RSS/Atom/JSON Feed URLs and writes the
// concatenated entries to a temp file as Markdown. It is the Go
// equivalent of openviking/parse/accessors/web_feed_accessor.
type WebFeedAccessor struct {
	Parser *gofeed.Parser
}

func init() {
	parse.RegisterAccessor("feed", func() (parse.DataAccessor, error) {
		return &WebFeedAccessor{Parser: gofeed.NewParser()}, nil
	})
	parse.RegisterAccessor("rss", func() (parse.DataAccessor, error) {
		return &WebFeedAccessor{Parser: gofeed.NewParser()}, nil
	})
	parse.RegisterAccessor("atom", func() (parse.DataAccessor, error) {
		return &WebFeedAccessor{Parser: gofeed.NewParser()}, nil
	})
}

// CanHandle reports whether source is a feed URL. The accessor is
// conservative: it only claims URLs whose path ends in .rss/.xml/.atom
// or that are passed with opts.Site=true (sitemap-style whole-site
// ingest).
func (a *WebFeedAccessor) CanHandle(source string, opts parse.AccessorOptions) bool {
	s := parse.SchemeOf(source)
	if s != "http" && s != "https" {
		return false
	}
	if opts.Site {
		return true
	}
	lower := strings.ToLower(source)
	if strings.HasSuffix(lower, ".rss") ||
		strings.HasSuffix(lower, ".xml") ||
		strings.HasSuffix(lower, ".atom") ||
		strings.HasSuffix(lower, "/feed") ||
		strings.HasSuffix(lower, "/rss") {
		return true
	}
	return false
}

// Schemes returns feed, rss, atom.
func (a *WebFeedAccessor) Schemes() []string { return []string{"feed", "rss", "atom"} }

// Fetch downloads and parses the feed, then writes a Markdown file with
// one section per entry.
func (a *WebFeedAccessor) Fetch(ctx context.Context, source string, opts parse.AccessorOptions) (*parse.LocalResource, error) {
	parser := a.Parser
	if parser == nil {
		parser = gofeed.NewParser()
	}
	fp, err := parser.ParseURLWithContext(source, ctx)
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 502,
			fmt.Errorf("feed parse %s: %w", source, err))
	}
	tmp, err := os.CreateTemp(opts.TemporaryDir, "openviking-feed-*.md")
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	defer tmp.Close()

	meta := map[string]any{
		"url":       source,
		"feed_type": fp.FeedType,
		"title":     fp.Title,
	}
	if fp.Link != "" {
		meta["link"] = fp.Link
	}

	var b strings.Builder
	b.WriteString("# ")
	b.WriteString(safeStr(fp.Title, "Feed"))
	b.WriteString("\n\n")
	if fp.Description != "" {
		b.WriteString(fp.Description)
		b.WriteString("\n\n")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = len(fp.Items)
	}
	for i, item := range fp.Items {
		if i >= limit {
			break
		}
		b.WriteString("## ")
		b.WriteString(safeStr(item.Title, "Item"))
		b.WriteString("\n\n")
		if item.Link != "" {
			b.WriteString("Link: ")
			b.WriteString(item.Link)
			b.WriteString("\n\n")
		}
		if !item.PublishedParsed.IsZero() {
			b.WriteString("Published: ")
			b.WriteString(item.PublishedParsed.UTC().Format(time.RFC3339))
			b.WriteString("\n\n")
		}
		if item.Content != "" {
			b.WriteString(item.Content)
		} else if item.Description != "" {
			b.WriteString(item.Description)
		}
		b.WriteString("\n\n")
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	path := tmp.Name()
	info, _ := os.Stat(path)
	if info != nil {
		meta["size"] = info.Size()
	}
	return &parse.LocalResource{
		Path:           path,
		SourceType:     parse.SourceWebFeed,
		OriginalSource: source,
		IsTemporary:    true,
		Meta:           meta,
	}, nil
}

// safeStr returns s when non-empty, fallback otherwise.
func safeStr(s, fallback string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return fallback
}
