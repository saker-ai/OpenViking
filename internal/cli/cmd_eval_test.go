package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/eval"
	"github.com/saker-ai/ctxhub/internal/models/vlm"
)

// evalStubGrader is a vlm.VLM used by the eval CLI tests to script
// both Chat (for answer generation and metric grading) and Embed (for
// AnswerRelevancy). It records every call so tests can assert on the
// request shape.
type evalStubGrader struct {
	chatResp   string
	embedResp  [][]float32
	chatCalls  int
	embedCalls int
}

func (g *evalStubGrader) Chat(ctx context.Context, req vlm.ChatRequest) (*vlm.ChatResponse, error) {
	g.chatCalls++
	return &vlm.ChatResponse{Content: g.chatResp}, nil
}

func (g *evalStubGrader) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	g.embedCalls++
	out := make([][]float32, len(texts))
	for i := range texts {
		if i < len(g.embedResp) {
			out[i] = g.embedResp[i]
		} else if len(g.embedResp) > 0 {
			out[i] = g.embedResp[0]
		} else {
			out[i] = []float32{1, 0, 0}
		}
	}
	return out, nil
}

// newEvalStubGrader returns a stub that responds to all metric prompts
// with "all good" verdicts so every metric scores 1.0. The Chat
// response is a JSON object the metric parsers accept for every
// metric (claims / verdicts / questions / sentences / relevant).
func newEvalStubGrader() *evalStubGrader {
	return &evalStubGrader{
		chatResp:  `{"claims": ["c1"], "verdicts": [{"claim": "c1", "verdict": 1, "context_index": 1}], "questions": ["q1"], "sentences": ["s1"], "relevant": ["s1"]}`,
		embedResp: [][]float32{{1, 0, 0}, {1, 0, 0}},
	}
}

// withEvalGraderStub swaps the eval grader constructor to return stub
// for the duration of the test. Returns the stub so the test can
// inspect call counts.
func withEvalGraderStub(t *testing.T) *evalStubGrader {
	t.Helper()
	t.Setenv("RAGAS_LLM_API_KEY", "test-key")
	t.Setenv("RAGAS_LLM_API_BASE", "https://example.invalid/v1")
	t.Setenv("RAGAS_LLM_MODEL", "test-model")
	stub := newEvalStubGrader()
	prev := newEvalGraderFn
	newEvalGraderFn = func() (eval.Grader, string, error) {
		return stub, "test-model", nil
	}
	t.Cleanup(func() { newEvalGraderFn = prev })
	return stub
}

func TestEvalCmd_RequiresDatasetFlag(t *testing.T) {
	t.Parallel()
	rt, _, _ := newTestRuntimeNoServer(t)
	cmd := silence(EvalCmd(rt))
	err := Execute(cmd, []string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--dataset is required")
}

func TestEvalCmd_RequiresGraderEnvVars(t *testing.T) {
	// Not parallel: t.Setenv is process-wide.
	t.Setenv("RAGAS_LLM_API_KEY", "")
	t.Setenv("RAGAS_LLM_API_BASE", "")
	t.Setenv("RAGAS_LLM_MODEL", "")
	tmp := writeDataset(t, "test-model", `{"question":"q","answer":"a"}`+"\n")
	rt, _, _ := newTestRuntimeNoServer(t)
	cmd := silence(EvalCmd(rt))
	err := Execute(cmd, []string{"--dataset", tmp})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RAGAS_LLM_API_KEY")
}

func TestEvalCmd_HappyPath(t *testing.T) {
	withEvalGraderStub(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/search" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[{"uri":"u","snippet":"ctx1","score":0.9}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	tmp := writeDataset(t, "ds", `{"question":"q1","answer":"a1"}`+"\n")
	rt, out, _ := newTestRuntime(t, srv)
	cmd := silence(EvalCmd(rt))
	require.NoError(t, Execute(cmd, []string{"--dataset", tmp, "--top-k", "1"}))
	assert.Contains(t, out.String(), "dataset:")
	assert.Contains(t, out.String(), "faithfulness")
}

func TestEvalCmd_JSONOutput(t *testing.T) {
	withEvalGraderStub(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/search" {
			_, _ = w.Write([]byte(`{"items":[{"snippet":"ctx1"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	tmp := writeDataset(t, "ds", `{"question":"q1","answer":"a1"}`+"\n")
	rt, out, _ := newTestRuntime(t, srv)
	cmd := silence(EvalCmd(rt))
	require.NoError(t, Execute(cmd, []string{"--dataset", tmp, "--json"}))
	var report map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	assert.Equal(t, "ds", report["dataset_name"])
	assert.NotEmpty(t, report["results"])
}

func TestEvalCmd_PersistsToRecorder(t *testing.T) {
	withEvalGraderStub(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/search" {
			_, _ = w.Write([]byte(`{"items":[{"snippet":"ctx1"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	dir := t.TempDir()
	recPath := filepath.Join(dir, "eval.db")
	tmp := writeDataset(t, "ds", `{"question":"q1","answer":"a1"}`+"\n")
	rt, out, _ := newTestRuntime(t, srv)
	cmd := silence(EvalCmd(rt))
	require.NoError(t, Execute(cmd, []string{"--dataset", tmp, "--recorder", recPath}))
	assert.Contains(t, out.String(), "saved run")
	_, err := os.Stat(recPath)
	require.NoError(t, err)
}

func TestDatasetName(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"/tmp/foo.jsonl", "foo"},
		{"foo.jsonl", "foo"},
		{"/tmp/foo", "foo"},
		{"", "dataset"},
		{"/tmp/", "dataset"}, // trailing slash -> empty basename
		{"foo.bar.jsonl", "foo.bar"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, datasetName(tc.in))
		})
	}
}

// writeDataset writes content to a temp .jsonl file and returns its path.
// name controls the basename (without extension) so tests can assert on
// the dataset_name field in JSON output.
func writeDataset(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name+".jsonl")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}
