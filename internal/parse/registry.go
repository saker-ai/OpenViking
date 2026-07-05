package parse

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// AccessorFactory constructs a DataAccessor. Registered with
// RegisterAccessor and invoked by LookupAccessor at registry build time.
type AccessorFactory func() (DataAccessor, error)

// ParserFactory constructs a Parser. Registered with RegisterParser.
type ParserFactory func() (Parser, error)

var (
	accessorRegMu sync.RWMutex
	accessorReg   = map[string]AccessorFactory{}

	parserRegMu sync.RWMutex
	parserReg   = map[string]ParserFactory{}
	extRegMu    sync.RWMutex
	extReg      = map[string]string{} // lowercase ext -> parser name
)

// RegisterAccessor installs an AccessorFactory under scheme. Re-registering
// an existing scheme overrides the prior factory.
func RegisterAccessor(scheme string, factory AccessorFactory) {
	if scheme == "" {
		panic("parse: register accessor with empty scheme")
	}
	if factory == nil {
		panic("parse: register nil accessor factory for " + scheme)
	}
	accessorRegMu.Lock()
	defer accessorRegMu.Unlock()
	accessorReg[scheme] = factory
}

// LookupAccessor returns the factory registered under scheme. ok=false
// when the scheme is unknown.
func LookupAccessor(scheme string) (AccessorFactory, bool) {
	accessorRegMu.RLock()
	defer accessorRegMu.RUnlock()
	f, ok := accessorReg[scheme]
	return f, ok
}

// RegisteredAccessors returns the URI schemes currently registered,
// sorted lexicographically.
func RegisteredAccessors() []string {
	accessorRegMu.RLock()
	out := make([]string, 0, len(accessorReg))
	for s := range accessorReg {
		out = append(out, s)
	}
	accessorRegMu.RUnlock()
	sort.Strings(out)
	return out
}

// RegisterParser installs a ParserFactory under name and binds the
// parser's supported extensions to that name.
func RegisterParser(name string, factory ParserFactory) {
	if name == "" {
		panic("parse: register parser with empty name")
	}
	if factory == nil {
		panic("parse: register nil parser factory for " + name)
	}
	parserRegMu.Lock()
	parserReg[name] = factory
	parserRegMu.Unlock()
}

// RegisterExtension binds a lowercase file extension (with leading dot,
// e.g. ".md") to a parser name. Callers typically call this from a
// parser's init(); RegisterParser does NOT auto-bind extensions because
// the parser instance is constructed lazily.
func RegisterExtension(ext, parserName string) {
	ext = strings.ToLower(ext)
	if ext == "" || parserName == "" {
		return
	}
	extRegMu.Lock()
	defer extRegMu.Unlock()
	extReg[ext] = parserName
}

// LookupParser returns the parser factory registered under name.
func LookupParser(name string) (ParserFactory, bool) {
	parserRegMu.RLock()
	defer parserRegMu.RUnlock()
	f, ok := parserReg[name]
	return f, ok
}

// LookupParserForExt returns the parser factory bound to ext (lowercase,
// dot-prefixed).
func LookupParserForExt(ext string) (ParserFactory, bool) {
	ext = strings.ToLower(ext)
	extRegMu.RLock()
	defer extRegMu.RUnlock()
	name, ok := extReg[ext]
	if !ok {
		return nil, false
	}
	parserRegMu.RLock()
	defer parserRegMu.RUnlock()
	f, ok := parserReg[name]
	return f, ok
}

// RegisteredParsers returns the parser names currently registered, sorted
// lexicographically.
func RegisteredParsers() []string {
	parserRegMu.RLock()
	out := make([]string, 0, len(parserReg))
	for n := range parserReg {
		out = append(out, n)
	}
	parserRegMu.RUnlock()
	sort.Strings(out)
	return out
}

// Dispatch picks the accessor by URI scheme and the parser by file
// extension in one call. The accessor fetches the source into a
// LocalResource; the parser converts it into a ParseResult. The caller
// owns the LocalResource and must call Cleanup when done.
//
// When the source is already a local path (no scheme), the LocalAccessor
// is used and the parser is invoked directly on the local file.
//
// Returns:
//   - domain.ErrUnsupported when no accessor or parser matches.
//   - domain.ErrNotFound when the local path does not exist.
func Dispatch(ctx context.Context, source string, opts AccessorOptions, popts ParserOptions) (*LocalResource, *ParseResult, error) {
	res, err := Access(ctx, source, opts)
	if err != nil {
		return nil, nil, err
	}
	pr, err := Parse(ctx, res, popts)
	if err != nil {
		res.Cleanup()
		return res, nil, err
	}
	return res, pr, nil
}

// Access picks the accessor by URI scheme and fetches the source. The
// caller owns the returned LocalResource.
func Access(ctx context.Context, source string, opts AccessorOptions) (*LocalResource, error) {
	scheme := schemeOf(source)
	factory, ok := LookupAccessor(scheme)
	if !ok {
		return nil, domain.ErrUnsupported
	}
	acc, err := factory()
	if err != nil {
		return nil, err
	}
	return acc.Fetch(ctx, source, opts)
}

// Parse picks the parser by file extension and parses the local resource.
func Parse(ctx context.Context, res *LocalResource, opts ParserOptions) (*ParseResult, error) {
	if res == nil {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422,
			"parse: nil local resource")
	}
	info, err := os.Stat(res.Path)
	if err != nil {
		return nil, domain.Wrap(domain.CodeResourceNotFound, 404, err)
	}
	if info.IsDir() {
		factory, ok := LookupParser("directory")
		if !ok {
			return nil, domain.ErrUnsupported
		}
		p, err := factory()
		if err != nil {
			return nil, err
		}
		return p.Parse(ctx, res, opts)
	}
	factory, ok := LookupParserForExt(ext(res.Path))
	if !ok {
		return nil, domain.ErrUnsupported
	}
	p, err := factory()
	if err != nil {
		return nil, err
	}
	return p.Parse(ctx, res, opts)
}

// schemeOf returns the lowercase URI scheme of source, or "local" when
// source is a plain path. Recognizes the standard "scheme://" form.
func schemeOf(source string) string {
	if source == "" {
		return "local"
	}
	idx := strings.Index(source, "://")
	if idx <= 0 {
		return "local"
	}
	return strings.ToLower(source[:idx])
}

// SchemeOf is the exported form of schemeOf, used by accessors that need
// to classify a source before fetching.
func SchemeOf(source string) string { return schemeOf(source) }

// FormatTableToMarkdown renders a 2D string matrix as a Markdown table.
// HasHeader marks the first row as a header.
func FormatTableToMarkdown(rows [][]string, hasHeader bool) string {
	if len(rows) == 0 {
		return ""
	}
	colCount := 0
	for _, r := range rows {
		if len(r) > colCount {
			colCount = len(r)
		}
	}
	if colCount == 0 {
		return ""
	}
	colWidths := make([]int, colCount)
	for _, r := range rows {
		for i, c := range r {
			if len(c) > colWidths[i] {
				colWidths[i] = len(c)
			}
		}
	}
	var b strings.Builder
	for rowIdx, r := range rows {
		for i := 0; i < colCount; i++ {
			cell := ""
			if i < len(r) {
				cell = r[i]
			}
			if i > 0 {
				b.WriteString(" | ")
			}
			b.WriteString(cell)
			for pad := len(cell); pad < colWidths[i]; pad++ {
				b.WriteByte(' ')
			}
		}
		b.WriteString("\n")
		if rowIdx == 0 && hasHeader && len(rows) > 1 {
			for i := 0; i < colCount; i++ {
				if i > 0 {
					b.WriteString(" | ")
				}
				for w := 0; w < colWidths[i]; w++ {
					b.WriteByte('-')
				}
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// formatSectionPath joins section titles into a slash-delimited path,
// mirroring Python's PageIndex.section_path.
func formatSectionPath(titles []string) string {
	var clean []string
	for _, t := range titles {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		clean = append(clean, t)
	}
	return strings.Join(clean, " / ")
}

// joinPath joins base and name, normalizing slashes.
func joinPath(base, name string) string {
	if base == "" {
		return name
	}
	return filepath.Join(base, name)
}
