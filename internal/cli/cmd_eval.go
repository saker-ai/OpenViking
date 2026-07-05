package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/saker-ai/ctxhub/internal/eval"
	"github.com/saker-ai/ctxhub/internal/models/vlm"
)

// EvalCmd builds `ov eval --dataset PATH` — runs the Go eval framework
// against the configured OpenViking server. For each case in the JSONL
// dataset the command:
//  1. Retrieves contexts from the server via POST /api/v1/search.
//  2. Generates an answer from the contexts using the configured LLM.
//  3. Scores the (query, ground_truth, answer, contexts) tuple with
//     the 5 built-in metrics: Faithfulness, AnswerRelevancy,
//     ContextualPrecision, ContextualRecall, ContextualRelevance.
//
// The LLM grader is configured from the same env vars as the Python
// RAGAS evaluator (RAGAS_LLM_API_KEY / RAGAS_LLM_API_BASE /
// RAGAS_LLM_MODEL) so a single config transfers between runtimes.
//
// Results are printed as a table by default; --json emits the full
// EvalReport. --recorder persists the run to SQLite for later
// comparison.
func EvalCmd(rt *Runtime) *cobra.Command {
	var (
		datasetPath string
		topK        int
		recorderPath string
		jsonOut     bool
		concurrency int
	)
	cmd := &cobra.Command{
		Use:   "eval --dataset PATH",
		Short: "Run RAG evaluation against the configured server",
		Long:  `Eval runs the Go eval framework (internal/eval) against the configured OpenViking server.`,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if datasetPath == "" {
				return fmt.Errorf("eval: --dataset is required")
			}
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			grader, model, err := newEvalGrader()
			if err != nil {
				return err
			}
			cases, err := eval.LoadDataset(datasetPath, rt.Err)
			if err != nil {
				return err
			}
			if len(cases) == 0 {
				return fmt.Errorf("eval: dataset %s has no valid cases", datasetPath)
			}
			runner := &eval.Runner{
				Metrics:     eval.DefaultMetrics(grader, model),
				Func:        newEvalFuncUnderTest(rt, grader, model, topK),
				Concurrency: concurrency,
			}
			report, err := runner.Run(cmd.Context(), datasetName(datasetPath), cases)
			if err != nil {
				return err
			}
			if recorderPath != "" {
				rec, err := eval.NewRecorder(recorderPath)
				if err != nil {
					return err
				}
				defer rec.Close()
				runID := eval.NewRunID()
				if err := rec.SaveRun(cmd.Context(), runID, report); err != nil {
					return err
				}
				fmt.Fprintf(rt.Out, "saved run %s to %s\n", runID, recorderPath)
			}
			if jsonOut {
				enc := json.NewEncoder(rt.Out)
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}
			printEvalReport(rt, report)
			return nil
		},
	}
	cmd.Flags().StringVar(&datasetPath, "dataset", "", "path to JSONL dataset (each line: {\"question\": ..., \"answer\": ...})")
	cmd.Flags().IntVarP(&topK, "top-k", "k", 5, "number of contexts to retrieve per query")
	cmd.Flags().StringVar(&recorderPath, "recorder", "", "path to SQLite file for persisting the run (empty = no persistence)")
	cmd.Flags().IntVarP(&concurrency, "concurrency", "c", 8, "max concurrent cases")
	cmd.Flags().BoolVarP(&jsonOut, "json", "j", false, "raw JSON output")
	return cmd
}

// newEvalGraderFn is the swappable constructor for the eval grader.
// Production code leaves this nil and the default newEvalGrader reads
// RAGAS_LLM_* env vars. Tests override it to inject a stub VLM so no
// network or real model is required.
var newEvalGraderFn func() (eval.Grader, string, error)

// newEvalGrader constructs a vlm.VLM from the RAGAS_LLM_* env vars.
// Returns an error when the env vars are missing so misconfiguration
// fails loudly rather than silently using a stub.
func newEvalGrader() (eval.Grader, string, error) {
	if newEvalGraderFn != nil {
		return newEvalGraderFn()
	}
	apiKey := os.Getenv("RAGAS_LLM_API_KEY")
	apiBase := os.Getenv("RAGAS_LLM_API_BASE")
	model := os.Getenv("RAGAS_LLM_MODEL")
	if apiKey == "" || apiBase == "" {
		return nil, "", fmt.Errorf("eval: RAGAS_LLM_API_KEY and RAGAS_LLM_API_BASE must be set (RAGAS_LLM_MODEL optional)")
	}
	if model == "" {
		model = "gpt-4o-mini"
	}
	client := vlm.NewOpenAI(apiBase, apiKey, model, nil)
	return client, model, nil
}

// newEvalFuncUnderTest builds the function under test: retrieve
// contexts from the server, then generate an answer via the LLM.
// The grader is reused for answer generation so a single config
// drives both retrieval-augmented generation and metric grading.
func newEvalFuncUnderTest(rt *Runtime, g eval.Grader, model string, topK int) eval.FuncUnderTest {
	return func(ctx context.Context, query string) (string, []string, error) {
		contexts, err := evalSearch(ctx, rt.Client, query, topK)
		if err != nil {
			return "", nil, err
		}
		if len(contexts) == 0 {
			return "", nil, nil
		}
		answer, err := evalGenerate(ctx, g, model, query, contexts)
		if err != nil {
			return "", nil, err
		}
		return answer, contexts, nil
	}
}

// evalSearch calls POST /api/v1/search on the server and extracts the
// snippet/content text from each result item. The server returns
// {items: [{uri, score, snippet, ...}]}.
func evalSearch(ctx context.Context, c *Client, query string, topK int) ([]string, error) {
	body := map[string]any{"query": query, "limit": topK}
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := c.PostJSON(ctx, "/api/v1/search", body, &resp); err != nil {
		return nil, fmt.Errorf("eval: search: %w", err)
	}
	out := make([]string, 0, len(resp.Items))
	for _, it := range resp.Items {
		// The search endpoint returns one of several text fields
		// depending on the resource type (snippet for code,
		// content/overview/abstract for documents). Accept any of
		// them so the eval command works across server versions.
		s, _ := it["snippet"].(string)
		if s == "" {
			s, _ = it["content"].(string)
		}
		if s == "" {
			s, _ = it["overview"].(string)
		}
		if s == "" {
			s, _ = it["abstract"].(string)
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// evalGenerate asks the LLM to answer the query using the retrieved
// contexts. The prompt mirrors the Python RAGQueryPipeline.query()
// template so answers are comparable across runtimes.
func evalGenerate(ctx context.Context, g eval.Grader, model string, query string, contexts []string) (string, error) {
	const sys = "You are a careful assistant. Answer the question using only the provided context. " +
		"If the context does not contain enough information, say \"I cannot answer this question based on the provided context.\""
	var sb strings.Builder
	sb.WriteString("Context:\n")
	for i, c := range contexts {
		fmt.Fprintf(&sb, "[%d] %s\n\n", i+1, c)
	}
	sb.WriteString("Question:\n")
	sb.WriteString(query)
	// g satisfies eval.Grader (Chat + Embed); we only need Chat here.
	type chatter interface {
		Chat(ctx context.Context, req vlm.ChatRequest) (*vlm.ChatResponse, error)
	}
	ch, ok := g.(chatter)
	if !ok {
		return "", fmt.Errorf("eval: grader does not expose Chat")
	}
	req := vlm.ChatRequest{
		Model: model,
		Messages: []vlm.Message{
			{Role: vlm.RoleSystem, Content: sys},
			{Role: vlm.RoleUser, Content: sb.String()},
		},
		Temperature: 0,
	}
	resp, err := ch.Chat(ctx, req)
	if err != nil {
		return "", fmt.Errorf("eval: generate: %w", err)
	}
	return resp.Content, nil
}

// printEvalReport renders the report as a table: one row per metric
// with its mean score, followed by per-case rows.
func printEvalReport(rt *Runtime, rep *eval.EvalReport) {
	fmt.Fprintf(rt.Out, "dataset: %s  cases: %d  started: %s  elapsed: %s\n",
		rep.DatasetName, rep.SampleCount,
		rep.StartedAt.Local().Format("2006-01-02 15:04:05"),
		rep.CompletedAt.Sub(rep.StartedAt).Round(time.Millisecond))
	headers := []string{"METRIC", "MEAN"}
	rows := make([][]string, 0, len(rep.MeanScores))
	for name, score := range rep.MeanScores {
		rows = append(rows, []string{name, fmt.Sprintf("%.3f", score)})
	}
	rt.PrintTable(headers, rows)
}

// datasetName derives a short dataset name from the path (basename
// without extension). Used as the report's DatasetName so recorded
// runs are easy to identify.
func datasetName(path string) string {
	base := path
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	if base == "" {
		return "dataset"
	}
	return base
}
