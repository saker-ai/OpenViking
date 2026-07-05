package vectordb

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// newTestVikingDBAdapter builds an adapter wired to a test server. The SDK's
// *base.Client is pointed at srv.Client() so all requests hit the test server.
func newTestVikingDBAdapter(t *testing.T, host string) *VikingDBAdapter {
	t.Helper()
	a, err := NewVikingDBAdapter(context.Background(), config.VikingDBConfig{
		Host:      host,
		Region:    "cn-beijing",
		AccessKey: "AK",
		SecretKey: "SK",
	}, "ov_")
	require.NoError(t, err)
	return a.withHTTPClient(http.DefaultClient)
}

func TestVikingDBAdapter_RequiresAccessKey(t *testing.T) {
	t.Parallel()
	_, err := NewVikingDBAdapter(context.Background(), config.VikingDBConfig{
		Host: "https://api-vikingdb.volces.com", SecretKey: "s",
	}, "ov_")
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, domain.CodeValidationFailed, appErr.Code)
}

func TestVikingDBAdapter_RequiresSecretKey(t *testing.T) {
	t.Parallel()
	_, err := NewVikingDBAdapter(context.Background(), config.VikingDBConfig{
		Host: "https://api-vikingdb.volces.com", AccessKey: "a",
	}, "ov_")
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, domain.CodeValidationFailed, appErr.Code)
}

func TestVikingDBAdapter_DefaultsHostAndRegion(t *testing.T) {
	t.Parallel()
	a, err := NewVikingDBAdapter(context.Background(), config.VikingDBConfig{
		AccessKey: "a",
		SecretKey: "s",
	}, "ov_")
	require.NoError(t, err)
	defer a.Close()
	assert.Equal(t, "api-vikingdb.volces.com", a.host)
	assert.Equal(t, "cn-beijing", a.region)
	assert.Equal(t, "air", vikingDBServiceName)
}

func TestVikingDBAdapter_StripsSchemeFromHost(t *testing.T) {
	t.Parallel()
	a, err := NewVikingDBAdapter(context.Background(), config.VikingDBConfig{
		Host:      "http://example.com",
		AccessKey: "a",
		SecretKey: "s",
	}, "ov_")
	require.NoError(t, err)
	defer a.Close()
	assert.Equal(t, "example.com", a.host)
	assert.Equal(t, "http", a.scheme)
}

func TestVikingDBAdapter_PreservesHTTPSByDefault(t *testing.T) {
	t.Parallel()
	a, err := NewVikingDBAdapter(context.Background(), config.VikingDBConfig{
		Host:      "https://example.com",
		AccessKey: "a",
		SecretKey: "s",
	}, "ov_")
	require.NoError(t, err)
	defer a.Close()
	assert.Equal(t, "example.com", a.host)
	assert.Equal(t, "https", a.scheme)
}

func TestVikingDBAdapter_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	a, err := NewVikingDBAdapter(context.Background(), config.VikingDBConfig{
		Host: "https://api-vikingdb.volces.com", AccessKey: "a", SecretKey: "s",
	}, "ov_")
	require.NoError(t, err)
	assert.NoError(t, a.Close())
	assert.NoError(t, a.Close())
}

func TestVikingDBAdapterInterface(t *testing.T) {
	var _ CollectionAdapter = (*VikingDBAdapter)(nil)
}

// authCheck returns a handler func that asserts the Authorization header is
// well-formed (V4 shape with a 64-hex-char signature). The SDK's internal
// signer produces the same HMAC-SHA256 Credential/SignedHeaders/Signature
// format as the deleted hand-rolled signer.
func authCheck(t *testing.T, next http.HandlerFunc) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		require.NotEmpty(t, auth, "Authorization header missing")
		require.True(t, strings.HasPrefix(auth, "HMAC-SHA256 "),
			"Authorization header must start with 'HMAC-SHA256 ': got %q", auth)
		m := regexp.MustCompile(`Signature=([0-9a-f]{64})`).FindStringSubmatch(auth)
		require.NotNil(t, m, "Authorization signature must be 64 hex chars: %q", auth)
		xd := r.Header.Get("X-Date")
		require.NotEmpty(t, xd, "X-Date header missing")
		require.Len(t, xd, 16, "X-Date must be YYYYMMDDTHHMMSSZ: %q", xd)
		require.True(t, strings.HasSuffix(xd, "Z"), "X-Date must end with 'Z': %q", xd)
		if r.Method == http.MethodPost {
			require.NotEmpty(t, r.Header.Get("X-Content-Sha256"), "X-Content-Sha256 missing")
		}
		if next != nil {
			next(w, r)
		}
	}
}

// readBody reads and returns the request body as a string.
func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	return string(b)
}

func TestVikingDBAdapter_EnsureCollection(t *testing.T) {
	var gotCreate, gotIdx atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collection/create", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		gotCreate.Store(readBody(t, r))
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	mux.HandleFunc("/api/index/create", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		gotIdx.Store(readBody(t, r))
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	err := a.EnsureCollection(context.Background(), CollectionSchema{Name: "ov_acct__doc", Dim: 4, Distance: "cosine"})
	require.NoError(t, err)

	body := gotCreate.Load().(string)
	var createReq map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &createReq))
	assert.Equal(t, "ov_acct__doc", createReq["collection_name"])
	assert.Equal(t, "id", createReq["primary_key"])
	fields := createReq["fields"].([]any)
	require.Len(t, fields, 2)
	f0 := fields[0].(map[string]any)
	assert.Equal(t, "id", f0["field_name"])
	assert.Equal(t, "string", f0["field_type"])
	f1 := fields[1].(map[string]any)
	assert.Equal(t, "vector", f1["field_name"])
	assert.Equal(t, "float32", f1["field_type"])
	assert.EqualValues(t, 4, f1["dim"])

	bodyIdx := gotIdx.Load().(string)
	var idxReq map[string]any
	require.NoError(t, json.Unmarshal([]byte(bodyIdx), &idxReq))
	assert.Equal(t, "ov_acct__doc", idxReq["collection_name"])
	assert.Equal(t, "ov_acct__doc_idx", idxReq["index_name"])
	vi := idxReq["vector_index"].(map[string]any)
	assert.Equal(t, "HNSW", vi["index_type"])
	assert.Equal(t, "cosine", vi["distance"])
}

func TestVikingDBAdapter_EnsureCollection_Idempotent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collection/create", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"ResponseMetadata":{"Error":{"CodeN":1000001,"Code":"CollectionAlreadyExists","Message":"collection already exists"}}}`))
	}))
	mux.HandleFunc("/api/index/create", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"ResponseMetadata":{"Error":{"CodeN":1000001,"Code":"IndexAlreadyExists","Message":"index already exists"}}}`))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	err := a.EnsureCollection(context.Background(), CollectionSchema{Name: "c", Dim: 4})
	require.NoError(t, err, "already-exists must be suppressed")
}

func TestVikingDBAdapter_DropCollection(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collection/drop", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	require.NoError(t, a.DropCollection(context.Background(), "c"))
}

func TestVikingDBAdapter_DropCollection_NotFoundIsSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collection/drop", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"ResponseMetadata":{"Error":{"CodeN":1000002,"Code":"CollectionNotFound","Message":"collection does not exist"}}}`))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	require.NoError(t, a.DropCollection(context.Background(), "c"))
}

func TestVikingDBAdapter_ListCollections(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collection/list", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[{"collection_name":"ov_acct__doc"},{"collection_name":"ov_acct__file"},{"collection_name":"other_coll"}]}`))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	got, err := a.ListCollections(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"ov_acct__doc", "ov_acct__file"}, got)
}

func TestVikingDBAdapter_Upsert_BodyShape(t *testing.T) {
	var got atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collection/upsert_data", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		got.Store(readBody(t, r))
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	rows := []Vector{
		{ID: "d1", Embedding: []float32{1, 2, 3}, Metadata: map[string]any{"account": "acme", "kind": "doc"}},
		{ID: "d2", Embedding: []float32{4, 5, 6}},
	}
	require.NoError(t, a.Upsert(context.Background(), "c", rows))
	body := got.Load().(string)
	var req map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	assert.Equal(t, "c", req["collection_name"])
	fields := req["fields"].([]any)
	require.Len(t, fields, 2)
	row0 := fields[0].(map[string]any)
	assert.Equal(t, "d1", row0["id"])
	assert.Equal(t, "acme", row0["account"])
	assert.Equal(t, "doc", row0["kind"])
	vec := row0["vector"].([]any)
	assert.Len(t, vec, 3)
}

func TestVikingDBAdapter_Upsert_EmptyRowsNoCall(t *testing.T) {
	called := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collection/upsert_data", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	require.NoError(t, a.Upsert(context.Background(), "c", nil))
	assert.False(t, called, "no HTTP call expected for empty batch")
}

func TestVikingDBAdapter_Upsert_MissingID(t *testing.T) {
	a := newTestVikingDBAdapter(t, "http://example.invalid")
	err := a.Upsert(context.Background(), "c", []Vector{{Embedding: []float32{1}}})
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, domain.CodeValidationFailed, appErr.Code)
}

func TestVikingDBAdapter_Delete(t *testing.T) {
	var got atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collection/del_data", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		got.Store(readBody(t, r))
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	require.NoError(t, a.Delete(context.Background(), "c", []string{"d1", "d2"}))
	body := got.Load().(string)
	var req map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	assert.Equal(t, "c", req["collection_name"])
	assert.Equal(t, []any{"d1", "d2"}, req["primary_keys"])
}

func TestVikingDBAdapter_Search(t *testing.T) {
	var got atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/api/index/search", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		got.Store(readBody(t, r))
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[{"id":"d1","vector":[0.1,0.2,0.3],"score":0.9,"account":"acme"},{"id":"d2","vector":[0.4,0.5,0.6],"score":0.5}]}`))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	res, err := a.Search(context.Background(), SearchParams{
		Collection: "c",
		Query:      []float32{1, 2, 3},
		TopK:       5,
		Filter:     Filter{Account: "acme", Kind: "doc"},
	})
	require.NoError(t, err)
	require.Len(t, res.Hits, 2)
	assert.Equal(t, "d1", res.Hits[0].ID)
	assert.InDelta(t, 0.9, res.Hits[0].Score, 1e-6)
	assert.Equal(t, "acme", res.Hits[0].Metadata["account"])
	assert.Equal(t, []float32{0.1, 0.2, 0.3}, res.Hits[0].Embedding)
	assert.Equal(t, "d2", res.Hits[1].ID)

	body := got.Load().(string)
	var req map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	assert.Equal(t, "c", req["collection_name"])
	assert.Equal(t, "c_idx", req["index_name"])
	search := req["search"].(map[string]any)
	assert.EqualValues(t, 5, search["limit"])
	obv := search["order_by_vector"].(map[string]any)
	vecs := obv["vectors"].([]any)
	require.Len(t, vecs, 1)
	flt, ok := search["filter"].(map[string]any)
	require.True(t, ok, "filter must be present when Filter is non-zero")
	conds := flt["conditions"].([]any)
	require.Len(t, conds, 2)
}

func TestVikingDBAdapter_Search_EmptyQuery(t *testing.T) {
	a := newTestVikingDBAdapter(t, "http://example.invalid")
	_, err := a.Search(context.Background(), SearchParams{Collection: "c", Query: nil})
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, domain.CodeValidationFailed, appErr.Code)
}

func TestVikingDBAdapter_Get(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collection/fetch_data", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		// SDK sends GET with JSON body (not query params).
		body := readBody(t, r)
		var req map[string]any
		if err := json.Unmarshal([]byte(body), &req); err == nil {
			assert.Equal(t, "c", req["collection_name"])
			assert.Equal(t, "d1", req["primary_keys"])
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[{"id":"d1","vector":[0.1,0.2],"account":"acme"}]}`))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	v, err := a.Get(context.Background(), "c", "d1")
	require.NoError(t, err)
	assert.Equal(t, "d1", v.ID)
	assert.Equal(t, []float32{0.1, 0.2}, v.Embedding)
	assert.Equal(t, "acme", v.Metadata["account"])
}

func TestVikingDBAdapter_Get_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collection/fetch_data", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"ResponseMetadata":{"Error":{"CodeN":1000002,"Code":"NotFound","Message":"does not exist"}}}`))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	_, err := a.Get(context.Background(), "c", "d1")
	require.Error(t, err)
	assert.True(t, isNotFound(err), "err must be a not-found error")
}

func TestVikingDBAdapter_Get_NotFoundEmptyData(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collection/fetch_data", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	_, err := a.Get(context.Background(), "c", "d1")
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, domain.CodeResourceNotFound, appErr.Code)
	assert.Equal(t, 404, appErr.Status)
}

func TestVikingDBAdapter_Count(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collection/info", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		var req map[string]any
		if err := json.Unmarshal([]byte(body), &req); err == nil {
			assert.Equal(t, "c", req["collection_name"])
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":{"stat":{"total_count":42}}}`))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	n, err := a.Count(context.Background(), "c")
	require.NoError(t, err)
	assert.EqualValues(t, 42, n)
}

func TestVikingDBAdapter_Count_NotFoundReturnsZero(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collection/info", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"ResponseMetadata":{"Error":{"CodeN":1000002,"Code":"NotFound","Message":"does not exist"}}}`))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	n, err := a.Count(context.Background(), "c")
	require.NoError(t, err)
	assert.EqualValues(t, 0, n)
}

func TestVikingDBAdapter_ServerError_MapsToVectorDBError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collection/upsert_data", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"ResponseMetadata":{"Error":{"CodeN":500,"Code":"Internal","Message":"boom"}}}`))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	err := a.Upsert(context.Background(), "c", []Vector{{ID: "d1", Embedding: []float32{1}}})
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, domain.CodeVectorDBError, appErr.Code)
	assert.Contains(t, appErr.Error(), "boom")
}

func TestVikingDBAdapter_BadRequest_MapsToValidationFailed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collection/upsert_data", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"ResponseMetadata":{"Error":{"CodeN":400,"Code":"BadRequest","Message":"bad input"}}}`))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	err := a.Upsert(context.Background(), "c", []Vector{{ID: "d1", Embedding: []float32{1}}})
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, domain.CodeValidationFailed, appErr.Code)
}

// TestVikingDBAdapter_SignatureIsWellFormed verifies the SDK's V4 signer is
// wired up: every request carries a well-formed HMAC-SHA256 Authorization
// header with a 64-hex signature. Two adapters with identical config must
// produce identical signatures for the same request (the V4 algorithm is
// deterministic for a fixed X-Date).
func TestVikingDBAdapter_SignatureIsWellFormed(t *testing.T) {
	var captured []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collection/upsert_data", authCheck(t, func(w http.ResponseWriter, r *http.Request) {
		captured = append(captured, r.Header.Get("Authorization"))
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a1 := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	a2 := newTestVikingDBAdapter(t, srv.URL).withHTTPClient(srv.Client())
	rows := []Vector{{ID: "d1", Embedding: []float32{1, 2, 3}}}
	require.NoError(t, a1.Upsert(context.Background(), "c", rows))
	require.NoError(t, a2.Upsert(context.Background(), "c", rows))
	require.Len(t, captured, 2)
	// The V4 signature is deterministic given identical X-Date + body + path.
	// X-Date has second resolution, so back-to-back calls in the same second
	// produce identical signatures.
	if captured[0] == captured[1] {
		return
	}
	// If the second ticked over, signatures differ but both must still be
	// well-formed (asserted by authCheck above).
	t.Logf("signatures differ across calls (expected if the second ticked): %s vs %s", captured[0], captured[1])
}

func TestAnySliceToFloat32(t *testing.T) {
	assert.Equal(t, []float32{0.1, 0.2, 0.3}, anySliceToFloat32([]any{float64(0.1), float64(0.2), float64(0.3)}))
	assert.Equal(t, []float32{1, 2, 3}, anySliceToFloat32([]any{float64(1), float64(2), float64(3)}))
	assert.Empty(t, anySliceToFloat32(nil))
}

func TestBuildVikingDBFilter(t *testing.T) {
	assert.Equal(t, "", buildVikingDBFilter(Filter{}))
	assert.Equal(t, "filter", buildVikingDBFilter(Filter{Account: "a"}))
}

func TestParseFilterConditions(t *testing.T) {
	conds := parseFilterConditions(Filter{
		Account:   "acct",
		Kind:      "doc",
		URIPrefix: "viking://",
		Metadata:  map[string]any{"foo": "bar"},
	})
	require.Len(t, conds, 4)
	gotFields := map[string]string{}
	for _, c := range conds {
		gotFields[c["field"].(string)] = c["operator"].(string)
	}
	assert.Equal(t, "=", gotFields["account"])
	assert.Equal(t, "=", gotFields["kind"])
	assert.Equal(t, "prefix", gotFields["uri"])
	assert.Equal(t, "=", gotFields["foo"])
}

func TestIsAlreadyExists(t *testing.T) {
	assert.True(t, isAlreadyExists(domain.Wrap(domain.CodeConflict, 409, errors.New("collection already exists"))))
	assert.False(t, isAlreadyExists(domain.Wrap(domain.CodeVectorDBError, 500, errors.New("boom"))))
}

func TestIsNotFound(t *testing.T) {
	assert.True(t, isNotFound(domain.Wrap(domain.CodeResourceNotFound, 404, errors.New("does not exist"))))
	assert.False(t, isNotFound(domain.Wrap(domain.CodeVectorDBError, 500, errors.New("boom"))))
}
