package accessors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkdocx "github.com/larksuite/oapi-sdk-go/v3/service/docx/v1"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// FeishuAccessor fetches Feishu/Lark documents and messages via the
// official larksuite/oapi-sdk-go/v3 client.
//
// Supported source URI forms:
//
//   - feishu://docx/{document_id}     — docx document, returns RawContent text
//   - feishu://im/{message_id}        — IM message, returns message body content
//   - https://*.feishu.cn/docx/{id}   — same as feishu://docx/{id}
//
// Credentials come from parse.feishu config (app_id + app_secret). The
// accessor uses tenant access tokens by default; user access tokens are
// not yet wired (deferred to OAuth flow).
type FeishuAccessor struct {
	client *lark.Client
}

func init() {
	parse.RegisterAccessor("feishu", func() (parse.DataAccessor, error) {
		return NewFeishuAccessorFromEnv()
	})
	parse.RegisterAccessor("feishu-doc", func() (parse.DataAccessor, error) {
		return NewFeishuAccessorFromEnv()
	})
	parse.RegisterAccessor("lark", func() (parse.DataAccessor, error) {
		return NewFeishuAccessorFromEnv()
	})
}

// NewFeishuAccessor constructs a FeishuAccessor with explicit credentials.
// domain may be empty (defaults to https://open.feishu.cn). Returns an
// accessor whose Fetch will return a clear configuration error when the
// client could not be initialized (e.g. missing app_id).
func NewFeishuAccessor(appID, appSecret, domain string) (*FeishuAccessor, error) {
	return newFeishuAccessor(appID, appSecret, domain, nil)
}

// NewFeishuAccessorWithClient constructs a FeishuAccessor with a custom
// HTTP client. Used by tests to point the SDK at an httptest server
// (via WithOpenBaseUrl + WithHttpClient).
func NewFeishuAccessorWithClient(appID, appSecret, domain string, httpClient *http.Client) (*FeishuAccessor, error) {
	return newFeishuAccessor(appID, appSecret, domain, httpClient)
}

// newFeishuAccessor is the shared constructor. When appID/appSecret are
// empty the accessor is returned with a nil client; Fetch then surfaces
// a clear configuration error rather than panicking.
func newFeishuAccessor(appID, appSecret, domain string, httpClient *http.Client) (*FeishuAccessor, error) {
	a := &FeishuAccessor{}
	if appID == "" || appSecret == "" {
		return a, nil
	}
	a.client = newLarkClient(appID, appSecret, domain, httpClient)
	return a, nil
}

// NewFeishuAccessorFromEnv reads credentials from the OV_PARSE_FEISHU_*
// environment variables (the viper loader maps parse.feishu.* keys to
// OV_PARSE_FEISHU_*). Missing credentials yield a registered accessor
// that returns a configuration error from Fetch.
func NewFeishuAccessorFromEnv() (*FeishuAccessor, error) {
	appID := os.Getenv("OV_PARSE_FEISHU_APP_ID")
	appSecret := os.Getenv("OV_PARSE_FEISHU_APP_SECRET")
	domain := os.Getenv("OV_PARSE_FEISHU_DOMAIN")
	return NewFeishuAccessor(appID, appSecret, domain)
}

// CanHandle reports whether source is a Feishu/Lark URL.
func (a *FeishuAccessor) CanHandle(source string, opts parse.AccessorOptions) bool {
	s := parse.SchemeOf(source)
	switch s {
	case "feishu", "feishu-doc", "lark":
		return true
	}
	if len(source) > 8 && (strings.HasPrefix(source, "https://") || strings.HasPrefix(source, "http://")) {
		if containsAny(source, []string{"feishu.cn", "larksuite.com", "larkoffice.com"}) {
			return true
		}
	}
	return false
}

// Schemes returns feishu, feishu-doc, lark.
func (a *FeishuAccessor) Schemes() []string { return []string{"feishu", "feishu-doc", "lark"} }

// Fetch retrieves the Feishu resource, writes its content to a temp
// markdown file, and returns a LocalResource.
func (a *FeishuAccessor) Fetch(ctx context.Context, source string, opts parse.AccessorOptions) (*parse.LocalResource, error) {
	if a.client == nil {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422,
			"feishu accessor: missing app_id/app_secret (set parse.feishu.app_id / parse.feishu.app_secret)")
	}
	docType, token, err := parseFeishuSource(source)
	if err != nil {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422, err)
	}

	var (
		content   string
		title     string
		fetchType string
	)
	switch docType {
	case "docx":
		body, docTitle, ferr := a.fetchDocxRawContent(ctx, token)
		if ferr != nil {
			return nil, ferr
		}
		content, title, fetchType = body, docTitle, "docx"
	case "wiki":
		// Wiki nodes resolve to a real docx token via the wiki API; that
		// endpoint requires wiki.v2.space.get_node which is not in this
		// SDK version's service path. Surface a clear error so callers
		// know to convert wiki URLs to docx URLs manually.
		return nil, domain.NewAppError(domain.CodeUnsupported, 501,
			"feishu accessor: wiki URL resolution is not supported; pass feishu://docx/{document_id} directly")
	case "im":
		body, ferr := a.fetchIMMessage(ctx, token)
		if ferr != nil {
			return nil, ferr
		}
		content, fetchType = body, "im"
	default:
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("feishu accessor: unsupported doc type %q (use docx or im)", docType))
	}

	tmpDir := opts.TemporaryDir
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	out, err := os.CreateTemp(tmpDir, "openviking-feishu-*-"+sanitizeFilename(token)+".md")
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	tmpPath := out.Name()
	// Write the markdown body. Prepend a title heading when known.
	if title != "" {
		content = "# " + title + "\n\n" + content
	}
	if _, err := out.WriteString(content); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}

	meta := map[string]any{
		"feishu_doc_type": fetchType,
		"feishu_token":    token,
		"feishu_title":    title,
		"original_url":    source,
	}
	if title != "" {
		meta["title"] = title
	}
	return &parse.LocalResource{
		Path:           tmpPath,
		SourceType:     parse.SourceFeishu,
		OriginalSource: source,
		IsTemporary:    true,
		Meta:           meta,
	}, nil
}

// fetchDocxRawContent calls docx.v1.document.raw_content and returns the
// plain-text body plus the document title (looked up via document.get).
func (a *FeishuAccessor) fetchDocxRawContent(ctx context.Context, documentID string) (string, string, error) {
	if a.client == nil || a.client.Docx == nil || a.client.Docx.Document == nil {
		return "", "", domain.NewAppError(domain.CodeInternalError, 500,
			"feishu accessor: lark client missing docx service")
	}
	rawReq := larkdocx.NewRawContentDocumentReqBuilder().
		DocumentId(documentID).
		Build()
	rawResp, err := a.client.Docx.Document.RawContent(ctx, rawReq)
	if err != nil {
		return "", "", domain.Wrap(domain.CodeInternalError, 502,
			fmt.Errorf("feishu accessor: docx raw_content request: %w", err))
	}
	if !rawResp.Success() {
		return "", "", domain.NewAppError(domain.CodeInternalError, 502,
			fmt.Sprintf("feishu accessor: docx raw_content failed: code=%d msg=%s", rawResp.Code, rawResp.Msg))
	}
	body := ""
	if rawResp.Data != nil && rawResp.Data.Content != nil {
		body = *rawResp.Data.Content
	}

	// Fetch document title (best-effort; ignore failures).
	title := ""
	getReq := larkdocx.NewGetDocumentReqBuilder().DocumentId(documentID).Build()
	getResp, err := a.client.Docx.Document.Get(ctx, getReq)
	if err == nil && getResp.Success() && getResp.Data != nil && getResp.Data.Document != nil {
		if t := getResp.Data.Document.Title; t != nil {
			title = strings.TrimSpace(*t)
		}
	}
	return strings.TrimSpace(body), title, nil
}

// fetchIMMessage calls im.v1.message.get and returns the message body
// content as plain text. Text/post message bodies are JSON-encoded; we
// extract the text content. Other message types (image, file, ...) are
// not yet supported.
func (a *FeishuAccessor) fetchIMMessage(ctx context.Context, messageID string) (string, error) {
	if a.client == nil || a.client.Im == nil || a.client.Im.Message == nil {
		return "", domain.NewAppError(domain.CodeInternalError, 500,
			"feishu accessor: lark client missing im service")
	}
	req := larkim.NewGetMessageReqBuilder().MessageId(messageID).Build()
	resp, err := a.client.Im.Message.Get(ctx, req)
	if err != nil {
		return "", domain.Wrap(domain.CodeInternalError, 502,
			fmt.Errorf("feishu accessor: im message get request: %w", err))
	}
	if !resp.Success() {
		return "", domain.NewAppError(domain.CodeInternalError, 502,
			fmt.Sprintf("feishu accessor: im message get failed: code=%d msg=%s", resp.Code, resp.Msg))
	}
	if resp.Data == nil || len(resp.Data.Items) == 0 {
		return "", nil
	}
	msg := resp.Data.Items[0]
	if msg == nil {
		return "", nil
	}
	msgType := ""
	if msg.MsgType != nil {
		msgType = *msg.MsgType
	}
	content := ""
	if msg.Body != nil && msg.Body.Content != nil {
		content = *msg.Body.Content
	}
	return imContentToText(msgType, content), nil
}

// imContentToText extracts plain text from a Lark IM message body. The
// content field is a JSON string whose shape depends on msg_type:
//
//   - text:   {"text": "..."}
//   - post:   {"zh_cn": {"title": "...", "content": [[{"tag":"text","text":"..."}]]}}
//
// Returns the raw content string when the type is unknown or parsing
// fails; callers can still inspect the markdown.
func imContentToText(msgType, content string) string {
	if content == "" {
		return ""
	}
	switch msgType {
	case "text":
		var body struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(content), &body) == nil && body.Text != "" {
			return body.Text
		}
	case "post":
		var body map[string]any
		if err := json.Unmarshal([]byte(content), &body); err != nil {
			break
		}
		// Pick any locale entry; they all carry the same shape.
		var locale any
		for _, v := range body {
			locale = v
			break
		}
		locMap, ok := locale.(map[string]any)
		if !ok {
			break
		}
		var chunks []string
		if title, ok := locMap["title"].(string); ok && strings.TrimSpace(title) != "" {
			chunks = append(chunks, "# "+strings.TrimSpace(title))
		}
		paras, _ := locMap["content"].([]any)
		for _, para := range paras {
			lines, _ := para.([]any)
			var lineText []string
			for _, line := range lines {
				m, ok := line.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := m["tag"].(string); t == "text" {
					if txt, _ := m["text"].(string); strings.TrimSpace(txt) != "" {
						lineText = append(lineText, strings.TrimSpace(txt))
					}
				}
			}
			if len(lineText) > 0 {
				chunks = append(chunks, strings.Join(lineText, " "))
			}
		}
		if len(chunks) > 0 {
			return strings.Join(chunks, "\n\n")
		}
	}
	// Fallback: return raw content so callers can inspect.
	return content
}

// parseFeishuSource splits a feishu/lark source into (doc_type, token).
// Accepts feishu://docx/{token}, https://*.feishu.cn/docx/{token}, and
// the message-id form feishu://im/{message_id}.
func parseFeishuSource(source string) (string, string, error) {
	scheme := parse.SchemeOf(source)
	if scheme == "feishu" || scheme == "feishu-doc" || scheme == "lark" {
		// feishu://docx/{token} — url.Parse treats "docx" as the host
		// and "{token}" as the path. Reconstruct by joining host + path.
		u, err := url.Parse(source)
		if err != nil {
			return "", "", fmt.Errorf("feishu accessor: parse source %q: %w", source, err)
		}
		docType := u.Host
		token := strings.TrimPrefix(u.Path, "/")
		// Strip any trailing query/fragment from token; the first path
		// segment is the token.
		if idx := strings.IndexByte(token, '/'); idx >= 0 {
			token = token[:idx]
		}
		if docType == "" || token == "" {
			return "", "", fmt.Errorf("feishu accessor: source %q must be feishu://{type}/{token}", source)
		}
		return docType, token, nil
	}
	// https://*.feishu.cn/docx/{token}
	u, err := url.Parse(source)
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("feishu accessor: invalid source %q", source)
	}
	path := strings.TrimPrefix(u.Path, "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("feishu accessor: source %q must be https://{host}/{type}/{token}", source)
	}
	return parts[0], parts[1], nil
}

// sanitizeFilename replaces path-unsafe characters with underscores so
// the token can be embedded in a temp-file name.
func sanitizeFilename(p string) string {
	p = strings.ReplaceAll(p, string(filepath.Separator), "_")
	p = strings.ReplaceAll(p, ":", "_")
	p = strings.ReplaceAll(p, "/", "_")
	return p
}

// newLarkClient constructs a *lark.Client with token caching, a custom
// base URL (for tests), and an optional HTTP transport override.
func newLarkClient(appID, appSecret, domain string, httpClient *http.Client) *lark.Client {
	opts := []lark.ClientOptionFunc{
		lark.WithEnableTokenCache(true),
	}
	if domain != "" {
		opts = append(opts, lark.WithOpenBaseUrl(domain))
	}
	if httpClient != nil {
		opts = append(opts, lark.WithHttpClient(coreHttpClient{httpClient}))
	}
	return lark.NewClient(appID, appSecret, opts...)
}

// coreHttpClient adapts a stdlib *http.Client to the larkcore.HttpClient
// interface.
type coreHttpClient struct{ c *http.Client }

func (h coreHttpClient) Do(req *http.Request) (*http.Response, error) {
	return h.c.Do(req)
}

// containsAny reports whether s contains any of the substrings.
func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if len(sub) == 0 {
			continue
		}
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}

// _ = larkcore.HttpClient keeps the larkcore import alive; the type is
// referenced through the WithHttpClient option in newLarkClient.
var _ larkcore.HttpClient = coreHttpClient{}
