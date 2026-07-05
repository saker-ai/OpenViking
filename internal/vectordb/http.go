package vectordb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// Doer is the minimal *http.Client surface used by HTTP-backed vector
// adapters (http, volcengine, vikingdb). It is satisfied by
// *http.Client and by httptest.Server.Client(); tests inject a stub
// to avoid network calls.
//
// Implementations must:
//   - Round-trip the provided *http.Request unchanged.
//   - Return a non-nil *http.Response whose Body the caller closes.
//   - Surface transport errors (DNS, timeout, refused) as a non-nil err.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// httpJSON is a tiny helper that POSTs a JSON body and decodes the JSON
// response into out. statusOK is the set of acceptable status codes; any
// other code is mapped to a *domain.AppError. The caller closes nothing —
// the response Body is fully consumed and closed here.
func httpJSON(ctx context.Context, doer Doer, method, url, apiKey string, body any, out any, statusOK ...int) error {
	if doer == nil {
		return domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("vectordb: nil HTTP doer"))
	}
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return domain.Wrap(domain.CodeVectorDBError, 500,
				fmt.Errorf("vectordb: marshal body: %w", err))
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("vectordb: build request %s %s: %w", method, url, err))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := doer.Do(req)
	if err != nil {
		return domain.Wrap(domain.CodeVectorDBError, 502,
			fmt.Errorf("vectordb: http %s %s: %w", method, url, err))
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	ok := len(statusOK) == 0
	for _, c := range statusOK {
		if resp.StatusCode == c {
			ok = true
			break
		}
	}
	if !ok {
		// Best-effort body snippet for debugging; truncate to keep logs sane.
		snippet := readSnippet(resp.Body, 512)
		switch resp.StatusCode {
		case http.StatusNotFound:
			return domain.Wrap(domain.CodeResourceNotFound, 404,
				fmt.Errorf("vectordb: http %s %s -> 404: %s", method, url, snippet))
		case http.StatusBadRequest:
			return domain.Wrap(domain.CodeValidationFailed, 422,
				fmt.Errorf("vectordb: http %s %s -> 400: %s", method, url, snippet))
		case http.StatusUnauthorized, http.StatusForbidden:
			return domain.Wrap(domain.CodeForbidden, 403,
				fmt.Errorf("vectordb: http %s %s -> %d: %s", method, url, resp.StatusCode, snippet))
		default:
			return domain.Wrap(domain.CodeVectorDBError, 502,
				fmt.Errorf("vectordb: http %s %s -> %d: %s", method, url, resp.StatusCode, snippet))
		}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		// A 2xx with no body is fine for some endpoints; surface EOF as success.
		if errors.Is(err, io.EOF) {
			return nil
		}
		return domain.Wrap(domain.CodeVectorDBError, 502,
			fmt.Errorf("vectordb: decode response %s %s: %w", method, url, err))
	}
	return nil
}

// readSnippet reads up to n bytes from r and returns a trimmed string.
// Used only to enrich error messages; failures return "<unreadable body>".
func readSnippet(r io.Reader, n int) string {
	if n <= 0 {
		n = 512
	}
	b := make([]byte, n)
	m, err := r.Read(b)
	if err != nil && m == 0 {
		return "<unreadable body>"
	}
	return strings.TrimSpace(string(b[:m]))
}

// newHTTPDoer returns a default *http.Client with a sane timeout. Adapters
// pass this to httpJSON when the caller didn't inject one.
func newHTTPDoer(timeout time.Duration) Doer {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{Timeout: timeout}
}

// HTTPAdapter is a CollectionAdapter that proxies every operation to a
// configurable HTTP REST API. It is useful for tests, proxies, and adapters
// to vector services not natively supported by OpenViking.
//
// Each operation maps to a configurable URL in HTTPVectorConfig. When a
// per-op URL is empty, the adapter falls back to URL + "/" + op (e.g.
// "http://example.com/api/vectordb/search"). All requests are POST with a
// JSON body defined by the httpReq* types below; responses use the
// httpResp* types. The wire format is documented inline.
//
// APIKey is sent as "Authorization: Bearer <api_key>" when non-empty.
type HTTPAdapter struct {
	doer Doer
	cfg  config.HTTPVectorConfig
}

// NewHTTPAdapter builds an HTTPAdapter from cfg. doer may be nil; a default
// *http.Client with a 30s timeout is used. The factory passes the resolved
// per-op URLs.
func NewHTTPAdapter(_ context.Context, cfg config.HTTPVectorConfig, doer Doer) (*HTTPAdapter, error) {
	if cfg.URL == "" && firstNonEmpty(
		cfg.EnsureCollectionURL, cfg.DropCollectionURL, cfg.ListCollectionsURL,
		cfg.UpsertURL, cfg.SearchURL, cfg.DeleteURL, cfg.GetURL, cfg.CountURL,
	) == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("vectordb: http backend requires at least one URL"))
	}
	if doer == nil {
		doer = newHTTPDoer(30 * time.Second)
	}
	return &HTTPAdapter{doer: doer, cfg: cfg}, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// opURL returns cfg.<op>URL if set, else URL + "/" + op.
func (a *HTTPAdapter) opURL(op, override string) string {
	if override != "" {
		return override
	}
	if a.cfg.URL == "" {
		return ""
	}
	return strings.TrimRight(a.cfg.URL, "/") + "/" + op
}

// --- Wire format ---
//
// All request/response types are exported so external HTTP services can
// implement the matching shape. Field names use snake_case to match the
// JSON tags; structs are flat so a 1:1 curl example is straightforward.

// httpReqEnsure is the body for EnsureCollection.
type httpReqEnsure struct {
	Collection string `json:"collection"`
	Dim        int    `json:"dim"`
	Distance   string `json:"distance,omitempty"`
}

// httpRespEmpty is the generic "ok" response with no payload.
type httpRespEmpty struct {
	OK bool `json:"ok"`
}

// httpReqDrop is the body for DropCollection.
type httpReqDrop struct {
	Collection string `json:"collection"`
}

// httpRespList is the response for ListCollections.
type httpRespList struct {
	Collections []string `json:"collections"`
}

// httpReqUpsert is the body for Upsert.
type httpReqUpsert struct {
	Collection string   `json:"collection"`
	Rows       []Vector `json:"rows"`
}

// httpReqDelete is the body for Delete.
type httpReqDelete struct {
	Collection string   `json:"collection"`
	IDs        []string `json:"ids"`
}

// httpReqSearch is the body for Search.
type httpReqSearch struct {
	Collection string      `json:"collection"`
	Query      []float32   `json:"query"`
	TopK       int         `json:"top_k"`
	Filter     Filter      `json:"filter"`
}

// httpRespSearch is the response for Search.
type httpRespSearch struct {
	Hits []Vector `json:"hits"`
}

// httpReqGet is the body for Get.
type httpReqGet struct {
	Collection string `json:"collection"`
	ID         string `json:"id"`
}

// httpRespGet is the response for Get.
type httpRespGet struct {
	Vector Vector `json:"vector"`
}

// httpReqCount is the body for Count.
type httpReqCount struct {
	Collection string `json:"collection"`
}

// httpRespCount is the response for Count.
type httpRespCount struct {
	Count int64 `json:"count"`
}

// EnsureCollection POSTs to the configured ensure_collection_url.
func (a *HTTPAdapter) EnsureCollection(ctx context.Context, schema CollectionSchema) error {
	if err := validateSchema(schema); err != nil {
		return err
	}
	dist := schema.Distance
	if dist == "" {
		dist = "cosine"
	}
	body := httpReqEnsure{Collection: schema.Name, Dim: schema.Dim, Distance: dist}
	return httpJSON(ctx, a.doer, http.MethodPost, a.opURL("ensure_collection", a.cfg.EnsureCollectionURL),
		a.cfg.APIKey, body, nil, 200, 201, 204)
}

// DropCollection POSTs to the configured drop_collection_url.
func (a *HTTPAdapter) DropCollection(ctx context.Context, name string) error {
	body := httpReqDrop{Collection: name}
	return httpJSON(ctx, a.doer, http.MethodPost, a.opURL("drop_collection", a.cfg.DropCollectionURL),
		a.cfg.APIKey, body, nil, 200, 202, 204, 404)
}

// ListCollections POSTs to the configured list_collections_url.
func (a *HTTPAdapter) ListCollections(ctx context.Context) ([]string, error) {
	var resp httpRespList
	if err := httpJSON(ctx, a.doer, http.MethodPost, a.opURL("list_collections", a.cfg.ListCollectionsURL),
		a.cfg.APIKey, struct{}{}, &resp, 200); err != nil {
		return nil, err
	}
	return resp.Collections, nil
}

// Upsert POSTs to the configured upsert_url.
func (a *HTTPAdapter) Upsert(ctx context.Context, collection string, rows []Vector) error {
	if collection == "" {
		return domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty collection name"))
	}
	if len(rows) == 0 {
		return nil
	}
	body := httpReqUpsert{Collection: collection, Rows: rows}
	return httpJSON(ctx, a.doer, http.MethodPost, a.opURL("upsert", a.cfg.UpsertURL),
		a.cfg.APIKey, body, nil, 200, 201, 204)
}

// Delete POSTs to the configured delete_url.
func (a *HTTPAdapter) Delete(ctx context.Context, collection string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	body := httpReqDelete{Collection: collection, IDs: ids}
	return httpJSON(ctx, a.doer, http.MethodPost, a.opURL("delete", a.cfg.DeleteURL),
		a.cfg.APIKey, body, nil, 200, 202, 204, 404)
}

// Search POSTs to the configured search_url.
func (a *HTTPAdapter) Search(ctx context.Context, params SearchParams) (*SearchResult, error) {
	if len(params.Query) == 0 {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty query vector"))
	}
	topK := params.TopK
	if topK <= 0 {
		topK = 10
	}
	body := httpReqSearch{
		Collection: params.Collection,
		Query:      append([]float32(nil), params.Query...),
		TopK:       topK,
		Filter:     params.Filter,
	}
	var resp httpRespSearch
	if err := httpJSON(ctx, a.doer, http.MethodPost, a.opURL("search", a.cfg.SearchURL),
		a.cfg.APIKey, body, &resp, 200); err != nil {
		return nil, err
	}
	return &SearchResult{Hits: resp.Hits}, nil
}

// Get POSTs to the configured get_url.
func (a *HTTPAdapter) Get(ctx context.Context, collection, id string) (*Vector, error) {
	if id == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty id"))
	}
	body := httpReqGet{Collection: collection, ID: id}
	var resp httpRespGet
	if err := httpJSON(ctx, a.doer, http.MethodPost, a.opURL("get", a.cfg.GetURL),
		a.cfg.APIKey, body, &resp, 200); err != nil {
		return nil, err
	}
	return &resp.Vector, nil
}

// Count POSTs to the configured count_url.
func (a *HTTPAdapter) Count(ctx context.Context, collection string) (int64, error) {
	body := httpReqCount{Collection: collection}
	var resp httpRespCount
	if err := httpJSON(ctx, a.doer, http.MethodPost, a.opURL("count", a.cfg.CountURL),
		a.cfg.APIKey, body, &resp, 200); err != nil {
		return 0, err
	}
	return resp.Count, nil
}

// Close is a no-op for HTTP — there is no persistent connection.
func (a *HTTPAdapter) Close() error { return nil }
