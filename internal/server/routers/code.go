package routers

import (
	"bytes"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// RegisterCode wires /api/v1/code/* — code outline / search / expand.
//
// Three endpoints, all POST so they can take a JSON body without dragging
// query-string parsing into the mix:
//   - POST /code/outline — structurally outline a file (functions, types,
//     imports) using a small regex-based heuristic per language.
//   - POST /code/search  — regex-content search via ragfs.Grep.
//   - POST /code/expand  — read a slice of a file by [start,end] line range.
//
// All paths are scoped to the caller's account: /accounts/{account}/code/<path>.
func RegisterCode(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/code")
	r.POST("/outline", codeOutline(deps))
	r.POST("/search", codeSearch(deps))
	r.POST("/expand", codeExpand(deps))
}

// codePathRequest is the common JSON body for code endpoints that take a path.
//
// Field shape mirrors openviking/server/routers/code.py CodeOutlineRequest:
// the Python field is "uri". The Go implementation has historically used
// "path"; we accept both so Python-authored clients (SDK, bot, eval) and
// Go-authored clients keep working. "uri" takes precedence when both are
// supplied.
type codePathRequest struct {
	// Python-aligned field (preferred).
	URI string `json:"uri,omitempty"`
	// Go-specific alias (kept for backward compat).
	Path string `json:"path,omitempty"`
}

// resolvedPath returns the effective path (Python "uri" preferred over the
// Go "path" alias).
func (r *codePathRequest) resolvedPath() string {
	if r == nil {
		return ""
	}
	if r.URI != "" {
		return r.URI
	}
	return r.Path
}

// codeSearchRequest extends codePathRequest with a regex pattern.
//
// Field shape mirrors openviking/server/routers/code.py CodeSearchRequest:
// Python uses "uri" + "query". The Go implementation uses "path" +
// "pattern"; we accept both, with Python fields taking precedence.
type codeSearchRequest struct {
	// Python-aligned fields (preferred).
	URI   string `json:"uri,omitempty"`
	Query string `json:"query,omitempty"`
	// Go-specific aliases (kept for backward compat).
	Path      string `json:"path,omitempty"`
	Pattern   string `json:"pattern,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
}

// resolvedSearchPath returns the effective path for /code/search.
func (r *codeSearchRequest) resolvedSearchPath() string {
	if r == nil {
		return ""
	}
	if r.URI != "" {
		return r.URI
	}
	return r.Path
}

// resolvedQuery returns the effective query/pattern for /code/search.
func (r *codeSearchRequest) resolvedQuery() string {
	if r == nil {
		return ""
	}
	if r.Query != "" {
		return r.Query
	}
	return r.Pattern
}

// codeExpandRequest extends codePathRequest with a symbol identifier.
//
// Field shape mirrors openviking/server/routers/code.py CodeExpandRequest:
// Python uses "uri" + "symbol". The Go implementation has historically used
// "path" + (start,end) line range; we accept both so a Python client can
// ask by symbol and a Go client can ask by range. "uri" + "symbol" take
// precedence when supplied.
type codeExpandRequest struct {
	// Python-aligned fields (preferred when non-empty).
	URI    string `json:"uri,omitempty"`
	Symbol string `json:"symbol,omitempty"`
	// Go-specific aliases (kept for backward compat).
	Path  string `json:"path,omitempty"`
	Start int    `json:"start,omitempty"`
	End   int    `json:"end,omitempty"`
}

// resolvedExpandPath returns the effective path for /code/expand.
func (r *codeExpandRequest) resolvedExpandPath() string {
	if r == nil {
		return ""
	}
	if r.URI != "" {
		return r.URI
	}
	return r.Path
}

// codePath resolves a request path against the caller's account /code prefix.
// When identity is absent the raw path is used (tests only).
func codePath(c *gin.Context, p string) string {
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "code", p))
	}
	return ragfs.Normalize(p)
}

// codeOutline handles POST /code/outline — read a file and return a coarse
// structural outline. The heuristic is language-aware: Go, Python, JS/TS,
// and Rust source files are scanned for top-level declarations via regex.
// Non-source files return an empty outline with the file size so callers
// can fall back to a raw read.
//
// Response shape mirrors openviking/server/routers/code.py code_outline:
// {"status":"ok","result":{...}}. The Go-specific path/outline/size fields
// are kept under result so existing Go clients keep working.
func codeOutline(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req codePathRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		p := req.resolvedPath()
		if p == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "path is required"))
			return
		}
		abs := codePath(c, p)
		var buf bytes.Buffer
		if err := deps.RAGFS.Read(c.Request.Context(), abs, &buf); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		outline := buildOutline(p, buf.Bytes())
		c.JSON(http.StatusOK, okResponse(gin.H{
			"path":    abs,
			"outline": outline,
			"size":    buf.Len(),
		}))
	}
}

// codeSearch handles POST /code/search — regex search via ragfs.Grep.
// Recursive defaults to true so a bare pattern matches across the subtree.
//
// Response shape mirrors openviking/server/routers/code.py code_search:
// {"status":"ok","result":{...}}. The Go-specific path/pattern/matches
// fields are kept under result.
func codeSearch(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req codeSearchRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		query := req.resolvedQuery()
		if query == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "query is required"))
			return
		}
		p := codePath(c, req.resolvedSearchPath())
		matches, err := deps.RAGFS.Grep(c.Request.Context(), query, p, req.Recursive)
		if err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"path":    p,
			"pattern": query,
			"matches": matches,
		}))
	}
}

// codeExpand handles POST /code/expand — read a [start,end] line slice of a
// file. Line numbers are 1-indexed; start/end default to 1 and EOF when 0.
//
// When the Python-aligned "symbol" field is supplied, the Go implementation
// cannot resolve it to a line range (no AST symbol table yet) and returns
// the full file with the symbol echoed under result.symbol_target so a
// Python client sees a non-error response. This is a known gap; a future
// AST-based resolver can fill it in.
//
// Response shape mirrors openviking/server/routers/code.py code_expand:
// {"status":"ok","result":{...}}.
func codeExpand(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req codeExpandRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		p := req.resolvedExpandPath()
		if p == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "path is required"))
			return
		}
		abs := codePath(c, p)
		var buf bytes.Buffer
		if err := deps.RAGFS.Read(c.Request.Context(), abs, &buf); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		start := req.Start
		if start < 1 {
			start = 1
		}
		end := req.End
		rawLines := bytes.Split(buf.Bytes(), []byte("\n"))
		// bytes.Split leaves a trailing empty element when the source ends
		// with a newline; drop it so Total matches the visible line count.
		if len(rawLines) > 0 && len(rawLines[len(rawLines)-1]) == 0 {
			rawLines = rawLines[:len(rawLines)-1]
		}
		if end <= 0 || end > len(rawLines) {
			end = len(rawLines)
		}
		if start > len(rawLines) {
			start = len(rawLines)
		}
		if start > end {
			start = end
		}
		slice := rawLines[start-1 : end]
		lines := make([]string, len(slice))
		for i, l := range slice {
			lines[i] = string(l)
		}
		result := gin.H{
			"path":   abs,
			"start":  start,
			"end":    end,
			"total":  len(rawLines),
			"lines":  lines,
			"source": string(bytes.Join(slice, []byte("\n"))),
		}
		if req.Symbol != "" {
			// Echo the Python "symbol" field so clients can correlate
			// requests with responses. AST-based symbol resolution is a
			// known gap (see comment above).
			result["symbol"] = req.Symbol
		}
		c.JSON(http.StatusOK, okResponse(result))
	}
}

// outlineEntry is one row in a code outline.
type outlineEntry struct {
	Kind   string `json:"kind"` // "func" | "type" | "class" | "import" | "const" | "var"
	Name   string `json:"name"`
	Line   int    `json:"line"`
	Indent int    `json:"indent,omitempty"`
}

// buildOutline produces a coarse structural outline for the given source
// file. The heuristic is intentionally simple: regex per language family
// (Go, Python, JS/TS, Rust) for top-level declarations. Non-source files
// return an empty slice.
func buildOutline(filename string, src []byte) []outlineEntry {
	ext := strings.ToLower(extOf(filename))
	switch ext {
	case ".go":
		return scanOutline(src, []outlinePattern{
			{Kind: "import", Re: `^\s*import\s+(?:/\s*"([^"]+)"\s*/?)|(?:[(]\s*"([^"]+)"\s*[)])`, Group: 1},
			{Kind: "func", Re: `^func\s+(?:[(][^)]*[)]\s+)?([A-Za-z0-9_]+)\s*[(]`, Group: 1},
			{Kind: "type", Re: `^type\s+([A-Za-z0-9_]+)\s`, Group: 1},
			{Kind: "const", Re: `^const\s+([A-Za-z0-9_]+)\s`, Group: 1},
			{Kind: "var", Re: `^var\s+([A-Za-z0-9_]+)\s`, Group: 1},
		})
	case ".py":
		return scanOutline(src, []outlinePattern{
			{Kind: "import", Re: `^\s*(?:from\s+([A-Za-z0-9_.]+)\s+import|import\s+([A-Za-z0-9_.]+))`, Group: 1},
			{Kind: "class", Re: `^class\s+([A-Za-z0-9_]+)`, Group: 1},
			{Kind: "func", Re: `^def\s+([A-Za-z0-9_]+)`, Group: 1},
			{Kind: "func", Re: `^\s{4,}def\s+([A-Za-z0-9_]+)`, Group: 1},
		})
	case ".js", ".ts", ".jsx", ".tsx", ".mjs", ".cjs":
		return scanOutline(src, []outlinePattern{
			{Kind: "import", Re: `^\s*import\s+.*?\s+from\s+['"]([^'"]+)['"]`, Group: 1},
			{Kind: "func", Re: `^\s*(?:export\s+)?function\s+([A-Za-z0-9_]+)`, Group: 1},
			{Kind: "func", Re: `^\s*(?:export\s+)?const\s+([A-Za-z0-9_]+)\s*=\s*(?:async\s*)?[(]`, Group: 1},
			{Kind: "class", Re: `^\s*(?:export\s+)?class\s+([A-Za-z0-9_]+)`, Group: 1},
		})
	case ".rs":
		return scanOutline(src, []outlinePattern{
			{Kind: "import", Re: `^use\s+([A-Za-z0-9_:]+)`, Group: 1},
			{Kind: "func", Re: `^\s*(?:pub\s+)?fn\s+([A-Za-z0-9_]+)`, Group: 1},
			{Kind: "type", Re: `^\s*(?:pub\s+)?struct\s+([A-Za-z0-9_]+)`, Group: 1},
			{Kind: "type", Re: `^\s*(?:pub\s+)?enum\s+([A-Za-z0-9_]+)`, Group: 1},
			{Kind: "type", Re: `^\s*(?:pub\s+)?trait\s+([A-Za-z0-9_]+)`, Group: 1},
		})
	default:
		return []outlineEntry{}
	}
}

// outlinePattern is one regex + kind tag for scanOutline. Group is 1-indexed;
// when the pattern has a second capture group as an alternative (e.g. Python
// import), Group+1 is also tried.
type outlinePattern struct {
	Kind  string
	Re    string
	Group int
}

// scanOutline applies each pattern line-by-line and collects the first
// capture-group match. Patterns are matched in order; the first matching
// pattern wins for each line. Indentation is recorded so callers can render
// nested declarations.
func scanOutline(src []byte, patterns []outlinePattern) []outlineEntry {
	if len(patterns) == 0 {
		return []outlineEntry{}
	}
	regexes := make([]compiledPattern, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p.Re)
		if err != nil {
			continue
		}
		regexes = append(regexes, compiledPattern{re: re, kind: p.Kind, group: p.Group})
	}
	var out []outlineEntry
	lines := bytes.Split(src, []byte("\n"))
	for i, line := range lines {
		for _, r := range regexes {
			matches := r.re.FindSubmatch(line)
			// Try the primary group, then group+1 for alternation patterns.
			name, ok := firstCapture(matches, r.group)
			if !ok {
				continue
			}
			out = append(out, outlineEntry{
				Kind:   r.kind,
				Name:   name,
				Line:   i + 1,
				Indent: leadingSpaces(line),
			})
			break
		}
	}
	if out == nil {
		return []outlineEntry{}
	}
	return out
}

// compiledPattern is a compiled outlinePattern.
type compiledPattern struct {
	re    *regexp.Regexp
	kind  string
	group int
}

// firstCapture returns the first non-empty capture group starting at the
// given 1-indexed position. Alternation patterns (e.g. Python import) put
// the alternative in the next group, so we scan forward until a group is
// populated. Returns false when no group matches.
func firstCapture(matches [][]byte, group int) (string, bool) {
	for g := group; g+1 <= len(matches); g++ {
		if len(matches[g]) > 0 {
			return string(matches[g]), true
		}
	}
	return "", false
}

// extOf returns the lowercase file extension including the leading dot.
// Files without a dot return "".
func extOf(name string) string {
	idx := strings.LastIndex(name, ".")
	if idx < 0 {
		return ""
	}
	return name[idx:]
}

// leadingSpaces counts the leading space/tab characters in a line.
func leadingSpaces(line []byte) int {
	n := 0
	for _, b := range line {
		if b == ' ' || b == '\t' {
			n++
			continue
		}
		break
	}
	return n
}
