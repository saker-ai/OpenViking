// Package parse implements the two-layer document parsing pipeline:
//
//   - L1 Accessor: fetches data from remote/special sources into a local
//     file or directory (LocalResource).
//   - L2 Parser:   converts a LocalResource into a ParseResult (a tree of
//     ResourceNode values preserving the document's natural hierarchy).
//
// Accessors are routed by URI scheme; parsers are routed by file
// extension. The Dispatch function picks the accessor by URI scheme and
// the parser by file extension in one call.
//
// See docs/design/parser-two-layer-refactor-plan.md for the architecture.
package parse

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"time"
)

// SourceType labels the origin of a LocalResource.
type SourceType string

const (
	// SourceLocal is a local filesystem resource.
	SourceLocal SourceType = "local"
	// SourceGit is a git repository fetched by GitAccessor.
	SourceGit SourceType = "git"
	// SourceHTTP is an HTTP/HTTPS resource fetched by HTTPAccessor.
	SourceHTTP SourceType = "http"
	// SourceWebFeed is an RSS/Atom feed fetched by WebFeedAccessor.
	SourceWebFeed SourceType = "webfeed"
	// SourceFeishu is a Feishu/Lark document fetched by FeishuAccessor.
	SourceFeishu SourceType = "feishu"
	// SourceWebCrawler is a site crawled by WebCrawlerAccessor.
	SourceWebCrawler SourceType = "webcrawler"
)

// LocalResource is the output of the L1 Accessor layer: a locally
// accessible file or directory plus metadata about its origin.
type LocalResource struct {
	// Path is the local file or directory path.
	Path string
	// SourceType is the original source type (SourceLocal / SourceGit / ...).
	SourceType SourceType
	// OriginalSource is the original source string (URL, repo URL, ...).
	OriginalSource string
	// Meta carries additional metadata (repo_name, branch,
	// content_type, ...).
	Meta map[string]any
	// IsTemporary reports whether Path can be deleted after parsing.
	IsTemporary bool
}

// Cleanup removes the local resource when temporary. Errors are silently
// swallowed; the OS will reap temp dirs on reboot.
func (r *LocalResource) Cleanup() {
	if r == nil || !r.IsTemporary || r.Path == "" {
		return
	}
	info, err := os.Stat(r.Path)
	if err != nil {
		return
	}
	if info.IsDir() {
		_ = os.RemoveAll(r.Path)
	} else {
		_ = os.Remove(r.Path)
	}
}

// Close marks the resource as cleaned up. Errors are swallowed.
func (r *LocalResource) Close() error {
	r.Cleanup()
	return nil
}

// NodeType enumerates the kinds of nodes in a parsed document tree.
type NodeType string

const (
	// NodeRoot is the top-level node.
	NodeRoot NodeType = "root"
	// NodeSection is a heading-delimited section.
	NodeSection NodeType = "section"
	// NodeParagraph is a leaf paragraph.
	NodeParagraph NodeType = "paragraph"
	// NodeCode is a code block.
	NodeCode NodeType = "code"
	// NodeTable is a table block.
	NodeTable NodeType = "table"
	// NodeImage is an image reference.
	NodeImage NodeType = "image"
	// NodeList is an ordered/unordered list.
	NodeList NodeType = "list"
	// NodeFile is a leaf file inside a directory or archive.
	NodeFile NodeType = "file"
)

// ResourceNode is one node in the parsed document tree. The tree
// preserves the natural document hierarchy (sections, paragraphs, ...)
// rather than arbitrary chunking.
type ResourceNode struct {
	Type        NodeType        `json:"type"`
	Title       string          `json:"title,omitempty"`
	ContentPath string          `json:"content_path,omitempty"`
	Level       int             `json:"level,omitempty"`
	Meta        map[string]any  `json:"meta,omitempty"`
	Children    []*ResourceNode `json:"children,omitempty"`
}

// AddChild appends a child node and returns it for chaining.
func (n *ResourceNode) AddChild(child *ResourceNode) *ResourceNode {
	if n == nil || child == nil {
		return n
	}
	n.Children = append(n.Children, child)
	return child
}

// ParseResult is the output of the L2 Parser layer.
type ParseResult struct {
	Root           *ResourceNode  `json:"root"`
	SourcePath     string         `json:"source_path,omitempty"`
	SourceFormat   string         `json:"source_format,omitempty"`
	ParserName     string         `json:"parser_name,omitempty"`
	ParserVersion  string         `json:"parser_version,omitempty"`
	ParseTime      float64        `json:"parse_time,omitempty"`
	ParseTimestamp time.Time      `json:"parse_timestamp,omitempty"`
	TempDirPath    string         `json:"temp_dir_path,omitempty"`
	Meta           map[string]any `json:"meta,omitempty"`
	Warnings       []string       `json:"warnings,omitempty"`
}

// Success reports whether parsing completed without warnings.
func (p *ParseResult) Success() bool {
	return p != nil && len(p.Warnings) == 0
}

// AllNodes returns a flattened, depth-first traversal of the tree.
func (p *ParseResult) AllNodes() []*ResourceNode {
	if p == nil || p.Root == nil {
		return nil
	}
	var out []*ResourceNode
	var walk func(*ResourceNode)
	walk = func(n *ResourceNode) {
		if n == nil {
			return
		}
		out = append(out, n)
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(p.Root)
	return out
}

// Sections returns the section nodes within [minLevel, maxLevel].
func (p *ParseResult) Sections(minLevel, maxLevel int) []*ResourceNode {
	if minLevel > maxLevel {
		minLevel, maxLevel = maxLevel, minLevel
	}
	var out []*ResourceNode
	for _, n := range p.AllNodes() {
		if n.Type != NodeSection {
			continue
		}
		if n.Level < minLevel || n.Level > maxLevel {
			continue
		}
		out = append(out, n)
	}
	return out
}

// NewParseResult is a convenience constructor that populates the
// metadata fields and timestamps.
func NewParseResult(root *ResourceNode, sourcePath, sourceFormat, parserName string) *ParseResult {
	return &ParseResult{
		Root:           root,
		SourcePath:     sourcePath,
		SourceFormat:   sourceFormat,
		ParserName:     parserName,
		ParserVersion:  "2.0",
		ParseTimestamp: time.Now().UTC(),
	}
}

// AddWarning appends a non-fatal warning and returns the result for
// chaining.
func (p *ParseResult) AddWarning(w string) *ParseResult {
	if p == nil {
		return p
	}
	p.Warnings = append(p.Warnings, w)
	return p
}

// DataAccessor is the L1 interface. Implementations fetch a remote
// source into a local file or directory.
type DataAccessor interface {
	// CanHandle reports whether this accessor can fetch the source.
	CanHandle(source string, opts AccessorOptions) bool

	// Fetch retrieves the source into a local resource. The caller owns
	// the returned LocalResource and must call Cleanup when done.
	Fetch(ctx context.Context, source string, opts AccessorOptions) (*LocalResource, error)

	// Schemes returns the URI schemes this accessor handles (e.g.
	// "http", "https"). Used by the registry for routing.
	Schemes() []string
}

// AccessorOptions carries optional hints from the caller.
type AccessorOptions struct {
	// Site hints that the source is a whole-site ingest (WebFeedAccessor
	// uses this to disambiguate sitemap URLs from single-article URLs).
	Site bool
	// Depth limits crawl depth for WebCrawlerAccessor. 0 means default.
	Depth int
	// Limit caps the number of children fetched (e.g. feed entries).
	Limit int
	// TemporaryDir overrides the OS temp dir for downloaded resources.
	TemporaryDir string
}

// Parser is the L2 interface. Implementations convert a LocalResource
// into a ParseResult.
type Parser interface {
	// SupportedExtensions returns the file extensions (lowercase,
	// dot-prefixed) this parser handles, e.g. ".md", ".markdown".
	SupportedExtensions() []string

	// Parse parses a local file or directory into a ParseResult.
	Parse(ctx context.Context, res *LocalResource, opts ParserOptions) (*ParseResult, error)

	// ParseContent parses an in-memory content blob. sourcePath, when
	// non-empty, is used for parser-name disambiguation only.
	ParseContent(ctx context.Context, content []byte, sourcePath string, opts ParserOptions) (*ParseResult, error)
}

// ParserOptions carries optional parse-time hints.
type ParserOptions struct {
	// Instruction guides the LLM how to understand the resource. Empty
	// disables LLM-driven understanding.
	Instruction string
	// MediaProcessor is the VLM client used by media parsers. nil
	// disables VLM (media parsers return ErrUnsupported).
	MediaProcessor VLMClient
}

// VLMClient abstracts the vision-language model calls used by media
// parsers. The real implementation is wired in P12 against
// internal/models/vlm; unit tests inject a fake.
type VLMClient interface {
	// DescribeImage returns a natural-language description of an image.
	DescribeImage(ctx context.Context, img []byte, mimeType string, instruction string) (string, error)
	// TranscribeAudio returns a transcript of an audio clip.
	TranscribeAudio(ctx context.Context, audio []byte, mimeType string, instruction string) (string, error)
	// DescribeVideo returns a description of a video clip.
	DescribeVideo(ctx context.Context, video []byte, mimeType string, instruction string) (string, error)
}

// tempDir creates a fresh temp directory under dir (or the OS default).
// Returns the absolute path.
func tempDir(dir string) (string, error) {
	if dir == "" {
		return os.MkdirTemp("", "openviking-parse-*")
	}
	return os.MkdirTemp(dir, "openviking-parse-*")
}

// copyFile copies src to dst with the given perm.
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// ext returns the lowercase file extension of path, including the dot.
func ext(path string) string {
	return filepath.Ext(path)
}
