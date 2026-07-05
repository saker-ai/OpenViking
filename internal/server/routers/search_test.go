package routers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/retrieve"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// newTestRouterAll extends newTestRouter with the four routers under test
// (search/code/relations/skills). It reuses the existing helper so the
// shared errorMiddleware / identity wiring stays in one place.
func newTestRouterAll(t *testing.T, deps *Deps) *gin.Engine {
	t.Helper()
	r := newTestRouter(t, deps)
	api := r.Group("/api/v1")
	RegisterSearch(api, deps)
	RegisterCode(api, deps)
	RegisterRelations(api, deps)
	RegisterSkills(api, deps)
	return r
}

// stubRetriever is a retrieve.Retriever test double. It returns the canned
// response and error, and records the last request so tests can assert
// request mapping.
type stubRetriever struct {
	resp *retrieve.RetrieveResponse
	err  error
	last retrieve.RetrieveRequest
}

func (s *stubRetriever) Retrieve(_ context.Context, req retrieve.RetrieveRequest) (*retrieve.RetrieveResponse, error) {
	s.last = req
	if s.err != nil {
		return nil, s.err
	}
	if s.resp != nil {
		return s.resp, nil
	}
	return &retrieve.RetrieveResponse{Query: req.Query, Documents: []retrieve.Document{}}, nil
}

func TestSearch_QueryHappy(t *testing.T) {
	deps := newTestDeps(t)
	deps.Retrieve = &stubRetriever{
		resp: &retrieve.RetrieveResponse{
			Query: "hello",
			Documents: []retrieve.Document{
				{URI: "/accounts/acct/resources/note.md", Content: "hello world", Score: 0.9, Level: retrieve.Level2Chunk},
			},
		},
	}
	r := newTestRouterAll(t, deps)

	body := `{"query":"hello","top_k":5}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/search", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Result *retrieve.RetrieveResponse `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Result)
	assert.Equal(t, "hello", resp.Result.Query)
	require.Len(t, resp.Result.Documents, 1)
	assert.Equal(t, "/accounts/acct/resources/note.md", resp.Result.Documents[0].URI)
}

func TestSearch_QueryValidation(t *testing.T) {
	deps := newTestDeps(t)
	deps.Retrieve = &stubRetriever{}
	r := newTestRouterAll(t, deps)

	// Empty query should fail validation.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/search",
		bytes.NewBufferString(`{"query":"  "}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestSearch_QueryDefaultsTopK(t *testing.T) {
	deps := newTestDeps(t)
	stub := &stubRetriever{}
	deps.Retrieve = stub
	r := newTestRouterAll(t, deps)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/search",
		bytes.NewBufferString(`{"query":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "acct", stub.last.Account)
	assert.Equal(t, 10, stub.last.TopK, "TopK should default to 10 when unset")
}

func TestSearch_ExplainSurfacesIntent(t *testing.T) {
	deps := newTestDeps(t)
	deps.Retrieve = &stubRetriever{
		resp: &retrieve.RetrieveResponse{
			Query:  "explain me",
			Intent: &retrieve.Intent{OriginalQuery: "explain me", RewrittenQuery: "explain me"},
		},
	}
	r := newTestRouterAll(t, deps)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/search/explain",
		bytes.NewBufferString(`{"query":"explain me"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Explain bool                       `json:"explain"`
		Result  *retrieve.RetrieveResponse `json:"result"`
		Intent  *retrieve.Intent           `json:"intent"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Explain)
	require.NotNil(t, resp.Intent)
	assert.Equal(t, "explain me", resp.Intent.OriginalQuery)
}

func TestSearch_Suggestions(t *testing.T) {
	deps := newTestDeps(t)
	ctx := reqContext(t, nil, "acct")
	// Pre-populate the resources root so suggestions has something to return.
	require.NoError(t, deps.RAGFS.Mkdir(ctx, "/accounts/acct/resources", 0o755))
	require.NoError(t, deps.RAGFS.Write(ctx, "/accounts/acct/resources/alpha.md",
		bytes.NewBufferString("a"), 0o644))
	require.NoError(t, deps.RAGFS.Write(ctx, "/accounts/acct/resources/alpine.md",
		bytes.NewBufferString("a"), 0o644))
	require.NoError(t, deps.RAGFS.Write(ctx, "/accounts/acct/resources/beta.md",
		bytes.NewBufferString("b"), 0o644))
	r := newTestRouterAll(t, deps)

	// ?q=al should match alpha + alpine.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/suggestions?q=al", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Suggestions []string `json:"suggestions"`
		Query       string   `json:"query"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "al", resp.Query)
	assert.ElementsMatch(t, []string{"alpha.md", "alpine.md"}, resp.Suggestions)
}

func TestSearch_SuggestionsMissingRoot(t *testing.T) {
	deps := newTestDeps(t)
	deps.Retrieve = &stubRetriever{}
	r := newTestRouterAll(t, deps)

	// No files created -> resources root missing -> empty suggestions.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/suggestions?q=x", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Suggestions []string `json:"suggestions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.Suggestions)
}

func TestSearch_NilDepsReturns501(t *testing.T) {
	r := newTestRouterAll(t, &Deps{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/search",
		bytes.NewBufferString(`{"query":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

func TestSearch_SuggestionsNilDepsReturns501(t *testing.T) {
	r := newTestRouterAll(t, &Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/suggestions?q=x", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
