package vectordb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/volcengine/volc-sdk-golang/base"
	vikingdb "github.com/volcengine/volc-sdk-golang/service/vikingdb"
	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// VikingDBAdapter is a CollectionAdapter backed by Volcengine VikingDB via
// the volc-sdk-golang/service/vikingdb SDK. The SDK ships an internal V4
// HMAC-SHA256 signer (service component "air"), so this adapter delegates
// all signing and HTTP transport to *base.Client. Tests inject a custom
// *http.Client via withHTTPClient to point at httptest.NewServer.
//
// Every data operation targets a (collection, index) pair; the index name is
// derived as "{collection}_idx" so callers see a single namespace.
//
// SDK errors carry the HTTP status and raw body in their Error() string
// (base/client.go makeRequest format: "api <api> http code <status> body
// <body>"). classifyVikingDBError parses that to map VikingDB's structured
// {"ResponseMetadata":{"Error":{CodeN,Code,Message}}} envelope onto
// domain.* sentinels.
type VikingDBAdapter struct {
	svc    *vikingdb.VikingDBService
	prefix string
	host   string
	region string
	scheme string
}

// NewVikingDBAdapter validates config and returns an adapter. It does NOT
// dial the API. We build *base.Client directly (rather than the SDK's
// NewVikingDBService) to skip its panicking Ping probe.
func NewVikingDBAdapter(_ context.Context, cfg config.VikingDBConfig, prefix string) (*VikingDBAdapter, error) {
	if cfg.AccessKey == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("vectordb: vikingdb access_key is empty"))
	}
	if cfg.SecretKey == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("vectordb: vikingdb secret_key is empty"))
	}
	host := cfg.Host
	if host == "" {
		host = "api-vikingdb.volces.com"
	}
	region := cfg.Region
	if region == "" {
		region = "cn-beijing"
	}
	scheme := "https"
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		if u.Scheme != "" {
			scheme = u.Scheme
		}
		host = u.Host
	}
	return &VikingDBAdapter{
		svc:    newVikingDBService(host, region, cfg.AccessKey, cfg.SecretKey, scheme),
		prefix: prefix, host: host, region: region, scheme: scheme,
	}, nil
}

// withHTTPClient replaces the SDK client's *http.Client. Used by tests.
func (a *VikingDBAdapter) withHTTPClient(c *http.Client) *VikingDBAdapter {
	if c != nil && a.svc != nil && a.svc.Client != nil {
		a.svc.Client.Client = c
	}
	return a
}

func (a *VikingDBAdapter) indexNameFor(collection string) string {
	return collection + "_idx"
}

// EnsureCollection creates the VikingDB collection (string PK "id", float32
// vector field "vector" of dim N) plus an HNSW index. Idempotent.
func (a *VikingDBAdapter) EnsureCollection(ctx context.Context, schema CollectionSchema) error {
	if err := validateSchema(schema); err != nil {
		return err
	}
	dist := schema.Distance
	if dist == "" {
		dist = "cosine"
	}
	createBody := map[string]any{
		"collection_name": schema.Name,
		"description":     "",
		"primary_key":     "id",
		"fields": []map[string]any{
			{FieldName: "id", FieldType: "string"},
			{FieldName: "vector", FieldType: "float32", Dim: schema.Dim},
		},
	}
	if _, err := a.do(ctx, "CreateCollection", createBody); err != nil {
		if isAlreadyExists(err) {
			return nil
		}
		return err
	}
	idxBody := map[string]any{
		"collection_name": schema.Name,
		"index_name":      a.indexNameFor(schema.Name),
		"cpu_quota":       2,
		"description":     "",
		"vector_index": map[string]any{
			"index_type": "HNSW", "distance": dist,
			"hnsw_m": 16, "hnsw_cef": 200, "hnsw_sef": 200,
		},
	}
	if _, err := a.do(ctx, "CreateIndex", idxBody); err != nil {
		if isAlreadyExists(err) {
			return nil
		}
		return err
	}
	return nil
}

// DropCollection removes the collection and its index. Missing = success.
func (a *VikingDBAdapter) DropCollection(ctx context.Context, name string) error {
	if _, err := a.do(ctx, "DropCollection", map[string]any{"collection_name": name}); err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

// ListCollections returns collection names filtered by the configured prefix.
func (a *VikingDBAdapter) ListCollections(ctx context.Context) ([]string, error) {
	res, err := a.do(ctx, "ListCollections", map[string]any{})
	if err != nil {
		return nil, err
	}
	data, _ := res["data"].([]any)
	out := make([]string, 0, len(data))
	for _, c := range data {
		m, _ := c.(map[string]any)
		name, _ := m["collection_name"].(string)
		if name == "" {
			continue
		}
		if a.prefix == "" || strings.HasPrefix(name, a.prefix) {
			out = append(out, name)
		}
	}
	return out, nil
}

// Upsert inserts or replaces rows by ID. Embedding -> "vector" field,
// Metadata merged into the row.
func (a *VikingDBAdapter) Upsert(ctx context.Context, collection string, rows []Vector) error {
	if collection == "" {
		return domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty collection name"))
	}
	if len(rows) == 0 {
		return nil
	}
	fields := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if r.ID == "" {
			return domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: upsert row missing id"))
		}
		row := map[string]any{"id": r.ID, "vector": r.Embedding}
		for k, v := range r.Metadata {
			if k != "id" && k != "vector" {
				row[k] = v
			}
		}
		fields = append(fields, row)
	}
	_, err := a.do(ctx, "UpsertData", map[string]any{"collection_name": collection, "fields": fields})
	return err
}

// Delete removes rows by ID. Unknown IDs are ignored.
func (a *VikingDBAdapter) Delete(ctx context.Context, collection string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := a.do(ctx, "DeleteData", map[string]any{"collection_name": collection, "primary_keys": ids})
	return err
}

// Search returns TopK rows nearest to Query, filtered by Filter.
func (a *VikingDBAdapter) Search(ctx context.Context, params SearchParams) (*SearchResult, error) {
	if len(params.Query) == 0 {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty query vector"))
	}
	topK := params.TopK
	if topK <= 0 {
		topK = 10
	}
	search := map[string]any{
		"order_by_vector": map[string]any{"vectors": [][]float32{append([]float32(nil), params.Query...)}},
		"limit":           topK, "partition": "", "output_fields": []string{"*"},
	}
	if buildVikingDBFilter(params.Filter) != "" {
		search["filter"] = map[string]any{"operator": "AND", "conditions": parseFilterConditions(params.Filter)}
	}
	res, err := a.do(ctx, "SearchIndex", map[string]any{
		"collection_name": params.Collection,
		"index_name":      a.indexNameFor(params.Collection),
		"search":          search,
	})
	if err != nil {
		return nil, err
	}
	data, _ := res["data"].([]any)
	hits := make([]Vector, 0, len(data))
	for _, item := range data {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		v := Vector{}
		if id, ok := m["id"].(string); ok {
			v.ID = id
		}
		if vec, ok := m["vector"].([]any); ok {
			v.Embedding = anySliceToFloat32(vec)
		}
		md := make(map[string]any, len(m))
		for k, val := range m {
			if k != "id" && k != "vector" {
				md[k] = val
			}
		}
		v.Metadata = md
		if s, ok := m["score"]; ok {
			if f, err := toFloat32(s); err == nil {
				v.Score = f
			}
		}
		hits = append(hits, v)
	}
	sortByScoreDesc(hits)
	return &SearchResult{Hits: hits}, nil
}

// Get fetches a single row by ID. Returns domain.ErrNotFound if absent.
func (a *VikingDBAdapter) Get(ctx context.Context, collection, id string) (*Vector, error) {
	if id == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty id"))
	}
	res, err := a.do(ctx, "FetchData", map[string]any{"collection_name": collection, "primary_keys": id})
	if err != nil {
		if isNotFound(err) {
			return nil, domain.Wrap(domain.CodeResourceNotFound, 404,
				fmt.Errorf("vectordb: vikingdb get %s/%s: %w", collection, id, domain.ErrNotFound))
		}
		return nil, err
	}
	data, _ := res["data"].([]any)
	if len(data) == 0 {
		return nil, domain.Wrap(domain.CodeResourceNotFound, 404,
			fmt.Errorf("vectordb: vikingdb get %s/%s: not found", collection, id))
	}
	m, _ := data[0].(map[string]any)
	if m == nil {
		return nil, domain.Wrap(domain.CodeResourceNotFound, 404,
			fmt.Errorf("vectordb: vikingdb get %s/%s: not found", collection, id))
	}
	v := Vector{}
	if s, ok := m["id"].(string); ok {
		v.ID = s
	} else {
		v.ID = id
	}
	if vec, ok := m["vector"].([]any); ok {
		v.Embedding = anySliceToFloat32(vec)
	}
	md := make(map[string]any, len(m))
	for k, val := range m {
		if k != "id" && k != "vector" {
			md[k] = val
		}
	}
	v.Metadata = md
	return &v, nil
}

// Count returns the row count via GetCollection's stat.total_count.
func (a *VikingDBAdapter) Count(ctx context.Context, collection string) (int64, error) {
	res, err := a.do(ctx, "GetCollection", map[string]any{"collection_name": collection})
	if err != nil {
		if isNotFound(err) {
			return 0, nil
		}
		return 0, err
	}
	data, _ := res["data"].(map[string]any)
	if data == nil {
		return 0, nil
	}
	stat, _ := data["stat"].(map[string]any)
	if stat == nil {
		return 0, nil
	}
	switch n := stat["total_count"].(type) {
	case float64:
		return int64(n), nil
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, nil
		}
	case string:
		var i int64
		if _, err := fmt.Sscanf(n, "%d", &i); err == nil {
			return i, nil
		}
	}
	return 0, nil
}

// Close is a no-op; the SDK client has no persistent connection.
func (a *VikingDBAdapter) Close() error { return nil }

// FieldName / FieldType are the JSON keys VikingDB expects in field descriptors.
const (
	FieldName = "field_name"
	FieldType = "field_type"
	Dim       = "dim"
)

// vikingDBServiceName is the V4 credential-scope service component for
// VikingDB. The reference SDK uses "air", not "vikingdb".
const vikingDBServiceName = "air"

// do is the single choke-point for SDK calls. Marshals body, delegates to
// the SDK's DoRequest (signs + sends), and maps errors to *domain.AppError.
func (a *VikingDBAdapter) do(ctx context.Context, api string, body any) (map[string]any, error) {
	if a.svc == nil {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("vectordb: nil vikingdb service"))
	}
	jsonBody := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, domain.Wrap(domain.CodeVectorDBError, 500,
				fmt.Errorf("vectordb: marshal body: %w", err))
		}
		jsonBody = string(b)
	}
	res, err := a.svc.DoRequest(ctx, api, nil, jsonBody)
	if err != nil {
		return nil, classifyVikingDBError(api, err)
	}
	return res, nil
}

// newVikingDBService builds *vikingdb.VikingDBService without the SDK's
// panicking Ping probe (we construct *base.Client directly).
func newVikingDBService(host, region, ak, sk, scheme string) *vikingdb.VikingDBService {
	info := &base.ServiceInfo{
		Timeout: 30 * time.Second, Scheme: scheme, Host: host,
		Header: http.Header{"Host": []string{host}},
		Credentials: base.Credentials{
			AccessKeyID: ak, SecretAccessKey: sk,
			Service: vikingDBServiceName, Region: region,
		},
	}
	return &vikingdb.VikingDBService{Client: base.NewClient(info, vikingDBAPIInfo())}
}

// vikingDBAPIInfo returns the API info table for the endpoints we use.
func vikingDBAPIInfo() map[string]*base.ApiInfo {
	h := http.Header{"Accept": []string{"application/json"}, "Content-Type": []string{"application/json"}}
	get := func(p string) *base.ApiInfo { return &base.ApiInfo{Method: http.MethodGet, Path: p, Header: h.Clone()} }
	post := func(p string) *base.ApiInfo { return &base.ApiInfo{Method: http.MethodPost, Path: p, Header: h.Clone()} }
	return map[string]*base.ApiInfo{
		"CreateCollection": post("/api/collection/create"),
		"DropCollection":   post("/api/collection/drop"),
		"ListCollections":  get("/api/collection/list"),
		"GetCollection":    get("/api/collection/info"),
		"CreateIndex":      post("/api/index/create"),
		"UpsertData":       post("/api/collection/upsert_data"),
		"DeleteData":       post("/api/collection/del_data"),
		"FetchData":        get("/api/collection/fetch_data"),
		"SearchIndex":      post("/api/index/search"),
	}
}

// classifyVikingDBError maps an SDK error to a *domain.AppError. SDK errors
// look like: "api <api> http code <status> body <body>". We classify by
// substring matching on the error message (which includes the raw body) and
// by HTTP status code.
func classifyVikingDBError(api string, err error) error {
	if err == nil {
		return nil
	}
	wrapped := fmt.Errorf("vectordb: %s: %w", api, err)
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "already exist") || strings.Contains(msg, "exists"):
		return domain.Wrap(domain.CodeConflict, 409, wrapped)
	case strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist"):
		return domain.Wrap(domain.CodeResourceNotFound, 404, wrapped)
	case strings.Contains(msg, "http code 400"):
		return domain.Wrap(domain.CodeValidationFailed, 422, wrapped)
	case strings.Contains(msg, "http code 401") || strings.Contains(msg, "http code 403"):
		return domain.Wrap(domain.CodeForbidden, 403, wrapped)
	default:
		return domain.Wrap(domain.CodeVectorDBError, 502, wrapped)
	}
}

func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exist") || strings.Contains(msg, "exists")
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist")
}

// anySliceToFloat32 converts []any (from JSON) to []float32.
func anySliceToFloat32(in []any) []float32 {
	out := make([]float32, 0, len(in))
	for _, v := range in {
		if f, err := toFloat32(v); err == nil {
			out = append(out, f)
		}
	}
	return out
}

func toFloat32(v any) (float32, error) {
	switch n := v.(type) {
	case float64:
		return float32(n), nil
	case float32:
		return n, nil
	case int:
		return float32(n), nil
	case int64:
		return float32(n), nil
	case json.Number:
		f, err := n.Float64()
		return float32(f), err
	}
	return 0, fmt.Errorf("vectordb: not a number: %T", v)
}

// parseFilterConditions builds a VikingDB filter conditions list.
func parseFilterConditions(f Filter) []map[string]any {
	var conds []map[string]any
	if f.Account != "" {
		conds = append(conds, map[string]any{"field": "account", "operator": "=", "value": f.Account})
	}
	if f.Kind != "" {
		conds = append(conds, map[string]any{"field": "kind", "operator": "=", "value": f.Kind})
	}
	if f.URIPrefix != "" {
		conds = append(conds, map[string]any{"field": "uri", "operator": "prefix", "value": f.URIPrefix})
	}
	for k, v := range f.Metadata {
		conds = append(conds, map[string]any{"field": k, "operator": "=", "value": v})
	}
	return conds
}

// buildVikingDBFilter returns "filter" when the filter has conditions.
func buildVikingDBFilter(f Filter) string {
	if f.Account == "" && f.Kind == "" && f.URIPrefix == "" && len(f.Metadata) == 0 {
		return ""
	}
	return "filter"
}
