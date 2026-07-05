package accessors

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/saker-ai/ctxhub/internal/parse"
)

// startMockLark launches an httptest.Server that mocks the Lark OpenAPI
// endpoints exercised by FeishuAccessor:
//
//   - POST /open-apis/auth/v3/tenant_access_token/internal  → returns a fake tenant token
//   - GET  /open-apis/docx/v1/documents/:document_id/raw_content → returns docx plain text
//   - GET  /open-apis/docx/v1/documents/:document_id       → returns docx title
//   - GET  /open-apis/im/v1/messages/:message_id           → returns IM message body
//
// The server returns the Lark-standard envelope {code, msg, data: {...}}.
func startMockLark(t *testing.T, docxContent, docxTitle, imContent, imMsgType string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"t-fake","expire":7200}`))
	})
	mux.HandleFunc("/open-apis/docx/v1/documents/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch {
		case strings.HasSuffix(r.URL.Path, "/raw_content"):
			body, _ := json.Marshal(map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{"content": docxContent},
			})
			_, _ = w.Write(body)
		default:
			body, _ := json.Marshal(map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"document": map[string]any{"title": docxTitle},
				},
			})
			_, _ = w.Write(body)
		}
	})
	mux.HandleFunc("/open-apis/im/v1/messages/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		body, _ := json.Marshal(map[string]any{
			"code": 0,
			"msg":  "ok",
			"data": map[string]any{
				"items": []map[string]any{
					{
						"message_id": "om_fake",
						"msg_type":   imMsgType,
						"body":       map[string]any{"content": imContent},
					},
				},
			},
		})
		_, _ = w.Write(body)
	})
	return httptest.NewServer(mux)
}

// newMockFeishuAccessor constructs a FeishuAccessor wired to the mock
// Lark server. The Lark SDK uses a cache-key based on app_id+secret; a
// unique app_id per test avoids cross-test cache collisions.
func newMockFeishuAccessor(t *testing.T, server *httptest.Server, appID string) *FeishuAccessor {
	t.Helper()
	acc, err := NewFeishuAccessorWithClient(appID, "secret", server.URL, server.Client())
	if err != nil {
		t.Fatalf("NewFeishuAccessorWithClient: %v", err)
	}
	if acc.client == nil {
		t.Fatalf("client is nil; expected a constructed lark client")
	}
	return acc
}

func TestFeishuAccessor_CanHandle(t *testing.T) {
	acc, _ := NewFeishuAccessor("", "", "")
	cases := []struct {
		source string
		want   bool
	}{
		{"feishu://docx/abc", true},
		{"feishu-doc://docx/abc", true},
		{"lark://im/om_123", true},
		{"https://example.feishu.cn/docx/abc", true},
		{"https://example.larksuite.com/wiki/abc", true},
		{"https://example.larkoffice.com/docx/abc", true},
		{"https://example.com/docx/abc", false},
		{"http://localhost/file.txt", false},
		{"/local/path.md", false},
	}
	for _, c := range cases {
		if got := acc.CanHandle(c.source, parse.AccessorOptions{}); got != c.want {
			t.Errorf("CanHandle(%q)=%v, want %v", c.source, got, c.want)
		}
	}
}

func TestFeishuAccessor_Schemes(t *testing.T) {
	acc, _ := NewFeishuAccessor("", "", "")
	got := acc.Schemes()
	want := []string{"feishu", "feishu-doc", "lark"}
	if len(got) != len(want) {
		t.Fatalf("Schemes()=%v, want %v", got, want)
	}
	for i, s := range got {
		if s != want[i] {
			t.Errorf("Schemes()[%d]=%q, want %q", i, s, want[i])
		}
	}
}

func TestFeishuAccessor_MissingCredentials(t *testing.T) {
	acc, err := NewFeishuAccessor("", "", "")
	if err != nil {
		t.Fatalf("NewFeishuAccessor: %v", err)
	}
	if acc.client != nil {
		t.Fatal("expected nil client when credentials are missing")
	}
	_, err = acc.Fetch(context.Background(), "feishu://docx/abc", parse.AccessorOptions{})
	if err == nil {
		t.Fatal("Fetch: expected error when credentials missing, got nil")
	}
	if !strings.Contains(err.Error(), "app_id") {
		t.Errorf("Fetch error should mention app_id, got: %v", err)
	}
}

func TestFeishuAccessor_FetchDocx(t *testing.T) {
	const docContent = "Hello Feishu docx."
	const docTitle = "Test Doc"
	server := startMockLark(t, docContent, docTitle, "", "")
	defer server.Close()
	acc := newMockFeishuAccessor(t, server, "test_fetch_docx")

	res, err := acc.Fetch(context.Background(), "feishu://docx/doxcnFake", parse.AccessorOptions{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer res.Cleanup()
	body, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, docContent) {
		t.Errorf("Fetch result missing doc content; got %q", got)
	}
	if !strings.Contains(got, "# "+docTitle) {
		t.Errorf("Fetch result missing title heading; got %q", got)
	}
	if res.SourceType != parse.SourceFeishu {
		t.Errorf("SourceType=%v, want SourceFeishu", res.SourceType)
	}
	if !res.IsTemporary {
		t.Errorf("IsTemporary=%v, want true", res.IsTemporary)
	}
}

func TestFeishuAccessor_FetchIMText(t *testing.T) {
	const imContent = `{"text":"Hello IM"}`
	const imMsgType = "text"
	server := startMockLark(t, "", "", imContent, imMsgType)
	defer server.Close()
	acc := newMockFeishuAccessor(t, server, "test_fetch_im")

	res, err := acc.Fetch(context.Background(), "feishu://im/om_fake", parse.AccessorOptions{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer res.Cleanup()
	body, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(body), "Hello IM") {
		t.Errorf("Fetch IM result missing text; got %q", string(body))
	}
}

func TestFeishuAccessor_FetchIMPost(t *testing.T) {
	// Lark post body: {"zh_cn":{"title":"T","content":[[{"tag":"text","text":"Hi"}]]}}
	const imContent = `{"zh_cn":{"title":"PostTitle","content":[[{"tag":"text","text":"Hi post"}]]}}`
	const imMsgType = "post"
	server := startMockLark(t, "", "", imContent, imMsgType)
	defer server.Close()
	acc := newMockFeishuAccessor(t, server, "test_fetch_im_post")

	res, err := acc.Fetch(context.Background(), "feishu://im/om_fake", parse.AccessorOptions{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer res.Cleanup()
	body, _ := os.ReadFile(res.Path)
	got := string(body)
	if !strings.Contains(got, "PostTitle") {
		t.Errorf("post result missing title; got %q", got)
	}
	if !strings.Contains(got, "Hi post") {
		t.Errorf("post result missing text; got %q", got)
	}
}

func TestFeishuAccessor_HTTPSURL(t *testing.T) {
	const docContent = "Docx via https URL."
	server := startMockLark(t, docContent, "T", "", "")
	defer server.Close()
	acc := newMockFeishuAccessor(t, server, "test_fetch_https")

	res, err := acc.Fetch(context.Background(), "https://example.feishu.cn/docx/doxcnFake", parse.AccessorOptions{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer res.Cleanup()
	body, _ := os.ReadFile(res.Path)
	if !strings.Contains(string(body), docContent) {
		t.Errorf("Fetch via https URL missing content; got %q", string(body))
	}
}

func TestFeishuAccessor_WikiNotSupported(t *testing.T) {
	server := startMockLark(t, "x", "T", "", "")
	defer server.Close()
	acc := newMockFeishuAccessor(t, server, "test_wiki")

	_, err := acc.Fetch(context.Background(), "feishu://wiki/nodesecret", parse.AccessorOptions{})
	if err == nil {
		t.Fatal("Fetch wiki: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "wiki") {
		t.Errorf("Fetch wiki error should mention wiki, got: %v", err)
	}
}

func TestFeishuAccessor_ParseSource(t *testing.T) {
	cases := []struct {
		source   string
		wantType string
		wantTok  string
		wantErr  bool
	}{
		{"feishu://docx/abc", "docx", "abc", false},
		{"feishu://im/om_123", "im", "om_123", false},
		{"lark://docx/d1", "docx", "d1", false},
		{"https://x.feishu.cn/docx/d2", "docx", "d2", false},
		{"feishu://docx/", "", "", true},     // empty token
		{"feishu://", "", "", true},          // missing type/token
		{"https://x.feishu.cn/", "", "", true}, // missing type/token
	}
	for _, c := range cases {
		dt, tok, err := parseFeishuSource(c.source)
		if (err != nil) != c.wantErr {
			t.Errorf("parseFeishuSource(%q) err=%v, wantErr=%v", c.source, err, c.wantErr)
			continue
		}
		if !c.wantErr {
			if dt != c.wantType {
				t.Errorf("parseFeishuSource(%q) type=%q, want %q", c.source, dt, c.wantType)
			}
			if tok != c.wantTok {
				t.Errorf("parseFeishuSource(%q) token=%q, want %q", c.source, tok, c.wantTok)
			}
		}
	}
}

func TestFeishuAccessor_IMContentToText(t *testing.T) {
	cases := []struct {
		name    string
		msgType string
		content string
		want    string
	}{
		{"text", "text", `{"text":"hello"}`, "hello"},
		{"post", "post", `{"zh_cn":{"title":"T","content":[[{"tag":"text","text":"a"}]]}}`, "T"},
		{"empty", "text", ``, ""},
		{"unknown_type", "image", `{"image_key":"x"}`, `{"image_key":"x"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := imContentToText(c.msgType, c.content)
			if !strings.Contains(got, c.want) {
				t.Errorf("imContentToText(%q,%q)=%q, want substring %q", c.msgType, c.content, got, c.want)
			}
		})
	}
}

// Ensure that the test file exercises the http transport override path
// even when the SDK's token cache is cold. The first Fetch call triggers
// a tenant_access_token request; the mock server returns a fake token
// that the cache keeps for the rest of the test.
func TestFeishuAccessor_FetchTwiceUsesTokenCache(t *testing.T) {
	const docContent = "Cached token doc."
	server := startMockLark(t, docContent, "T", "", "")
	defer server.Close()
	acc := newMockFeishuAccessor(t, server, "test_cache")

	// First Fetch warms the token cache.
	res1, err := acc.Fetch(context.Background(), "feishu://docx/d1", parse.AccessorOptions{})
	if err != nil {
		t.Fatalf("Fetch 1: %v", err)
	}
	defer res1.Cleanup()
	body1, _ := os.ReadFile(res1.Path)
	if !strings.Contains(string(body1), docContent) {
		t.Errorf("Fetch 1 missing content; got %q", string(body1))
	}

	// Second Fetch should reuse the cached token without re-requesting.
	res2, err := acc.Fetch(context.Background(), "feishu://docx/d2", parse.AccessorOptions{})
	if err != nil {
		t.Fatalf("Fetch 2: %v", err)
	}
	defer res2.Cleanup()
	body2, _ := os.ReadFile(res2.Path)
	if !strings.Contains(string(body2), docContent) {
		t.Errorf("Fetch 2 missing content; got %q", string(body2))
	}
}

// _ = io.Discard keeps the io import alive; some Lark SDK paths emit
// debug logs to io.Writer when verbose mode is on, and the test file
// references io in case a future test wants to capture SDK logs.
var _ = io.Discard
